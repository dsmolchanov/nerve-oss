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
	"strconv"
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

func TestInboundNativeInvalidSuccessIsUnknownOnlyForRecovery(t *testing.T) {
	for _, operation := range []struct{ name, input, result string }{
		{InboundUsageTool, `{}`, inboundUsageFixture},
		{InboundReceiptsTool, `{"limit":1}`, `{"items":[]}`},
		{InboundRecoverTool, `{"receipt_id":"` + inboundTestID + `"}`, `{"receipt_id":"` + inboundTestID + `","state":"content_paused","reason":"storage_exhausted"}`},
	} {
		for _, defect := range []string{"invalid", "mismatched", "foreign"} {
			t.Run(operation.name+"/"+defect, func(t *testing.T) {
				bad := operation.result
				switch defect {
				case "invalid":
					bad = `null`
				case "foreign":
					bad = strings.TrimSuffix(bad, "}") + `,"org_id":"private-foreign-org"}`
				case "mismatched":
					switch operation.name {
					case InboundUsageTool:
						bad = strings.Replace(bad, `"reserved":0`, `"reserved":1`, 1)
					case InboundReceiptsTool:
						bad = `{"items":[],"next_cursor":"` + inboundTestID + `"}`
					case InboundRecoverTool:
						bad = strings.Replace(bad, inboundTestID, "22222222-2222-4222-8222-222222222222", 1)
					}
				}
				stub := &inboundStub{result: json.RawMessage(bad)}
				got, err := invokeInboundTool(context.Background(), stub, inboundCaller(), operation.name, json.RawMessage(operation.input))
				want := "inbound_retry_later"
				if operation.name == InboundRecoverTool {
					want = "inbound_outcome_unknown"
				}
				var business *InboundBusinessError
				mapped, mapResult := got.(map[string]any)
				if (got != nil && (!mapResult || mapped != nil)) || stub.calls != 1 || !errors.As(err, &business) || business.Code != want || !business.Retryable {
					t.Fatalf("result=%v calls=%d err=%v", got, stub.calls, err)
				}
				cfg := hostedRouterConfig()
				runtime := NewServer(cfg, nil, authForInbound(cfg), nil)
				runtime.Inbound = stub
				var args map[string]any
				if err := json.Unmarshal([]byte(operation.input), &args); err != nil {
					t.Fatal(err)
				}
				caller := inboundCaller()
				request := billingModernRequest(t, caller.Principal, "tools/call", map[string]any{"_meta": modernOAuthMeta(), "name": operation.name, "arguments": args}, operation.name)
				request.Header.Set("Authorization", caller.Authorization)
				recorder := httptest.NewRecorder()
				NewSDKHandler(runtime, true).ServeHTTP(recorder, request)
				if stub.calls != 2 || !strings.Contains(recorder.Body.String(), `"code":"`+want+`"`) || !strings.Contains(recorder.Body.String(), `"retryable":true`) || strings.Contains(recorder.Body.String(), "private-foreign-org") {
					t.Fatalf("native response calls=%d: %s", stub.calls, recorder.Body.String())
				}
			})
		}
	}
}

