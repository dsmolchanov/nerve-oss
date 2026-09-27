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
		_, _ = writer.Write([]byte(`{"result":{"resultType":"complete","hosted_state":"session_open","starter_active":false}}`))
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
		`{"result":{"resultType":"complete","hosted_state":"active"}}`,
		`{"result":{"resultType":"complete","hosted_state":"active","starter_active":true,"customer_id":"cus_other"}}`,
		`{"result":{"resultType":"complete","hosted_state":"active","hosted_state":"terminal","starter_active":true}}`,
		`{"result":{"resultType":"complete","hosted_state":"unknown","starter_active":false}}`,
		`{"result":{"resultType":"complete","hosted_state":"active","starter_active":true},"error":{"code":"billing_invalid_state","retryable":false}}`,
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
