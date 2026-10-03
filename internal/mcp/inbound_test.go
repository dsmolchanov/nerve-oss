package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
	"neuralmail/internal/localauth"
	"strings"
	"testing"
)

const inboundTestID = "11111111-1111-4111-8111-111111111111"
const inboundUsageFixture = `{"enrolled":false,"soft_limit":0,"hard_limit":0,"reserved":0,"materialized":0,"receipt_overflow":false,"usage_complete":true}`

type inboundStub struct {
	calls  int
	caller InboundCaller
	op     string
	input  InboundInput
	result json.RawMessage
	err    error
}

func (s *inboundStub) Inbound(_ context.Context, c InboundCaller, o string, i InboundInput) (json.RawMessage, error) {
	s.calls++
	s.caller = c
	s.op = o
	s.input = i
	return s.result, s.err
}
func inboundCaller() InboundCaller {
	p := activeBillingPrincipal("nerve:email.read")
	p.TokenID = "token-1"
	return InboundCaller{Principal: p, Authorization: "Bearer original-token"}
}
func TestInboundNativeRejectsAuthorityAndMalformedInputsBeforeDelegation(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{InboundUsageTool, `{"org_id":"foreign"}`}, {InboundUsageTool, `null`}, {InboundUsageTool, `{} {}`},
		{InboundReceiptsTool, `{"limit":101}`}, {InboundReceiptsTool, `{"limit":0}`}, {InboundReceiptsTool, `{"limit":1,"Limit":2}`}, {InboundReceiptsTool, `{"limit":true}`}, {InboundReceiptsTool, `{"cursor":"provider_id"}`},
		{InboundRecoverTool, `{"receipt_id":"provider_id"}`}, {InboundRecoverTool, `{"receipt_id":"` + inboundTestID + `","org_id":"foreign"}`}, {InboundRecoverTool, `{"receipt_id":"` + inboundTestID + `","receipt_id":"` + inboundTestID + `"}`},
	} {
		s := &inboundStub{}
		_, e := invokeInboundTool(context.Background(), s, inboundCaller(), tc.name, json.RawMessage(tc.body))
		if e == nil || s.calls != 0 {
			t.Fatalf("%s %s reached provider: %v", tc.name, tc.body, e)
		}
	}
	for _, change := range []func(*InboundCaller){func(c *InboundCaller) { c.Principal.Generation = 0 }, func(c *InboundCaller) { c.Principal.TokenID = "" }, func(c *InboundCaller) { c.Principal.Scopes = []string{"nerve:billing.subscribe"} }, func(c *InboundCaller) { c.Authorization = "Bearer x\r\n" }} {
		c := inboundCaller()
		change(&c)
		s := &inboundStub{}
		_, e := invokeInboundTool(context.Background(), s, c, InboundUsageTool, json.RawMessage(`{}`))
		if e == nil || s.calls != 0 {
			t.Fatal("invalid authority reached provider")
		}
	}
}
func TestInboundResultClosedContract(t *testing.T) {
	page := `{"items":[{"id":"` + inboundTestID + `","inbox_id":"","period_id":"","state":"content_paused","reason":"inbound_hard_cap","created_at":"2026-10-01T00:00:00Z","expires_at":"2026-11-01T00:00:00Z"}]}`
	recovery := `{"receipt_id":"` + inboundTestID + `","state":"materialized","message_id":"` + inboundTestID + `","provider_created_at":"2026-10-01T00:00:00Z","recovery_attempt_deadline":"2026-10-31T00:00:00Z"}`
	for _, tc := range []struct {
		op   string
		i    InboundInput
		good string
	}{{"usage", InboundInput{}, inboundUsageFixture}, {"receipts", InboundInput{Limit: 1}, page}, {"recover", InboundInput{ReceiptID: inboundTestID}, recovery}} {
		if _, e := ValidateInboundResult(tc.op, tc.i, []byte(tc.good)); e != nil {
			t.Fatalf("valid %s: %v", tc.op, e)
		}
		for _, bad := range []string{strings.TrimSuffix(tc.good, "}") + `,"body":"secret"}`, strings.TrimSuffix(tc.good, "}") + `,"provider_email_id":"secret"}`, strings.TrimSuffix(tc.good, "}") + `,"org_id":"foreign"}`, tc.good + `{}`, `null`, strings.Replace(tc.good, `"state":"materialized"`, `"state":"unexpected"`, 1)} {
			if bad == tc.good {
				continue
			}
			if _, e := ValidateInboundResult(tc.op, tc.i, []byte(bad)); e == nil {
				t.Fatalf("accepted %s", bad)
			}
		}
	}
	for _, bad := range []string{strings.Replace(inboundUsageFixture, `"hard_limit":0`, `"hard_limit":true`, 1), strings.Replace(inboundUsageFixture, `"reserved":0`, `"reserved":1`, 1), strings.Replace(inboundUsageFixture, `"usage_complete":true`, `"usage_complete":null`, 1), strings.Replace(inboundUsageFixture, `"enrolled":false`, `"enrolled":true`, 1), strings.Replace(inboundUsageFixture, `"enrolled":false`, `"enrolled":false,"Enrolled":false`, 1)} {
		if _, e := ValidateInboundResult("usage", InboundInput{}, []byte(bad)); e == nil {
			t.Fatalf("invalid usage %s", bad)
		}
	}
	for _, bad := range []string{strings.Replace(recovery, inboundTestID, "22222222-2222-4222-8222-222222222222", 1), strings.Replace(recovery, "2026-10-31", "2026-11-01", 1), strings.Replace(recovery, "materialized", "content_paused", 1), strings.TrimSuffix(recovery, "}") + `,"attachments_reopened":101}`} {
		if _, e := ValidateInboundResult("recover", InboundInput{ReceiptID: inboundTestID}, []byte(bad)); e == nil {
			t.Fatalf("invalid recovery %s", bad)
		}
	}
	if _, e := ValidateInboundResult("receipts", InboundInput{Limit: 1}, []byte(strings.Replace(page, `}]}`, `},`+strings.TrimPrefix(strings.TrimSuffix(page, `}`), `{"items":[`)+`}`, 1))); e == nil {
		t.Fatal("oversized or duplicate page accepted")
	}
}
func TestInboundModernReadRemainsAvailableAtZeroOutboundAllowance(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, authForInbound(cfg), nil)
	gate := &fakeEntitlementGate{preAuthErr: entitlements.ErrQuotaExceeded}
	runtime.Entitlements = gate
	stub := &inboundStub{result: json.RawMessage(inboundUsageFixture)}
	runtime.Inbound = stub
	c := inboundCaller()
	handler := NewSDKHandler(runtime, true)
	for _, name := range []string{InboundUsageTool, InboundReceiptsTool, InboundRecoverTool} {
		if !strings.Contains(rawJSON(modernToolCatalog(context.Background(), runtime, c.Principal)), name) {
			t.Fatalf("missing %s", name)
		}
	}
	request := billingModernRequest(t, c.Principal, "tools/call", map[string]any{"_meta": modernOAuthMeta(), "name": InboundUsageTool, "arguments": map[string]any{}}, InboundUsageTool)
	request.Header.Set("Authorization", c.Authorization)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"usage_complete":true`) || stub.calls != 1 || gate.preAuthCalls != 0 || stub.caller.Authorization != c.Authorization {
		t.Fatalf("call %d %s calls=%d quota=%d", recorder.Code, recorder.Body.String(), stub.calls, gate.preAuthCalls)
	}
	request = billingModernRequest(t, c.Principal, "tools/call", map[string]any{"_meta": modernOAuthMeta(), "name": InboundUsageTool, "arguments": map[string]any{}}, InboundUsageTool)
	request = request.WithContext(localauth.WithIdentity(request.Context(), localauth.Identity{}))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if stub.calls != 1 {
		t.Fatal("local caller reached provider")
	}
	stub.err = errors.New("private provider diagnostic")
	_, e := invokeInboundTool(context.Background(), stub, c, InboundUsageTool, json.RawMessage(`{}`))
	if e == nil || strings.Contains(e.Error(), "private") {
		t.Fatal("provider diagnostic escaped")
	}
}

func authForInbound(c config.Config) *auth.Service { return auth.NewService(c, nil) }

func TestInboundEveryNativeToolUsesItsDeclaredSchemaAndOriginalBearer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   map[string]any
		result string
	}{
		{InboundUsageTool, map[string]any{}, inboundUsageFixture},
		{InboundReceiptsTool, map[string]any{"limit": 1}, `{"items":[]}`},
		{InboundRecoverTool, map[string]any{"receipt_id": inboundTestID}, `{"receipt_id":"` + inboundTestID + `","state":"content_paused","reason":"storage_exhausted"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := hostedRouterConfig()
			runtime := NewServer(cfg, nil, authForInbound(cfg), nil)
			stub := &inboundStub{result: json.RawMessage(tc.result)}
			runtime.Inbound = stub
			c := inboundCaller()
			request := billingModernRequest(t, c.Principal, "tools/call", map[string]any{"_meta": modernOAuthMeta(), "name": tc.name, "arguments": tc.args}, tc.name)
			request.Header.Set("Authorization", c.Authorization)
			rec := httptest.NewRecorder()
			NewSDKHandler(runtime, true).ServeHTTP(rec, request)
			if rec.Code != 200 || strings.Contains(rec.Body.String(), `"isError":true`) || strings.Contains(rec.Body.String(), `"error":`) || stub.calls != 1 || stub.caller.Authorization != c.Authorization {
				t.Fatalf("native %d %s calls=%d", rec.Code, rec.Body.String(), stub.calls)
			}
		})
	}
}
