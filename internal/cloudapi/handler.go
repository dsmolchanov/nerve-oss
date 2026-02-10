package cloudapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

var ErrMaxInboxesExceeded = errors.New("max inboxes exceeded")

type BillingWebhookProcessor interface {
	ProcessWebhook(ctx context.Context, payload []byte, signatureHeader string) error
}

type Handler struct {
	Config config.Config
	Store  *store.Store
	Auth   *auth.Service

	Billing BillingWebhookProcessor
	Tokens  ServiceTokenIssuer
}

func NewHandler(cfg config.Config, st *store.Store, authSvc *auth.Service, billingSvc BillingWebhookProcessor, tokenSvc ServiceTokenIssuer) *Handler {
	return &Handler{
		Config:  cfg,
		Store:   st,
		Auth:    authSvc,
		Billing: billingSvc,
		Tokens:  tokenSvc,
	}
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/orgs", h.handleCreateOrg)
	mux.HandleFunc("/v1/subscriptions/checkout", h.handleCheckout)
	mux.HandleFunc("/v1/billing/webhook/stripe", h.handleStripeWebhook)
	mux.HandleFunc("/v1/subscriptions/current", h.handleCurrentSubscription)
	mux.HandleFunc("/v1/tokens/service", h.handleIssueServiceToken)
	mux.HandleFunc("/v1/billing/portal", h.handleBillingPortal)
}

func (h *Handler) EnforceInboxLimit(ctx context.Context, orgID string) error {
	if h == nil || h.Store == nil || orgID == "" {
		return nil
	}
	ent, err := h.Store.GetOrgEntitlement(ctx, orgID)
	if err != nil {
		return err
	}
	if ent.MaxInboxes <= 0 {
		return nil
	}
	count, err := h.Store.CountInboxesByOrg(ctx, orgID)
	if err != nil {
		return err
	}
	if count >= ent.MaxInboxes {
		return ErrMaxInboxesExceeded
	}
	return nil
}

func (h *Handler) handleCreateOrg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, err := h.requireBillingAdmin(r); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	orgID, err := h.Store.CreateOrg(r.Context(), strings.TrimSpace(req.Name))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"org_id": orgID})
}

func (h *Handler) handleCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, err := h.requireBillingAdmin(r); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		OrgID string `json:"org_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.OrgID = strings.TrimSpace(req.OrgID)
	if req.OrgID == "" {
		http.Error(w, "missing org_id", http.StatusBadRequest)
		return
	}

	checkoutURL := fmt.Sprintf("https://checkout.stripe.com/pay/mock?client_reference_id=%s", req.OrgID)
	writeJSON(w, http.StatusOK, map[string]any{
		"checkout_url":        checkoutURL,
		"client_reference_id": req.OrgID,
	})
}

func (h *Handler) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if h.Billing == nil {
		http.Error(w, "billing not configured", http.StatusInternalServerError)
		return
	}
	payload, err := ioReadAll(r)
	if err != nil {
		http.Error(w, "failed to read payload", http.StatusBadRequest)
		return
	}
	if err := h.Billing.ProcessWebhook(r.Context(), payload, r.Header.Get("Stripe-Signature")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (h *Handler) handleCurrentSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	principal, err := h.authenticatePrincipal(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	orgID := principal.OrgID
	if qp := strings.TrimSpace(r.URL.Query().Get("org_id")); qp != "" && principal.AuthMethod == "bootstrap_key" {
		orgID = qp
	}
	if orgID == "" {
		http.Error(w, "missing org_id", http.StatusBadRequest)
		return
	}

	summary, err := h.Store.GetSubscriptionSummaryByOrg(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "subscription not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (h *Handler) handleIssueServiceToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if h.Tokens == nil {
		http.Error(w, "token issuer not configured", http.StatusInternalServerError)
		return
	}

	var req struct {
		OrgID      string   `json:"org_id"`
		Scopes     []string `json:"scopes"`
		TTLSeconds int      `json:"ttl_seconds"`
		Rotate     bool     `json:"rotate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.OrgID = strings.TrimSpace(req.OrgID)
	if req.OrgID == "" {
		http.Error(w, "missing org_id", http.StatusBadRequest)
		return
	}
	if len(req.Scopes) == 0 {
		http.Error(w, "missing scopes", http.StatusBadRequest)
		return
	}
	for _, scope := range req.Scopes {
		if !allowedServiceScope(scope) {
			http.Error(w, "invalid scope", http.StatusBadRequest)
			return
		}
	}

	ttl := 15 * time.Minute
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	if ttl > time.Hour {
		http.Error(w, "ttl too large", http.StatusBadRequest)
		return
	}

	issued, err := h.Tokens.IssueServiceToken(r.Context(), req.OrgID, principal.ActorID, req.Scopes, ttl, req.Rotate)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, issued)
}

func (h *Handler) handleBillingPortal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if _, err := h.requireBillingAdmin(r); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	var req struct {
		OrgID string `json:"org_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	req.OrgID = strings.TrimSpace(req.OrgID)
	if req.OrgID == "" {
		http.Error(w, "missing org_id", http.StatusBadRequest)
		return
	}
	portalURL := fmt.Sprintf("https://billing.stripe.com/p/session/mock?org_id=%s", req.OrgID)
	writeJSON(w, http.StatusOK, map[string]any{"url": portalURL})
}

func (h *Handler) requireBillingAdmin(r *http.Request) (auth.Principal, error) {
	principal, err := h.authenticatePrincipal(r)
	if err != nil {
		return auth.Principal{}, err
	}
	if h.Auth == nil {
		return auth.Principal{}, errors.New("auth service not configured")
	}
	if err := h.Auth.ValidateScopes(principal, "nerve:admin.billing"); err != nil {
		return auth.Principal{}, err
	}
	return principal, nil
}

func (h *Handler) authenticatePrincipal(r *http.Request) (auth.Principal, error) {
	if bootstrap := strings.TrimSpace(h.Config.Security.APIKey); bootstrap != "" {
		if strings.TrimSpace(r.Header.Get("X-API-Key")) == bootstrap {
			return auth.Principal{
				ActorID:    "bootstrap_admin",
				Scopes:     []string{"nerve:admin.billing"},
				AuthMethod: "bootstrap_key",
			}, nil
		}
	}
	if h.Auth == nil {
		return auth.Principal{}, errors.New("auth service not configured")
	}
	return h.Auth.AuthenticateRequest(r)
}

func allowedServiceScope(scope string) bool {
	switch scope {
	case "nerve:email.read", "nerve:email.search", "nerve:email.draft", "nerve:email.send", "nerve:admin.billing":
		return true
	default:
		return false
	}
}

func ioReadAll(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	return io.ReadAll(r.Body)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