func TestInboundAttachmentRetryStateIsClosedAndMaterializedOnly(t *testing.T) {
	for _, retryState := range []string{"evaluated", "retry_later"} {
		for _, reopened := range []int{0, 2} {
			body := `{"receipt_id":"` + inboundTestID + `","state":"materialized","message_id":"` + inboundTestID + `","attachment_retry_state":"` + retryState + `","attachments_reopened":` + strconv.Itoa(reopened) + `}`
			if _, err := ValidateInboundResult("recover", InboundInput{ReceiptID: inboundTestID}, []byte(body)); err != nil {
				t.Fatalf("valid independent state/count %s: %v", body, err)
			}
		}
	}
	for _, bad := range []string{
		`{"receipt_id":"` + inboundTestID + `","state":"materialized","message_id":"` + inboundTestID + `","attachment_retry_state":"unexpected"}`,
		`{"receipt_id":"` + inboundTestID + `","state":"materialized","message_id":"` + inboundTestID + `","attachment_retry_state":null}`,
		`{"receipt_id":"` + inboundTestID + `","state":"materialized","message_id":"` + inboundTestID + `","attachment_retry_state":true}`,
	} {
		if _, err := ValidateInboundResult("recover", InboundInput{ReceiptID: inboundTestID}, []byte(bad)); err == nil {
			t.Fatalf("accepted invalid retry state: %s", bad)
		}
	}
	for _, state := range []string{"pending", "content_paused", "expired_unrecoverable"} {
		body := `{"receipt_id":"` + inboundTestID + `","state":"` + state + `","reason":"provider_unavailable","attachment_retry_state":"retry_later"}`
		if _, err := ValidateInboundResult("recover", InboundInput{ReceiptID: inboundTestID}, []byte(body)); err == nil {
			t.Fatalf("accepted nonmaterialized retry state: %s", body)
		}
	}
	for _, tc := range []struct {
		op    string
		input InboundInput
		body  string
	}{
		{"usage", InboundInput{}, strings.TrimSuffix(inboundUsageFixture, "}") + `,"attachment_retry_state":"evaluated"}`},
		{"receipts", InboundInput{Limit: 1}, `{"items":[],"attachment_retry_state":"evaluated"}`},
		{"receipts", InboundInput{Limit: 1}, `{"items":[{"id":"` + inboundTestID + `","inbox_id":"","period_id":"","state":"materialized","message_id":"` + inboundTestID + `","created_at":"2026-10-01T00:00:00Z","expires_at":"2026-11-01T00:00:00Z","attachment_retry_state":"evaluated"}]}`},
	} {
		if _, err := ValidateInboundResult(tc.op, tc.input, []byte(tc.body)); err == nil {
			t.Fatalf("accepted read-only retry state: %s", tc.body)
		}
	}
}

func TestInboundNativeRecoveryErrorsAreMutationOnly(t *testing.T) {
	for _, tc := range []struct{ name, input string }{{InboundUsageTool, `{}`}, {InboundReceiptsTool, `{"limit":1}`}, {InboundRecoverTool, `{"receipt_id":"` + inboundTestID + `"}`}} {
		for _, business := range []*InboundBusinessError{
			{Code: "recovery_in_progress_or_changed", Retryable: true},
			{Code: "recovery_response_exceeds_limit"},
			{Code: "recovery_provider_or_save_unavailable", Retryable: true},
			{Code: "inbound_outcome_unknown", Retryable: true},
		} {
			t.Run(tc.name+"/"+business.Code, func(t *testing.T) {
				stub := &inboundStub{err: business}
				_, err := invokeInboundTool(context.Background(), stub, inboundCaller(), tc.name, json.RawMessage(tc.input))
				want, retryable := business.Code, business.Retryable
				if tc.name != InboundRecoverTool {
					want = "inbound_retry_later"
					retryable = true
				}
				var actual *InboundBusinessError
				if stub.calls != 1 || !errors.As(err, &actual) || actual.Code != want || actual.Retryable != retryable {
					t.Fatalf("err=%v calls=%d want=%s/%v", err, stub.calls, want, retryable)
				}
			})
		}
	}
}

