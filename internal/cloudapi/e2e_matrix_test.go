package cloudapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"neuralmail/internal/auth"
	"neuralmail/internal/billing"
	"neuralmail/internal/config"
	"neuralmail/internal/entitlements"
	"neuralmail/internal/llm"
	"neuralmail/internal/mcp"
	"neuralmail/internal/observability"
	"neuralmail/internal/policy"
	"neuralmail/internal/store"
	"neuralmail/internal/tools"
)

const (
	bootstrapAdminAPIKey   = "bootstrap-admin"
	e2eTokenSigningKey     = "e2e-test-signing-key"
	e2eStripeWebhookSecret = "whsec_e2e_test_secret"
)

type cloudE2EHarness struct {
	ctx          context.Context
	store        *store.Store
	controlPlane *httptest.Server
	mcp          *httptest.Server
}

type rpcResponse struct {
	JSONRPC string             `json:"jsonrpc"`
	ID      any                `json:"id"`
	Result  json.RawMessage    `json:"result,omitempty"`
	Error   *mcp.ResponseError `json:"error,omitempty"`
}

type fixedDraftLLM struct {
	draftText string
}

func (f *fixedDraftLLM) Classify(_ context.Context, _ string, _ map[string]any) (llm.Classification, error) {
	return llm.Classification{
		Intent:     "refund_request",
		Urgency:    "high",
		Sentiment:  "negative",
		Confidence: 0.99,
	}, nil
}

func (f *fixedDraftLLM) Extract(_ context.Context, _ string, _ map[string]any, _ []map[string]any) (llm.Extraction, error) {
	return llm.Extraction{Data: map[string]any{}, Confidence: 0.0}, nil
}

func (f *fixedDraftLLM) Draft(_ context.Context, _ string, _ map[string]any, _ string) (llm.Draft, error) {
	return llm.Draft{
		Text:          f.draftText,
		Citations:     nil,
		RiskFlags:     nil,
		NeedsApproval: false,
	}, nil
}

func (f *fixedDraftLLM) Name() string  { return "fixed-draft-llm" }
func (f *fixedDraftLLM) Model() string { return "fixed-draft-llm" }

