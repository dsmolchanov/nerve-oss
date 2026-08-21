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
	"encoding/json"
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

const immutableSDK02GoldenPython = `
import asyncio
import sys

import httpx

from nerve_email import NerveClient, __version__
from nerve_email.exceptions import (
    NerveError,
    NerveQuotaError,
    NerveRateLimitError,
    NerveSubscriptionError,
)


def client(base_url):
    return NerveClient(
        base_url=base_url,
        rest_base_url=base_url,
        api_key="immutable-sdk-0.2-proof",
        timeout=15,
        max_retries=0,
    )


async def expect_error(
    base_url,
    tool,
    expected_type,
    expected_code,
    sdk_retry_after=None,
    wire_retry_after=None,
):
    observed_errors = []

    async def observe(response):
        await response.aread()
        payload = response.json()
        if payload.get("error"):
            observed_errors.append(payload["error"])

    async with client(base_url) as sdk:
        sdk._http = httpx.AsyncClient(
            base_url=base_url,
            headers={
                "Content-Type": "application/json",
                "MCP-Protocol-Version": "2025-11-25",
                "X-Nerve-Cloud-Key": "immutable-sdk-0.2-proof",
            },
            timeout=15,
            event_hooks={"response": [observe]},
        )
        try:
            await sdk.execute_tool(tool, {})
        except Exception as exc:
            if type(exc) is not expected_type:
                raise SystemExit(f"{tool} raised {type(exc).__name__}, want exact {expected_type.__name__}") from exc
            if exc.code != expected_code:
                raise SystemExit(f"{tool} code={exc.code}, want {expected_code}")
            if sdk_retry_after is not None and exc.retry_after != sdk_retry_after:
                raise SystemExit(f"{tool} SDK retry_after={exc.retry_after}, want {sdk_retry_after}")
        else:
            raise SystemExit(f"{tool} did not raise {expected_type.__name__}")
    if len(observed_errors) != 1:
        raise SystemExit(f"{tool} observed wire errors={observed_errors}, want exactly one")
    observed_retry = observed_errors[0].get("data", {}).get("retry_after_seconds")
    if observed_retry != wire_retry_after:
        raise SystemExit(f"{tool} wire retry_after={observed_retry}, want {wire_retry_after}")


async def main():
    if __version__ != "0.2.0":
        raise SystemExit(f"unexpected SDK version: {__version__}")
    base_url = sys.argv[1]
    async with client(base_url) as sdk:
        tools = await sdk.list_tools()
        names = [tool["name"] for tool in tools]
        expected_names = [
            "list_threads", "get_thread", "search_inbox", "triage_message",
            "extract_to_schema", "draft_reply_with_policy", "send_reply", "compose_email",
        ]
        if names != expected_names:
            raise SystemExit(f"legacy tools/list={names}, want {expected_names}")
        resources = await sdk._rpc("resources/list", {})
        expected_resources = {
            "resources": [{"description": "List inbox IDs", "uri": "email://inboxes"}],
        }
        if resources != expected_resources:
            raise SystemExit(f"legacy resources/list={resources}, want {expected_resources}")

    await expect_error(base_url, "fixture_quota", NerveQuotaError, -32040)
    await expect_error(base_url, "fixture_subscription", NerveSubscriptionError, -32041)
    await expect_error(
        base_url, "fixture_rate", NerveRateLimitError, -32042,
        sdk_retry_after=12, wire_retry_after=12,
    )
    await expect_error(
        base_url, "fixture_idempotency", NerveError, -32043, wire_retry_after=3,
    )
    print("immutable-sdk-0.2-golden-ok")


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
	clientKeyThumbprint := sha256.Sum256([]byte("dual-profile-client-key"))
	clientKeyID := base64.RawURLEncoding.EncodeToString(clientKeyThumbprint[:])
	m2mToken := jwt.NewWithClaims(jwt.SigningMethodPS256, jwt.MapClaims{
		"iss": issuer, "aud": audience, "exp": expiresAt.Unix(), "nbf": now.Unix(), "iat": now.Unix(),
		"jti": tokenID, "sub": clientID, "client_id": clientID, "client_kid": clientKeyID, "generation": 1,
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
	proveImmutableSDK02GoldenFixtures(t, python)
}

func proveImmutableSDK02GoldenFixtures(t *testing.T, python string) {
	t.Helper()
	fixture := &sdk02GoldenFixtureServer{seen: make(map[string]int)}
	hosted := httptest.NewServer(fixture)
	defer hosted.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-c", immutableSDK02GoldenPython, hosted.URL)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("immutable SDK 0.2 golden fixtures: %v; output=%s", err, output)
	}
	if !strings.Contains(string(output), "immutable-sdk-0.2-golden-ok") {
		t.Fatalf("immutable SDK 0.2 golden marker missing: %s", output)
	}
	fixture.requireSeen(t, map[string]int{
		"initialize":           5,
		"tools/list":           1,
		"resources/list":       1,
		"fixture_quota":        1,
		"fixture_subscription": 1,
		"fixture_rate":         1,
		"fixture_idempotency":  1,
	})
}

type sdk02GoldenFixtureServer struct {
	mu   sync.Mutex
	seen map[string]int
}

func (fixture *sdk02GoldenFixtureServer) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/mcp" || request.Method != http.MethodPost ||
		request.Header.Get("MCP-Protocol-Version") != LegacyProtocolVersion ||
		request.Header.Get("X-Nerve-Cloud-Key") != "immutable-sdk-0.2-proof" {
		http.Error(w, "unexpected immutable SDK request boundary", http.StatusBadRequest)
		return
	}
	var wire struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.NewDecoder(request.Body).Decode(&wire); err != nil {
		http.Error(w, "invalid JSON-RPC request", http.StatusBadRequest)
		return
	}

	key := wire.Method
	response := ""
	switch wire.Method {
	case "initialize":
		if wire.ID != 1 {
			http.Error(w, "initialize fixture requires id 1", http.StatusBadRequest)
			return
		}
		w.Header().Set("MCP-Session-Id", "frozen-session")
		response = legacyInitializeWireGolden
	case "tools/list":
		if wire.ID != 2 {
			http.Error(w, "tools/list fixture requires id 2", http.StatusBadRequest)
			return
		}
		response = legacyToolsListWireGolden
	case "resources/list":
		if wire.ID != 3 {
			http.Error(w, "resources/list fixture requires id 3", http.StatusBadRequest)
			return
		}
		response = legacyResourcesListWireGolden
	case "tools/call":
		if wire.ID != 2 {
			http.Error(w, "error fixture requires id 2", http.StatusBadRequest)
			return
		}
		key = wire.Params.Name
		response = map[string]string{
			"fixture_quota":        legacyQuotaErrorWireGolden,
			"fixture_subscription": legacySubscriptionErrorWireGolden,
			"fixture_rate":         legacyRateErrorWireGolden,
			"fixture_idempotency":  legacyIdempotencyErrorWireGolden,
		}[key]
	}
	if response == "" {
		http.Error(w, "unexpected immutable SDK JSON-RPC request", http.StatusBadRequest)
		return
	}
	if wire.Method != "initialize" && request.Header.Get("MCP-Session-Id") != "frozen-session" {
		http.Error(w, "missing frozen session", http.StatusBadRequest)
		return
	}
	fixture.mu.Lock()
	fixture.seen[key]++
	fixture.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(response))
}

func (fixture *sdk02GoldenFixtureServer) requireSeen(t *testing.T, want map[string]int) {
	t.Helper()
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.seen) != len(want) {
		t.Fatalf("immutable SDK 0.2 fixture calls=%v want=%v", fixture.seen, want)
	}
	for key, count := range want {
		if fixture.seen[key] != count {
			t.Fatalf("immutable SDK 0.2 fixture %s calls=%d want=%d; all=%v", key, fixture.seen[key], count, fixture.seen)
		}
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
