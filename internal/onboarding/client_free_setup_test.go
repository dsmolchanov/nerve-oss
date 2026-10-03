package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"neuralmail/internal/auth"
	"neuralmail/internal/mcp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func freeClientCaller() mcp.OnboardingCaller {
	c := testCaller()
	c.Principal.AuthMethod = "m2m_bearer"
	c.Principal.Kind = auth.PrincipalM2MOnboarding
	c.Principal.OrgID = ""
	c.Principal.TokenID = "token-1"
	c.Principal.Scopes = []string{"nerve:onboarding"}
	return c
}
func freeClientResponse() string {
	return `{"result":{"resultType":"complete","setup_id":"11111111-1111-4111-8111-111111111111","onboarding_id":"22222222-2222-4222-8222-222222222222","generation":7,"apex_domain":"example.com","setup_state":"pending","state":"provisioning","setup_expires_at":"2026-10-04T00:00:00Z","ownership_txt":{"type":"TXT","name":"_nerve-verify.example.com","value":"nerve-free-verification=` + strings.Repeat("a", 64) + `"},"next_action":"configure_ownership_dns_then_verify","reauthorize":false}}`
}
func TestClientFreeSetupSignsEachExactOriginalCallerTuple(t *testing.T) {
	for op, input := range map[string]string{"setup": `{"idempotency_key":"free","organization_name":"Free","apex_domain":"example.com","local_part":"agent"}`, "status": `{}`, "verify-domain": `{}`, "close": `{"idempotency_key":"close","expected_generation":7}`} {
		t.Run(op, func(t *testing.T) {
			calls := 0
			var client *Client
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				var envelope delegationRequest
				if json.Unmarshal(body, &envelope) != nil || envelope.Principal.OrgID != "" || envelope.Principal.TokenID != "token-1" || envelope.Principal.Generation != 7 || r.URL.Path != mcp.FreeSetupDelegationPath+op || r.Header.Get("Authorization") != freeClientCaller().Authorization {
					t.Errorf("wrong caller/body %s", body)
				}
				if r.Header.Get(delegationSignatureHeader) != client.signature("POST", r.URL.EscapedPath(), r.Header.Get(delegationNonceHeader), r.Header.Get(delegationTimestampHeader), r.Header.Get(delegationBodyHashHeader)) {
					t.Error("wrong signature")
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, freeClientResponse())
			}))
			defer server.Close()
			client = newTestClient(t, server.URL, server.Client(), time.Now())
			got, err := client.FreeSetup(context.Background(), freeClientCaller(), op, json.RawMessage(input))
			if err != nil || got.Generation != 7 || got.Address != "" || calls != 1 {
				t.Fatalf("result%+v err%v calls%d", got, err, calls)
			}
		})
	}
}
func TestClientFreeSetupAmbiguousMutationResponsesRequireStatusWithoutRetry(t *testing.T) {
	bad := []string{`{}`, freeClientResponse() + ` {}`, strings.Replace(freeClientResponse(), `"reauthorize":false`, `"reauthorize":false,"address":"agent@example.com"`, 1), strings.Replace(freeClientResponse(), `"generation":7`, `"generation":8`, 1), strings.Replace(freeClientResponse(), `"reauthorize":false`, `"reauthorize":false,"provider_id":"private"`, 1), strings.Replace(freeClientResponse(), `"reauthorize":false`, `"reauthorize":false,"Reauthorize":true`, 1), strings.Repeat("x", 65537), `{"error":{"code":"private-provider-error","retryable":true}}`}
	for op, input := range map[string]string{"setup": `{"idempotency_key":"free","organization_name":"Free","apex_domain":"example.com","local_part":"agent"}`, "status": `{}`, "verify-domain": `{}`, "close": `{"idempotency_key":"close","expected_generation":7}`} {
		for _, body := range bad {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body)
			}))
			client := newTestClient(t, server.URL, server.Client(), time.Now())
			_, err := client.FreeSetup(context.Background(), freeClientCaller(), op, json.RawMessage(input))
			server.Close()
			if err == nil || calls != 1 || errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) != (op != "status") || strings.Contains(err.Error(), "private-provider-error") {
				t.Fatalf("%s err%v calls%d", op, err, calls)
			}
		}
	}
}

func TestClientFreeSetupTransportAmbiguityAndNoRedirect(t *testing.T) {
	inputs := map[string]string{"setup": `{"idempotency_key":"free","organization_name":"Free","apex_domain":"example.com","local_part":"agent"}`, "status": `{}`, "verify-domain": `{}`, "close": `{"idempotency_key":"close","expected_generation":7}`}
	for op, input := range inputs {
		for _, mode := range []string{"disconnect", "body timeout", "redirect", "wrong content type", "wrong status"} {
			t.Run(op+"/"+mode, func(t *testing.T) {
				var calls atomic.Int64
				var redirectCalls atomic.Int64
				redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					redirectCalls.Add(1)
					io.WriteString(w, freeClientResponse())
				}))
				defer redirected.Close()
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					switch mode {
					case "disconnect":
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
						return
					case "body timeout":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(200)
						w.(http.Flusher).Flush()
						<-r.Context().Done()
						return
					case "redirect":
						http.Redirect(w, r, redirected.URL, 307)
						return
					case "wrong content type":
						w.Header().Set("Content-Type", "text/plain")
					case "wrong status":
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(500)
					}
					io.WriteString(w, freeClientResponse())
				}))
				defer server.Close()
				client := newTestClient(t, server.URL, server.Client(), time.Now())
				client.timeout = 50 * time.Millisecond
				_, err := client.FreeSetup(context.Background(), freeClientCaller(), op, json.RawMessage(input))
				if err == nil || calls.Load() != 1 || redirectCalls.Load() != 0 || errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) != (op != "status") {
					t.Fatalf("mode%s op%s err%v calls%d redirects%d", mode, op, err, calls.Load(), redirectCalls.Load())
				}
			})
		}
	}
}
func TestClientFreeSetupReturnsOnlyValidatedDurableBusinessErrors(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"code":"onboarding_temporarily_unavailable","retryable":true}}`)
	}))
	defer server.Close()
	client := newTestClient(t, server.URL, server.Client(), time.Now())
	_, err := client.FreeSetup(context.Background(), freeClientCaller(), "verify-domain", json.RawMessage(`{}`))
	var business *mcp.OnboardingBusinessError
	if !errors.As(err, &business) || business.Code != mcp.OnboardingErrorTemporarilyUnavailable || !business.Retryable || calls != 1 || errors.Is(err, mcp.ErrOnboardingOutcomeUnknown) {
		t.Fatalf("err%v calls%d", err, calls)
	}
}