func TestCloudE2EMatrix(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		h := newCloudE2EHarness(t, ctx, st)
		defer h.Close()

		t.Run("RBACAndScoping", func(t *testing.T) {
			orgID := h.createOrg(t, "rbac-org")
			h.upsertActiveEntitlement(t, orgID, 1000, 1000)
			inboxID, threadID, _ := h.seedInboxThreadMessage(t, orgID, "rbac@local.neuralmail", "RBAC", "Secret Project X")

			token := h.issueServiceToken(t, orgID, []string{"nerve:email.read", "nerve:email.search"}, false)
			session := h.initializeSession(t, token)

			status, resp := h.callTool(t, token, session, "search_inbox", map[string]any{
				"inbox_id": inboxID,
				"query":    "Project X",
				"top_k":    5,
			})
			if status != http.StatusOK {
				t.Fatalf("expected successful search status, got %d", status)
			}
			if resp.Error != nil {
				t.Fatalf("expected successful search, got rpc error %+v", resp.Error)
			}

			status, _ = h.callTool(t, token, session, "draft_reply_with_policy", map[string]any{
				"thread_id": threadID,
				"goal":      "Promise immediate refund.",
			})
			if status != http.StatusForbidden {
				t.Fatalf("expected forbidden status for missing draft scope, got %d", status)
			}

			used := h.currentUsage(t, orgID)
			if used != 1 {
				t.Fatalf("expected exactly one metered call after blocked draft, got %d", used)
			}
		})

		t.Run("CrossTenantIsolation", func(t *testing.T) {
			orgA := h.createOrg(t, "tenant-a")
			orgB := h.createOrg(t, "tenant-b")
			h.upsertActiveEntitlement(t, orgA, 1000, 1000)
			h.upsertActiveEntitlement(t, orgB, 1000, 1000)

			inboxA, _, messageA := h.seedInboxThreadMessage(t, orgA, "a@local.neuralmail", "Tenant A", "Secret Project X alpha")
			inboxB, _, messageB := h.seedInboxThreadMessage(t, orgB, "b@local.neuralmail", "Tenant B", "Secret Project X beta")

			tokenA := h.issueServiceToken(t, orgA, []string{"nerve:email.read", "nerve:email.search"}, false)
			sessionA := h.initializeSession(t, tokenA)

			status, resp := h.readResource(t, tokenA, sessionA, "email://inboxes")
			if status != http.StatusOK || resp.Error != nil {
				t.Fatalf("expected resources/read success, status=%d err=%+v", status, resp.Error)
			}
			var inboxPayload struct {
				InboxIDs []string `json:"inbox_ids"`
			}
			decodeRawResult(t, resp.Result, &inboxPayload)
			if len(inboxPayload.InboxIDs) != 1 || inboxPayload.InboxIDs[0] != inboxA {
				t.Fatalf("expected only tenant A inbox, got %#v", inboxPayload.InboxIDs)
			}

			status, resp = h.callTool(t, tokenA, sessionA, "search_inbox", map[string]any{
				"inbox_id": inboxA,
				"query":    "Project X",
				"top_k":    5,
			})
			if status != http.StatusOK || resp.Error != nil {
				t.Fatalf("expected tenant A search success, status=%d err=%+v", status, resp.Error)
			}
			var searchPayload struct {
				Results []struct {
					MessageID string `json:"message_id"`
				} `json:"results"`
			}
			decodeRawResult(t, resp.Result, &searchPayload)
			if len(searchPayload.Results) != 1 || searchPayload.Results[0].MessageID != messageA {
				t.Fatalf("expected only tenant A message, got %#v", searchPayload.Results)
			}
			if searchPayload.Results[0].MessageID == messageB {
				t.Fatalf("cross-tenant data leak detected: returned tenant B message")
			}

			status, resp = h.callTool(t, tokenA, sessionA, "search_inbox", map[string]any{
				"inbox_id": inboxB,
				"query":    "Project X",
				"top_k":    5,
			})
			if status != http.StatusOK {
				t.Fatalf("expected rpc error response status, got %d", status)
			}
			if resp.Error == nil || resp.Error.Code != -32000 {
				t.Fatalf("expected ownership mismatch rpc error, got %#v", resp.Error)
			}
		})

		t.Run("PolicySoftBlock", func(t *testing.T) {
			orgID := h.createOrg(t, "policy-org")
			h.upsertActiveEntitlement(t, orgID, 1000, 1000)
			_, threadID, _ := h.seedInboxThreadMessage(t, orgID, "policy@local.neuralmail", "Dispute", "My $500 transaction was unauthorized.")

			token := h.issueServiceToken(t, orgID, []string{"nerve:email.read", "nerve:email.draft"}, false)
			session := h.initializeSession(t, token)

			status, resp := h.callTool(t, token, session, "draft_reply_with_policy", map[string]any{
				"thread_id": threadID,
				"goal":      "Resolve quickly",
			})
			if status != http.StatusOK || resp.Error != nil {
				t.Fatalf("expected policy-evaluated draft response, status=%d err=%+v", status, resp.Error)
			}
			var draftPayload struct {
				Draft              string `json:"draft"`
				PolicyBlocked      bool   `json:"policy_blocked"`
				NeedsHumanApproval bool   `json:"needs_human_approval"`
				Reason             string `json:"reason"`
			}
			decodeRawResult(t, resp.Result, &draftPayload)
			if !draftPayload.PolicyBlocked {
				t.Fatalf("expected soft block for high-risk refund promise")
			}
			if !draftPayload.NeedsHumanApproval {
				t.Fatalf("expected needs_human_approval=true")
			}
			if draftPayload.Draft != "" {
				t.Fatalf("expected blocked draft body to be empty")
			}
			if !strings.Contains(strings.ToLower(draftPayload.Reason), "forbidden phrase") {
				t.Fatalf("expected policy reason to mention forbidden phrase, got %q", draftPayload.Reason)
			}
		})

		t.Run("QuotaEnforcement", func(t *testing.T) {
			orgID := h.createOrg(t, "quota-org")
			h.upsertActiveEntitlement(t, orgID, 1000, 5)
			inboxID, _, _ := h.seedInboxThreadMessage(t, orgID, "quota@local.neuralmail", "Quota", "quota test")

			token := h.issueServiceToken(t, orgID, []string{"nerve:email.read"}, false)
			session := h.initializeSession(t, token)

			for i := 0; i < 5; i++ {
				status, resp := h.callTool(t, token, session, "list_threads", map[string]any{
					"inbox_id": inboxID,
					"limit":    1,
				})
				if status != http.StatusOK || resp.Error != nil {
					t.Fatalf("expected quota call %d to succeed, status=%d err=%+v", i+1, status, resp.Error)
				}
			}

			status, resp := h.callTool(t, token, session, "list_threads", map[string]any{
				"inbox_id": inboxID,
				"limit":    1,
			})
			if status != http.StatusOK {
				t.Fatalf("expected rpc response for quota exceed, got status=%d", status)
			}
			if resp.Error == nil || resp.Error.Code != -32040 || resp.Error.Message != "quota_exceeded" {
				t.Fatalf("expected quota_exceeded rpc error, got %#v", resp.Error)
			}

			used := h.currentUsage(t, orgID)
			if used != 5 {
				t.Fatalf("expected usage to stop at quota boundary 5, got %d", used)
			}
		})

		t.Run("BillingWebhookDeactivationBlocksMCP", func(t *testing.T) {
			orgID := h.createOrg(t, "billing-org")
			h.upsertPlan(t, "pro", 120, 1000, 10)
			inboxID, _, _ := h.seedInboxThreadMessage(t, orgID, "billing@local.neuralmail", "Billing", "billing test")
			subscriptionID := "sub_" + uuid.NewString()
			customerID := "cus_" + uuid.NewString()

			h.postStripeSubscriptionEvent(t, map[string]any{
				"id":   "evt_activate_" + uuid.NewString(),
				"type": "customer.subscription.updated",
				"data": map[string]any{
					"object": map[string]any{
						"id":                   subscriptionID,
						"customer":             customerID,
						"status":               "active",
						"current_period_start": 1_700_000_000,
						"current_period_end":   1_999_999_999,
						"cancel_at_period_end": false,
						"metadata": map[string]any{
							"org_id": orgID,
						},
						"items": map[string]any{
							"data": []map[string]any{
								{
									"price": map[string]any{
										"lookup_key": "pro",
										"id":         "price_pro",
									},
								},
							},
						},
					},
				},
			})

			token := h.issueServiceToken(t, orgID, []string{"nerve:email.read"}, false)
			session := h.initializeSession(t, token)

			status, resp := h.callTool(t, token, session, "list_threads", map[string]any{
				"inbox_id": inboxID,
				"limit":    1,
			})
			if status != http.StatusOK || resp.Error != nil {
				t.Fatalf("expected active subscription to allow tool calls, status=%d err=%+v", status, resp.Error)
			}

			h.postStripeSubscriptionEvent(t, map[string]any{
				"id":   "evt_cancel_" + uuid.NewString(),
				"type": "customer.subscription.deleted",
				"data": map[string]any{
					"object": map[string]any{
						"id":                   subscriptionID,
						"customer":             customerID,
						"status":               "canceled",
						"current_period_start": 1_600_000_000,
						"current_period_end":   1_600_003_600,
						"cancel_at_period_end": true,
						"metadata": map[string]any{
							"org_id": orgID,
						},
						"items": map[string]any{
							"data": []map[string]any{
								{
									"price": map[string]any{
										"lookup_key": "pro",
										"id":         "price_pro",
									},
								},
							},
						},
					},
				},
			})

			status, resp = h.callTool(t, token, session, "list_threads", map[string]any{
				"inbox_id": inboxID,
				"limit":    1,
			})
			if status != http.StatusOK {
				t.Fatalf("expected rpc error status after cancellation, got %d", status)
			}
			if resp.Error == nil || resp.Error.Code != -32041 || resp.Error.Message != "subscription_inactive" {
				t.Fatalf("expected subscription_inactive error after cancellation, got %#v", resp.Error)
			}

			var statusValue string
			err := h.store.DB().QueryRowContext(h.ctx, `SELECT subscription_status FROM org_entitlements WHERE org_id = $1`, orgID).Scan(&statusValue)
			if err != nil {
				t.Fatalf("read org entitlement status: %v", err)
			}
			if statusValue != "canceled" {
				t.Fatalf("expected org entitlement status=canceled, got %s", statusValue)
			}
		})

		t.Run("TokenRotationRevokesOldTokenImmediately", func(t *testing.T) {
			orgID := h.createOrg(t, "rotate-org")
			h.upsertActiveEntitlement(t, orgID, 1000, 1000)
			inboxID, _, _ := h.seedInboxThreadMessage(t, orgID, "rotate@local.neuralmail", "Rotation", "rotation test")

			v1 := h.issueServiceToken(t, orgID, []string{"nerve:email.read"}, false)
			sessionV1 := h.initializeSession(t, v1)
			status, resp := h.callTool(t, v1, sessionV1, "list_threads", map[string]any{
				"inbox_id": inboxID,
				"limit":    1,
			})
			if status != http.StatusOK || resp.Error != nil {
				t.Fatalf("expected v1 token to work before rotation, status=%d err=%+v", status, resp.Error)
			}

			v2 := h.issueServiceToken(t, orgID, []string{"nerve:email.read"}, true)
			sessionV2 := h.initializeSession(t, v2)
			status, resp = h.callTool(t, v2, sessionV2, "list_threads", map[string]any{
				"inbox_id": inboxID,
				"limit":    1,
			})
			if status != http.StatusOK || resp.Error != nil {
				t.Fatalf("expected rotated token v2 to work immediately, status=%d err=%+v", status, resp.Error)
			}

			initStatus, _, _, body := h.mcpRequest(t, v1, "", map[string]any{
				"jsonrpc": "2.0",
				"id":      99,
				"method":  "initialize",
				"params":  map[string]any{},
			})
			if initStatus != http.StatusUnauthorized {
				t.Fatalf("expected revoked v1 token to return 401, got status=%d body=%s", initStatus, string(body))
			}
		})

		t.Run("IngestionLagUnderThirtySeconds", func(t *testing.T) {
			orgID := h.createOrg(t, "lag-org")
			h.upsertActiveEntitlement(t, orgID, 6000, 20000)
			inboxID := h.createInbox(t, orgID, "lag@local.neuralmail")

			token := h.issueServiceToken(t, orgID, []string{"nerve:email.read", "nerve:email.search"}, false)
			session := h.initializeSession(t, token)

			marker := "lag-marker-" + uuid.NewString()
			start := time.Now()
			insertErrCh := make(chan error, 1)

			go func() {
				time.Sleep(350 * time.Millisecond)
				_, _, err := h.insertThreadAndMessageInInbox(inboxID, "Lag Probe", "Ingestion benchmark "+marker)
				insertErrCh <- err
			}()

			deadline := start.Add(30 * time.Second)
			for time.Now().Before(deadline) {
				select {
				case err := <-insertErrCh:
					if err != nil {
						t.Fatalf("insert lag probe message: %v", err)
					}
					insertErrCh = nil
				default:
				}
				status, resp := h.callTool(t, token, session, "search_inbox", map[string]any{
					"inbox_id": inboxID,
					"query":    marker,
					"top_k":    3,
				})
				if status != http.StatusOK {
					t.Fatalf("expected search status 200 while polling lag, got %d", status)
				}
				if resp.Error != nil {
					t.Fatalf("expected polling search to avoid rpc errors, got %+v", resp.Error)
				}
				var payload struct {
					Results []struct {
						MessageID string `json:"message_id"`
						Snippet   string `json:"snippet"`
					} `json:"results"`
				}
				decodeRawResult(t, resp.Result, &payload)
				if len(payload.Results) > 0 {
					elapsed := time.Since(start)
					if elapsed > 30*time.Second {
						t.Fatalf("ingestion/search lag exceeded SLO: %s", elapsed)
					}
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatalf("message was not searchable within 30 seconds")
		})

		t.Run("StubbornAgentLoopRateLimited", func(t *testing.T) {
			orgID := h.createOrg(t, "stubborn-agent-org")
			h.upsertActiveEntitlement(t, orgID, 1, 1000)
			_, threadID, _ := h.seedInboxThreadMessage(t, orgID, "loop@local.neuralmail", "Dispute", "please refund $500")

			token := h.issueServiceToken(t, orgID, []string{"nerve:email.read", "nerve:email.draft"}, false)
			session := h.initializeSession(t, token)

			status, resp := h.callTool(t, token, session, "draft_reply_with_policy", map[string]any{
				"thread_id": threadID,
				"goal":      "refund",
			})
			if status != http.StatusOK || resp.Error != nil {
				t.Fatalf("expected first draft attempt to be policy-handled, status=%d err=%+v", status, resp.Error)
			}

			status, resp = h.callTool(t, token, session, "draft_reply_with_policy", map[string]any{
				"thread_id": threadID,
				"goal":      "refund",
			})
			if status != http.StatusOK {
				t.Fatalf("expected rpc response status for rate limiting, got %d", status)
			}
			if resp.Error == nil || resp.Error.Code != -32042 || resp.Error.Message != "rate_limited" {
				t.Fatalf("expected rate_limited rpc error on stubborn retry, got %#v", resp.Error)
			}
		})
	})
}

