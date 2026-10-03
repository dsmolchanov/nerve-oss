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
