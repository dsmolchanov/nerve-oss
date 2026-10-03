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

func TestClientDelegatesHostedStatusWithOriginalBearerAndTuple(t *testing.T) {
	now := time.Unix(1_723_000_000, 0).UTC()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != hostedStatusDelegationPath {
			t.Fatalf("hosted status target=%s %s", request.Method, request.URL.Path)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var envelope struct {
			Principal delegationPrincipal `json:"principal"`
			Input     map[string]any      `json:"input"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Input) != 0 ||
			envelope.Principal.ClientID != "client-1" || envelope.Principal.OrgID != "org-1" ||
			envelope.Principal.Generation != 7 || envelope.Principal.TokenID != "token-1" {
			t.Fatalf("hosted status delegation=%+v err=%v", envelope, err)
		}
		bodyHash := sha256.Sum256(body)
		canonical := strings.Join([]string{"runtime-current", "nonce-1", "1723000000",
			http.MethodPost, hostedStatusDelegationPath, hex.EncodeToString(bodyHash[:])}, "\n")
		mac := hmac.New(sha256.New, []byte("delegation-secret"))
		_, _ = mac.Write([]byte(canonical))
		if request.Header.Get(delegationSignatureHeader) != hex.EncodeToString(mac.Sum(nil)) ||
			request.Header.Get("Authorization") != "Bearer original-token" {
			t.Fatalf("invalid hosted status headers: %v", request.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"result":{"resultType":"complete","hosted_state":"session_open","starter_active":false,"active_tier":""}}`))
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client(), now)
	result, err := client.BillingStatus(context.Background(), testBillingCaller())
	if err != nil || result.HostedState != "session_open" || result.StarterActive {
		t.Fatalf("hosted status=%+v err=%v", result, err)
	}
}

func TestClientHostedStatusRejectsMalformedEnvelope(t *testing.T) {
	for _, body := range []string{
		`{"result":{"resultType":"complete","hosted_state":"active","active_tier":"starter"}}`,
		`{"result":{"resultType":"complete","hosted_state":"active","starter_active":true,"active_tier":"starter","customer_id":"cus_other"}}`,
		`{"result":{"resultType":"complete","hosted_state":"active","hosted_state":"terminal","starter_active":true,"active_tier":"starter"}}`,
		`{"result":{"resultType":"complete","hosted_state":"unknown","starter_active":false,"active_tier":""}}`,
		`{"result":{"resultType":"complete","hosted_state":"active","starter_active":false}}`,
		`{"result":{"resultType":"complete","hosted_state":"active","starter_active":true,"active_tier":"growth"}}`,
		`{"result":{"resultType":"complete","hosted_state":"session_open","starter_active":false,"active_tier":"scale"}}`,
		`{"result":{"resultType":"complete","hosted_state":"active","starter_active":false,"active_tier":"legacy"}}`,
		`{"result":{"resultType":"complete","hosted_state":"active","starter_active":true,"active_tier":"starter"},"error":{"code":"billing_invalid_state","retryable":false}}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(body))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, server.Client(), time.Unix(1_723_000_000, 0).UTC())
			_, err := client.BillingStatus(context.Background(), testBillingCaller())
			var business *mcp.BillingBusinessError
			if !errors.As(err, &business) || business.Code != mcp.BillingErrorTemporarilyUnavailable {
				t.Fatalf("malformed status accepted: err=%v", err)
			}
		})
	}
}

func TestHostedStatusDecodesOnlyConsistentPaidTier(t *testing.T) {
	for _, tier := range []string{"", "starter", "growth", "scale"} {
		state := "active"
		if tier == "" {
			state = "terminal"
		}
		body, err := json.Marshal(map[string]any{"result": map[string]any{"resultType": "complete", "hosted_state": state, "starter_active": tier == "starter", "active_tier": tier}})
		if err != nil {
			t.Fatal(err)
		}
		value, business, err := decodeHostedStatusResponse(body)
		if err != nil || business != nil || value.ActiveTier != tier || value.StarterActive != (tier == "starter") {
			t.Fatalf("tier=%q result=%+v business=%v err=%v", tier, value, business, err)
		}
	}
}

func TestClientHostedStatusTierChangeOptionalPairIsClosed(t *testing.T) {
	base := `"resultType":"complete","hosted_state":"active","starter_active":true,"active_tier":"starter"`
	for _, tc := range []struct {
		name, fields string
		valid        bool
	}{
		{"absent", "", true},
		{"pending-growth", `,"tier_change_state":"pending_payment","tier_change_offer_id":"growth_2026_09_v2"`, true},
		{"review-scale", `,"tier_change_state":"operator_review","tier_change_offer_id":"scale_2026_09_v2"`, true},
		{"state-only", `,"tier_change_state":"pending_payment"`, false},
		{"offer-only", `,"tier_change_offer_id":"growth_2026_09_v2"`, false},
		{"empty-pair", `,"tier_change_state":"","tier_change_offer_id":""`, false},
		{"empty-state", `,"tier_change_state":"","tier_change_offer_id":"growth_2026_09_v2"`, false},
		{"null-state", `,"tier_change_state":null,"tier_change_offer_id":"growth_2026_09_v2"`, false},
		{"null-offer", `,"tier_change_state":"pending_payment","tier_change_offer_id":null`, false},
		{"unknown-state", `,"tier_change_state":"unknown","tier_change_offer_id":"growth_2026_09_v2"`, false},
		{"unknown-offer", `,"tier_change_state":"pending_payment","tier_change_offer_id":"starter_2026_09_v2"`, false},
		{"mismatched-key", `,"tier_change_state":"pending_payment","tier_change_offer":"growth_2026_09_v2"`, false},
		{"duplicate", `,"tier_change_state":"pending_payment","Tier_Change_State":"applied","tier_change_offer_id":"growth_2026_09_v2"`, false},
		{"private-id", `,"tier_change_state":"pending_payment","tier_change_offer_id":"growth_2026_09_v2","change_intent_id":"private"`, false},
		{"applied-starter", `,"tier_change_state":"applied","tier_change_offer_id":"growth_2026_09_v2"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"result":{` + base + tc.fields + `}}`
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, server.Client(), time.Now())
			result, err := client.BillingStatus(context.Background(), testBillingCaller())
			if (err == nil) != tc.valid || calls != 1 {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, calls)
			}
			if tc.valid && (result.ActiveTier != "starter" || !result.StarterActive) {
				t.Fatalf("pending decision changed current tier: %+v", result)
			}
		})
	}
	for _, offer := range []struct{ id, tier string }{{"growth_2026_09_v2", "growth"}, {"scale_2026_09_v2", "scale"}} {
		for _, tier := range []string{"", "starter", "growth", "scale"} {
			body, _ := json.Marshal(map[string]any{"result": map[string]any{"resultType": "complete", "hosted_state": "active", "starter_active": tier == "starter", "active_tier": tier, "tier_change_state": "applied", "tier_change_offer_id": offer.id}})
			result, _, err := decodeHostedStatusResponse(body)
			if (err == nil) != (tier == "" || tier == offer.tier) {
				t.Fatalf("applied offer=%s tier=%s result=%+v err=%v", offer.id, tier, result, err)
			}
		}
	}
}