func newCloudE2EHarness(t *testing.T, ctx context.Context, st *store.Store) *cloudE2EHarness {
	t.Helper()

	cfg := config.Default()
	cfg.Dev.Mode = true
	cfg.Cloud.Mode = true
	cfg.Security.APIKey = bootstrapAdminAPIKey
	cfg.Security.TokenSigningKey = e2eTokenSigningKey
	cfg.Billing.StripeWebhookSecret = e2eStripeWebhookSecret

	authSvc := auth.NewService(cfg, st)
	billingSvc := billing.NewStripeService(cfg, st)
	tokenSvc := NewTokenService(st, e2eTokenSigningKey)

	controlHandler := NewHandler(cfg, st, authSvc, billingSvc, tokenSvc)
	controlMux := http.NewServeMux()
	controlHandler.RegisterRoutes(controlMux)
	controlPlane := httptest.NewServer(controlMux)

	observer := observability.NewEntitlementObserver(log.New(io.Discard, "", 0))
	entitlementSvc := entitlements.NewService(cfg, st, observer)
	pol := policy.Policy{
		ForbiddenPhrases: []string{"processed your refund of $500 immediately"},
	}
	toolSvc := tools.NewService(cfg, st, &fixedDraftLLM{draftText: "I have processed your refund of $500 immediately."}, nil, pol, nil)
	mcpServer := mcp.NewServer(cfg, toolSvc, authSvc, entitlementSvc)
	mcpMux := http.NewServeMux()
	mcpMux.HandleFunc("/mcp", mcpServer.HandleHTTP)
	mcpHTTP := httptest.NewServer(mcpMux)

	return &cloudE2EHarness{
		ctx:          ctx,
		store:        st,
		controlPlane: controlPlane,
		mcp:          mcpHTTP,
	}
}

