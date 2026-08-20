//go:build mcp2026artifact

package mcp

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"neuralmail/internal/auth"
	"neuralmail/internal/store"
)

const immutableSDK02Python = `
import asyncio
import sys

from nerve_email import NerveClient, __version__


async def main():
    if __version__ != "0.2.0":
        raise SystemExit(f"unexpected SDK version: {__version__}")
    async with NerveClient(
        base_url=sys.argv[1],
        rest_base_url=sys.argv[1],
        api_key="immutable-sdk-0.2-proof",
        timeout=15,
        max_retries=0,
    ) as client:
        if not await client.health_check():
            raise SystemExit("SDK 0.2 health check failed")
        tools = await client.list_tools()
        if not tools:
            raise SystemExit("SDK 0.2 legacy tools/list was empty")
    print("immutable-sdk-0.2-ok")


asyncio.run(main())
`

func TestImmutableSDK02AndNativeMCP2026ShareEndpoint(t *testing.T) {
	python := os.Getenv("NERVE_IMMUTABLE_SDK_0_2_PYTHON")
	if python == "" {
		t.Fatal("NERVE_IMMUTABLE_SDK_0_2_PYTHON must name the verified SDK 0.2 environment")
	}

	cfg := hostedRouterConfig()
	now := time.Unix(1_800_000_000, 0).UTC()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate M2M access-token key: %v", err)
	}
	const keyID = "dual-profile-access-key"
	encodedModulus := base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes())
	encodedExponent := base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1})
	keys, err := auth.ParseRSAPublicJWKS([]byte(fmt.Sprintf(
		`{"keys":[{"kty":"RSA","kid":%q,"use":"sig","alg":"PS256","n":%q,"e":%q}]}`,
		keyID, encodedModulus, encodedExponent,
	)))
	if err != nil {
		t.Fatalf("parse M2M access-token key: %v", err)
	}
	const (
		issuer   = "https://auth.nerve.email"
		audience = "https://api.nerve.email/mcp"
		clientID = "dual-profile-agent"
		orgID    = "dual-profile-org"
		tokenID  = "dual-profile-token"
	)
	expiresAt := now.Add(15 * time.Minute)
	m2mToken := jwt.NewWithClaims(jwt.SigningMethodPS256, jwt.MapClaims{
		"iss": issuer, "aud": audience, "exp": expiresAt.Unix(), "nbf": now.Unix(), "iat": now.Unix(),
		"jti": tokenID, "sub": clientID, "client_id": clientID, "generation": 1,
		"token_use": string(auth.PrincipalM2MOrg), "org_id": orgID, "scope": "nerve:email.read",
	})
	m2mToken.Header["kid"] = keyID
	signedM2MToken, err := m2mToken.SignedString(privateKey)
	if err != nil {
		t.Fatalf("sign M2M access token: %v", err)
	}

	authService := auth.NewService(cfg, nil)
	authService.Now = func() time.Time { return now }
	authService.M2M = &auth.M2MTokenVerifier{
		Issuer: issuer, Audience: audience, Keys: keys, Now: func() time.Time { return now },
	}
	legacyKeyHash := sha256.Sum256([]byte("immutable-sdk-0.2-proof"))
	authService.LookupCloudKey = func(_ context.Context, keyHash string) (store.CloudAPIKey, error) {
		if keyHash != hex.EncodeToString(legacyKeyHash[:]) {
			return store.CloudAPIKey{}, sql.ErrNoRows
		}
		return store.CloudAPIKey{
			ID: "immutable-sdk-0.2", OrgID: "legacy-org", Scopes: []string{"nerve:email.read"},
		}, nil
	}
	authService.LookupServiceToken = func(_ context.Context, requestedTokenID string) (store.ServiceToken, error) {
		if requestedTokenID != tokenID {
			return store.ServiceToken{}, sql.ErrNoRows
		}
		return store.ServiceToken{
			ID: tokenID, OrgID: orgID, Actor: "oauth_client:" + clientID + ":g:1",
			Scopes: []string{"nerve:email.read"}, ExpiresAt: expiresAt,
		}, nil
	}
	runtime := NewServer(cfg, nil, authService, nil)
	router := NewRouter(cfg, authService, NewLegacyHandler(runtime), NewSDKHandler(runtime, true))
	barrier := newDualProfileBarrier(router)
	mux := http.NewServeMux()
	mux.Handle("/mcp", barrier)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	hosted := httptest.NewServer(mux)
	defer hosted.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	legacy := exec.CommandContext(ctx, python, "-c", immutableSDK02Python, hosted.URL)
	legacyOutput := &strings.Builder{}
	legacy.Stdout, legacy.Stderr = legacyOutput, legacyOutput
	if err := legacy.Start(); err != nil {
		t.Fatalf("start immutable SDK 0.2 client: %v", err)
	}

	client := newModernSDKTestClient()
	session, err := client.Connect(ctx, &sdkmcp.StreamableClientTransport{
		Endpoint: hosted.URL + "/mcp",
		HTTPClient: &http.Client{Transport: authenticatedOriginRoundTripper{
			base: http.DefaultTransport, origin: "https://agent.example", bearer: signedM2MToken,
		}},
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		_ = legacy.Process.Kill()
		_ = legacy.Wait()
		t.Fatalf("connect native MCP 2026 client: %v; SDK 0.2 output=%s", err, legacyOutput.String())
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("native MCP 2026 tools/list: %v", err)
	}
	if listed.CacheScope != "private" || len(listed.Tools) == 0 {
		t.Fatalf("native MCP 2026 catalog is not private/non-empty: %#v", listed)
	}
	if err := legacy.Wait(); err != nil {
		t.Fatalf("immutable SDK 0.2 client: %v; output=%s", err, legacyOutput.String())
	}
	if !strings.Contains(legacyOutput.String(), "immutable-sdk-0.2-ok") {
		t.Fatalf("immutable SDK 0.2 proof marker missing: %s", legacyOutput.String())
	}
	if !barrier.sawBoth() {
		t.Fatal("both protocol profiles did not reach the shared /mcp endpoint concurrently")
	}
}

type authenticatedOriginRoundTripper struct {
	base   http.RoundTripper
	origin string
	bearer string
}

func (transport authenticatedOriginRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Origin", transport.origin)
	clone.Header.Set("Authorization", "Bearer "+transport.bearer)
	return transport.base.RoundTrip(clone)
}

type dualProfileBarrier struct {
	next  http.Handler
	ready chan struct{}
	once  sync.Once
	mu    sync.Mutex
	seen  map[string]bool
}

func newDualProfileBarrier(next http.Handler) *dualProfileBarrier {
	return &dualProfileBarrier{next: next, ready: make(chan struct{}), seen: make(map[string]bool)}
}

func (barrier *dualProfileBarrier) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	version := request.Header.Get("MCP-Protocol-Version")
	barrier.mu.Lock()
	barrier.seen[version] = true
	if barrier.seen[LegacyProtocolVersion] && barrier.seen[ModernProtocolVersion] {
		barrier.once.Do(func() { close(barrier.ready) })
	}
	barrier.mu.Unlock()

	select {
	case <-barrier.ready:
		barrier.next.ServeHTTP(w, request)
	case <-request.Context().Done():
		http.Error(w, "dual-profile rendezvous canceled", http.StatusRequestTimeout)
	}
}

func (barrier *dualProfileBarrier) sawBoth() bool {
	barrier.mu.Lock()
	defer barrier.mu.Unlock()
	return barrier.seen[LegacyProtocolVersion] && barrier.seen[ModernProtocolVersion]
}
