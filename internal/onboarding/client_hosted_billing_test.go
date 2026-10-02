package onboarding

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"neuralmail/internal/mcp"
)

func TestClientDelegatesHostedUpgradeWithExactAuthority(t *testing.T) {
	now := time.Unix(1_723_000_000, 0).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != hostedUpgradeDelegationPath {
			t.Fatalf("hosted upgrade target=%s %s", request.Method, request.URL.Path)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Principal delegationPrincipal     `json:"principal"`
			Input     mcp.BillingUpgradeInput `json:"input"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Principal.ClientID != "client-1" || envelope.Principal.OrgID != "org-1" ||
			envelope.Principal.Generation != 7 || envelope.Principal.TokenID != "token-1" ||
			envelope.Input.IdempotencyKey != "upgrade-once" {
			t.Fatalf("hosted delegation=%+v", envelope)
		}
		bodyHash := sha256.Sum256(body)
		canonical := strings.Join([]string{"runtime-current", "nonce-1", "1723000000",
			http.MethodPost, hostedUpgradeDelegationPath, hex.EncodeToString(bodyHash[:])}, "\n")
		mac := hmac.New(sha256.New, []byte("delegation-secret"))
		_, _ = mac.Write([]byte(canonical))
		if request.Header.Get(delegationSignatureHeader) != hex.EncodeToString(mac.Sum(nil)) ||
			request.Header.Get("Authorization") != "Bearer original-token" {
			t.Fatalf("invalid hosted delegation headers: %v", request.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"result":{"resultType":"complete","state":"awaiting_owner",` +
			`"offer_id":"starter_2026_09_v2","upgrade_url":"https://nerve.example/billing/upgrade?intent=11111111-1111-4111-8111-111111111111"}}`))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client(), now)
	result, err := client.Upgrade(context.Background(), testBillingCaller(),
		mcp.BillingUpgradeInput{IdempotencyKey: "upgrade-once"})
	if err != nil || result.State != "awaiting_owner" || result.OfferID != "starter_2026_09_v2" {
		t.Fatalf("hosted result=%+v err=%v", result, err)
	}
}

func TestClientHostedUpgradeRejectsMalformedProviderEnvelope(t *testing.T) {
	for _, body := range []string{
		`{"result":{"resultType":"complete","state":"awaiting_owner","offer_id":"starter_2026_09_v2"}}`,
		`{"result":{"resultType":"complete","state":"awaiting_owner","offer_id":"starter_2026_09_v2","upgrade_url":"x","customer_id":"cus_other"}}`,
		`{"result":{"resultType":"complete","state":"awaiting_owner","offer_id":"starter_2026_09_v2","upgrade_url":"x","upgrade_url":"y"}}`,
		`{"result":{"resultType":"complete","state":"active","offer_id":"starter_2026_09_v2","upgrade_url":"x"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, server.Client(), time.Unix(1_723_000_000, 0).UTC())
			_, err := client.Upgrade(context.Background(), testBillingCaller(),
				mcp.BillingUpgradeInput{IdempotencyKey: "upgrade-once"})
			var business *mcp.BillingBusinessError
			if !errors.As(err, &business) || business.Code != mcp.BillingErrorTemporarilyUnavailable {
				t.Fatalf("malformed response accepted: err=%v", err)
			}
		})
	}
}