func (h *cloudE2EHarness) Close() {
	if h.controlPlane != nil {
		h.controlPlane.Close()
	}
	if h.mcp != nil {
		h.mcp.Close()
	}
}

func (h *cloudE2EHarness) createOrg(t *testing.T, name string) string {
	t.Helper()
	orgID, err := h.store.CreateOrg(h.ctx, name)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return orgID
}

func (h *cloudE2EHarness) upsertPlan(t *testing.T, code string, rpm int, monthlyUnits int64, maxInboxes int) {
	t.Helper()
	_, err := h.store.DB().ExecContext(h.ctx, `
		INSERT INTO plan_entitlements (plan_code, mcp_rpm, monthly_units, max_inboxes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (plan_code)
		DO UPDATE SET mcp_rpm = EXCLUDED.mcp_rpm, monthly_units = EXCLUDED.monthly_units, max_inboxes = EXCLUDED.max_inboxes
	`, code, rpm, monthlyUnits, maxInboxes)
	if err != nil {
		t.Fatalf("upsert plan entitlement: %v", err)
	}
}

func (h *cloudE2EHarness) upsertActiveEntitlement(t *testing.T, orgID string, rpm int, monthlyUnits int64) {
	t.Helper()
	periodStart := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	periodEnd := periodStart.Add(30 * 24 * time.Hour)
	err := h.store.UpsertOrgEntitlement(h.ctx, store.OrgEntitlement{
		OrgID:              orgID,
		PlanCode:           "pro",
		SubscriptionStatus: "active",
		MCPRPM:             rpm,
		MonthlyUnits:       monthlyUnits,
		MaxInboxes:         10,
		UsagePeriodStart:   periodStart,
		UsagePeriodEnd:     periodEnd,
	})
	if err != nil {
		t.Fatalf("upsert active entitlement: %v", err)
	}
	if err := h.store.EnsureOrgUsageCounter(h.ctx, orgID, "mcp_units", periodStart, periodEnd); err != nil {
		t.Fatalf("ensure usage counter: %v", err)
	}
}

