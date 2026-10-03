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
	"neuralmail/internal/mcp"
	"strings"
	"testing"
	"time"
)

func inboundTestCaller() mcp.InboundCaller {
	c := testBillingCaller()
	c.Principal.Scopes = []string{"nerve:email.read"}
	return mcp.InboundCaller{Principal: c.Principal, Authorization: c.Authorization}
}
func TestClientInboundDelegatesOriginalReadBearerAndExactSignedTuple(t *testing.T) {
	for _, op := range []string{"usage", "receipts", "recover"} {
		t.Run(op, func(t *testing.T) {
			input := mcp.InboundInput{}
			response := `{"enrolled":false,"soft_limit":0,"hard_limit":0,"reserved":0,"materialized":0,"receipt_overflow":false,"usage_complete":true}`
			if op == "receipts" {
				input.Limit = 1
				response = `{"items":[]}`
			}
			if op == "recover" {
				input.ReceiptID = "11111111-1111-4111-8111-111111111111"
				response = `{"receipt_id":"` + input.ReceiptID + `","state":"content_paused","reason":"storage_exhausted"}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, e := io.ReadAll(r.Body)
				if e != nil {
					t.Fatal(e)
				}
				var envelope struct {
					Principal delegationPrincipal `json:"principal"`
					Input     mcp.InboundInput    `json:"input"`
				}
				if e = json.Unmarshal(body, &envelope); e != nil || envelope.Input != input || envelope.Principal.OrgID != "org-1" || envelope.Principal.Generation != 7 || envelope.Principal.TokenID != "token-1" {
					t.Fatalf("tuple %+v %v", envelope, e)
				}
				hash := sha256.Sum256(body)
				canonical := strings.Join([]string{"runtime-current", "nonce-1", "1723000000", http.MethodPost, mcp.InboundDelegationPath + op, hex.EncodeToString(hash[:])}, "\n")
				mac := hmac.New(sha256.New, []byte("delegation-secret"))
				mac.Write([]byte(canonical))
				if r.Header.Get(delegationSignatureHeader) != hex.EncodeToString(mac.Sum(nil)) || r.Header.Get("Authorization") != "Bearer original-token" {
					t.Fatal("signature or original bearer changed")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(response))
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, server.Client(), time.Unix(1723000000, 0).UTC())
			got, e := client.Inbound(context.Background(), inboundTestCaller(), op, input)
			if e != nil || string(got) != response {
				t.Fatalf("result %s %v", got, e)
			}
		})
	}
}
func TestClientInboundRecoveryInvalidPostCommitResponsesAreUnknown(t *testing.T) {
	for _, body := range []string{`{}`, `{"receipt_id":"11111111-1111-4111-8111-111111111111","state":"pending","body":"secret"}`, strings.Repeat("x", 65537), `{"receipt_id":"11111111-1111-4111-8111-111111111111","state":"pending","state":"materialized"}`} {
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		}))
		client := newTestClient(t, server.URL, server.Client(), time.Now())
		_, e := client.Inbound(context.Background(), inboundTestCaller(), "recover", mcp.InboundInput{ReceiptID: "11111111-1111-4111-8111-111111111111"})
		server.Close()
		var business *mcp.InboundBusinessError
		if !errors.As(e, &business) || business.Code != "inbound_outcome_unknown" || calls != 1 {
			t.Fatalf("unknown result=%v calls=%d", e, calls)
		}
	}
}

func TestClientInboundAttachmentRetryStateClosedResponse(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	base := `{"receipt_id":"` + id + `","state":"materialized","message_id":"` + id + `","attachments_reopened":0}`
	for _, tc := range []struct {
		name, field string
		valid       bool
	}{
		{"absent", "", true},
		{"evaluated", `,"attachment_retry_state":"evaluated"`, true},
		{"retry-later", `,"attachment_retry_state":"retry_later"`, true},
		{"unknown", `,"attachment_retry_state":"unknown"`, false},
		{"null", `,"attachment_retry_state":null`, false},
		{"duplicate", `,"attachment_retry_state":"evaluated","attachment_retry_state":"retry_later"`, false},
		{"foreign", `,"attachment_retry_state":"evaluated","org_id":"private"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.TrimSuffix(base, "}") + tc.field + "}"
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, body)
			}))
			defer server.Close()
			client := newTestClient(t, server.URL, server.Client(), time.Now())
			result, err := client.Inbound(context.Background(), inboundTestCaller(), "recover", mcp.InboundInput{ReceiptID: id})
			if calls != 1 {
				t.Fatalf("automatic retry: %d", calls)
			}
			if tc.valid {
				if err != nil || string(result) != body {
					t.Fatalf("valid result=%s err=%v", result, err)
				}
				return
			}
			var business *mcp.InboundBusinessError
			if result != nil || !errors.As(err, &business) || business.Code != "inbound_outcome_unknown" || !business.Retryable {
				t.Fatalf("ambiguous invalid response=%s err=%v", result, err)
			}
		})
	}
}

func TestClientInboundHTTPErrorMappingIsOperationAware(t *testing.T) {
	for _, operation := range []string{"usage", "receipts", "recover"} {
		for _, tc := range []struct {
			name       string
			status     int
			body, code string
			retryable  bool
		}{
			{"bad-request", 400, `{"error":"private"}`, "inbound_invalid_request", false},
			{"unauthorized", 401, `{"error":"private"}`, "inbound_unavailable", false},
			{"forbidden", 403, `{"error":"private"}`, "inbound_unavailable", false},
			{"not-found", 404, `{"error":"private"}`, "inbound_unavailable", false},
			{"unavailable", 409, `{"error":"inbound_unavailable"}`, "inbound_unavailable", false},
			{"progress", 409, `{"error":"recovery_in_progress_or_changed"}`, "recovery_in_progress_or_changed", true},
			{"limit", 409, `{"error":"recovery_response_exceeds_limit"}`, "recovery_response_exceeds_limit", false},
			{"unknown-recovery", 409, `{"error":"recovery_private_code"}`, "inbound_outcome_unknown", true},
			{"extra-field", 409, `{"error":"recovery_in_progress_or_changed","provider_id":"private"}`, "inbound_outcome_unknown", true},
			{"duplicate", 409, `{"error":"inbound_unavailable","error":"recovery_in_progress_or_changed"}`, "inbound_outcome_unknown", true},
			{"null", 409, `{"error":null}`, "inbound_outcome_unknown", true},
			{"malformed", 409, `{`, "inbound_outcome_unknown", true},
			{"gateway", 502, `{"error":"private"}`, "recovery_provider_or_save_unavailable", true},
			{"unexpected", 503, `{"error":"private"}`, "inbound_outcome_unknown", true},
		} {
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				input := mcp.InboundInput{}
				if operation == "receipts" {
					input.Limit = 1
				}
				if operation == "recover" {
					input.ReceiptID = "11111111-1111-4111-8111-111111111111"
				}
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
				}))
				defer server.Close()
				client := newTestClient(t, server.URL, server.Client(), time.Now())
				result, err := client.Inbound(context.Background(), inboundTestCaller(), operation, input)
				code, retryable := tc.code, tc.retryable
				if operation != "recover" && (strings.HasPrefix(code, "recovery_") || code == "inbound_outcome_unknown") {
					code = "inbound_retry_later"
					retryable = true
				}
				var business *mcp.InboundBusinessError
				if result != nil || calls != 1 || !errors.As(err, &business) || business.Code != code || business.Retryable != retryable || strings.Contains(err.Error(), "private") {
					t.Fatalf("result=%s err=%v calls=%d want=%s/%v", result, err, calls, code, retryable)
				}
			})
		}
	}
}