func TestInboundNativeCatalogErrorsMatchReadAndMutationContracts(t *testing.T) {
	cfg := hostedRouterConfig()
	runtime := NewServer(cfg, nil, authForInbound(cfg), nil)
	runtime.Inbound = &inboundStub{}
	caller := inboundCaller()
	request := billingModernRequest(t, caller.Principal, "tools/list", map[string]any{"_meta": modernOAuthMeta()}, "")
	request.Header.Set("Authorization", caller.Authorization)
	recorder := httptest.NewRecorder()
	NewSDKHandler(runtime, true).ServeHTTP(recorder, request)
	var listed struct {
		Result struct {
			Tools []struct {
				Name         string         `json:"name"`
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil || recorder.Code != http.StatusOK {
		t.Fatalf("list=%s err=%v", recorder.Body.String(), err)
	}
	for _, name := range []string{InboundUsageTool, InboundReceiptsTool, InboundRecoverTool} {
		t.Run(name, func(t *testing.T) {
			var schema map[string]any
			for _, tool := range listed.Result.Tools {
				if tool.Name == name {
					schema = tool.OutputSchema
				}
			}
			if schema == nil {
				t.Fatalf("missing scoped tool %s", name)
			}
			outcomes := schema["oneOf"].([]any)
			errorShape := outcomes[1].(map[string]any)
			properties := errorShape["properties"].(map[string]any)["error"].(map[string]any)["properties"].(map[string]any)
			codes := properties["code"].(map[string]any)["enum"].([]any)
			wanted := []string{"inbound_invalid_request", "inbound_unavailable", "inbound_retry_later"}
			if name == InboundRecoverTool {
				wanted = append(wanted, "inbound_outcome_unknown", "recovery_in_progress_or_changed", "recovery_provider_or_save_unavailable", "recovery_response_exceeds_limit")
			}
			if len(codes) != len(wanted) {
				t.Fatalf("schema codes=%v want=%v", codes, wanted)
			}
			for i, code := range codes {
				if code != wanted[i] {
					t.Fatalf("schema codes=%v want=%v", codes, wanted)
				}
			}
		})
	}
}

func TestInboundOwnerBoundaryOutcomesRemainContentFreeAcrossNativeTools(t *testing.T) {
	for _, reason := range []string{"ownership_changed", "ownership_unverified"} {
		for _, id := range []string{inboundTestID, "q_" + strings.Repeat("a", 64)} {
			body := `{"receipt_id":"` + id + `","state":"expired_unrecoverable","reason":"` + reason + `"}`
			stub := &inboundStub{result: json.RawMessage(body)}
			got, err := invokeInboundTool(context.Background(), stub, inboundCaller(), InboundRecoverTool, json.RawMessage(`{"receipt_id":"`+id+`"}`))
			if err != nil || got == nil || stub.calls != 1 {
				t.Fatalf("terminal %s: %v calls=%d", body, err, stub.calls)
			}
			page := `{"items":[{"id":"` + id + `","inbox_id":"","period_id":"","state":"expired_unrecoverable","reason":"` + reason + `","created_at":"2026-10-01T00:00:00Z","expires_at":"2026-11-01T00:00:00Z"}]}`
			if _, err := ValidateInboundResult("receipts", InboundInput{Limit: 1}, []byte(page)); err != nil {
				t.Fatal(err)
			}
			for _, extra := range []string{`,"message_id":"` + inboundTestID + `"`, `,"body":"prior owner private mail"`, `,"attachments_reopened":1`, `,"provider_email_id":"private"`} {
				if _, err := ValidateInboundResult("recover", InboundInput{ReceiptID: id}, []byte(strings.TrimSuffix(body, "}")+extra+"}")); err == nil {
					t.Fatalf("terminal outcome admitted prior content: %s", extra)
				}
			}
		}
	}
}

func TestInboundOwnershipReasonsAreTerminalForRecoveryAndReceiptList(t *testing.T) {
	for _, reason := range []string{"ownership_changed", "ownership_unverified"} {
		for _, state := range []string{"pending", "content_paused", "materialized"} {
			for _, op := range []string{"recover", "receipts"} {
				t.Run(op+"/"+reason+"/"+state, func(t *testing.T) {
					extra := ""
					if state == "materialized" {
						extra = `,"message_id":"` + inboundTestID + `"`
					}
					body := `{"receipt_id":"` + inboundTestID + `","state":"` + state + `","reason":"` + reason + `"` + extra + `}`
					input := InboundInput{ReceiptID: inboundTestID}
					want := "inbound_outcome_unknown"
					if op == "receipts" {
						body = `{"items":[{"id":"` + inboundTestID + `","inbox_id":"","period_id":"","state":"` + state + `","reason":"` + reason + `","created_at":"2026-10-01T00:00:00Z","expires_at":"2026-11-01T00:00:00Z"` + extra + `}]}`
						input = InboundInput{Limit: 1}
						want = "inbound_retry_later"
					}
					_, err := ValidateInboundResult(op, input, []byte(body))
					var outcome *InboundBusinessError
					if !errors.As(err, &outcome) || outcome.Code != want || !outcome.Retryable {
						t.Fatalf("impossible ownership outcome accepted: %s err=%v", body, err)
					}
				})
			}
		}
	}
}