func (h *cloudE2EHarness) createInbox(t *testing.T, orgID string, address string) string {
	t.Helper()
	inboxID := uuid.NewString()
	_, err := h.store.DB().ExecContext(h.ctx, `
		INSERT INTO inboxes (id, org_id, address, status)
		VALUES ($1, $2, $3, 'active')
	`, inboxID, orgID, address)
	if err != nil {
		t.Fatalf("create inbox: %v", err)
	}
	return inboxID
}

func (h *cloudE2EHarness) seedInboxThreadMessage(t *testing.T, orgID string, address string, subject string, body string) (string, string, string) {
	t.Helper()
	inboxID := h.createInbox(t, orgID, address)
	return h.seedThreadAndMessageInInbox(t, inboxID, subject, body)
}

func (h *cloudE2EHarness) seedThreadAndMessageInInbox(t *testing.T, inboxID string, subject string, body string) (string, string, string) {
	t.Helper()
	threadID, msgID, err := h.insertThreadAndMessageInInbox(inboxID, subject, body)
	if err != nil {
		t.Fatalf("insert thread/message in inbox: %v", err)
	}
	return inboxID, threadID, msgID
}

func (h *cloudE2EHarness) insertThreadAndMessageInInbox(inboxID string, subject string, body string) (string, string, error) {
	threadID, err := h.store.EnsureThread(h.ctx, inboxID, "provider-"+uuid.NewString(), subject, []store.Participant{
		{Name: "Alice", Email: "alice@example.test"},
	})
	if err != nil {
		return "", "", err
	}
	msgID, err := h.store.InsertMessage(h.ctx, store.Message{
		InboxID:           inboxID,
		ThreadID:          threadID,
		Direction:         "inbound",
		Subject:           subject,
		Text:              body,
		CreatedAt:         time.Now().UTC(),
		ProviderMessageID: "provider-msg-" + uuid.NewString(),
		From:              store.Participant{Name: "Alice", Email: "alice@example.test"},
		To:                []store.Participant{{Name: "Support", Email: "support@example.test"}},
		CC:                nil,
	})
	if err != nil {
		return "", "", err
	}
	return threadID, msgID, nil
}

func (h *cloudE2EHarness) issueServiceToken(t *testing.T, orgID string, scopes []string, rotate bool) string {
	t.Helper()
	body := map[string]any{
		"org_id":      orgID,
		"scopes":      scopes,
		"ttl_seconds": 900,
		"rotate":      rotate,
	}
	status, raw := h.doJSONRequest(t, http.MethodPost, h.controlPlane.URL+"/v1/tokens/service", bootstrapAdminAPIKey, body, "")
	if status != http.StatusOK {
		t.Fatalf("issue service token failed status=%d body=%s", status, string(raw))
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if payload.Token == "" {
		t.Fatalf("service token response missing token")
	}
	return payload.Token
}

func (h *cloudE2EHarness) postStripeSubscriptionEvent(t *testing.T, event map[string]any) {
	t.Helper()
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal stripe event: %v", err)
	}
	timestamp := time.Now().Unix()
	mac := hmac.New(sha256.New, []byte(e2eStripeWebhookSecret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", timestamp, string(payload))))
	signature := hex.EncodeToString(mac.Sum(nil))
	sigHeader := fmt.Sprintf("t=%d,v1=%s", timestamp, signature)

	req, err := http.NewRequest(http.MethodPost, h.controlPlane.URL+"/v1/billing/webhook/stripe", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create stripe webhook request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", sigHeader)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("execute stripe webhook: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read stripe webhook response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stripe webhook failed status=%d body=%s", resp.StatusCode, string(raw))
	}
}

func (h *cloudE2EHarness) initializeSession(t *testing.T, token string) string {
	t.Helper()
	status, sessionID, resp, body := h.mcpRequest(t, token, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	if status != http.StatusOK {
		t.Fatalf("mcp initialize failed status=%d body=%s", status, string(body))
	}
	if resp.Error != nil {
		t.Fatalf("mcp initialize rpc error: %+v", resp.Error)
	}
	if strings.TrimSpace(sessionID) == "" {
		t.Fatalf("initialize response missing MCP-Session-Id")
	}
	return sessionID
}

func (h *cloudE2EHarness) readResource(t *testing.T, token string, sessionID string, uri string) (int, rpcResponse) {
	t.Helper()
	status, _, resp, body := h.mcpRequest(t, token, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "resources/read",
		"params": map[string]any{
			"uri": uri,
		},
	})
	if status == http.StatusOK && len(body) == 0 {
		t.Fatalf("expected rpc body for resources/read")
	}
	return status, resp
}

func (h *cloudE2EHarness) callTool(t *testing.T, token string, sessionID string, name string, arguments map[string]any) (int, rpcResponse) {
	t.Helper()
	status, _, resp, body := h.mcpRequest(t, token, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if status == http.StatusOK && len(body) == 0 {
		t.Fatalf("expected rpc body for tools/call")
	}
	return status, resp
}

func (h *cloudE2EHarness) currentUsage(t *testing.T, orgID string) int64 {
	t.Helper()
	ent, err := h.store.GetOrgEntitlement(h.ctx, orgID)
	if err != nil {
		t.Fatalf("get org entitlement: %v", err)
	}
	used, err := h.store.GetOrgUsageCounterUsed(h.ctx, orgID, "mcp_units", ent.UsagePeriodStart)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0
		}
		t.Fatalf("get usage counter: %v", err)
	}
	return used
}

func (h *cloudE2EHarness) doJSONRequest(t *testing.T, method string, target string, apiKey string, payload any, bearerToken string) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal request payload: %v", err)
	}
	req, err := http.NewRequest(method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("execute request: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, raw
}

func (h *cloudE2EHarness) mcpRequest(t *testing.T, token string, sessionID string, payload map[string]any) (int, string, rpcResponse, []byte) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal mcp payload: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, h.mcp.URL+"/mcp", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create mcp request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	if sessionID != "" {
		req.Header.Set("MCP-Session-Id", sessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("execute mcp request: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read mcp response body: %v", err)
	}
	var parsed rpcResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &parsed); err != nil && resp.StatusCode == http.StatusOK {
			t.Fatalf("decode mcp rpc response: %v body=%s", err, string(raw))
		}
	}
	return resp.StatusCode, resp.Header.Get("MCP-Session-Id"), parsed, raw
}

func decodeRawResult(t *testing.T, raw json.RawMessage, out any) {
	t.Helper()
	if len(raw) == 0 {
		t.Fatalf("missing result payload")
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode rpc result: %v raw=%s", err, string(raw))
	}
}
