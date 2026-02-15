package cloudapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"neuralmail/internal/auth"
	"neuralmail/internal/billing"
	"neuralmail/internal/config"
	"neuralmail/internal/domains"
	"neuralmail/internal/emailaddr"
	"neuralmail/internal/store"
)

var ErrMaxInboxesExceeded = errors.New("max inboxes exceeded")
var ErrMaxDomainsExceeded = errors.New("max domains exceeded")

type BillingWebhookProcessor interface {
	ProcessWebhook(ctx context.Context, payload []byte, signatureHeader string) error
}

type BillingCheckoutProvider interface {
	CreateCheckoutSession(ctx context.Context, orgID, successURL, cancelURL string) (*billingCheckoutResult, error)
	CreateBillingPortalSession(ctx context.Context, orgID string) (*billingPortalResult, error)
}

type billingCheckoutResult = billing.CheckoutResult
type billingPortalResult = billing.PortalResult

type Handler struct {
	Config config.Config
	Store  *store.Store
	Auth   *auth.Service

	Billing  BillingWebhookProcessor
	Checkout BillingCheckoutProvider
	Tokens   ServiceTokenIssuer
	Domains  *domains.Verifier
}

func NewHandler(cfg config.Config, st *store.Store, authSvc *auth.Service, billingSvc BillingWebhookProcessor, tokenSvc ServiceTokenIssuer) *Handler {
	h := &Handler{
		Config:  cfg,
		Store:   st,
		Auth:    authSvc,
		Billing: billingSvc,
		Tokens:  tokenSvc,
		Domains: domains.NewVerifier(nil),
	}
	// If the billing service also implements checkout/portal, wire it up.
	if cp, ok := billingSvc.(BillingCheckoutProvider); ok {
		h.Checkout = cp
	}
	return h
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/orgs", h.handleCreateOrg)
	mux.HandleFunc("/v1/orgs/runtime", h.handleOrgRuntime)
	mux.HandleFunc("/v1/subscriptions/checkout", h.handleCheckout)
	mux.HandleFunc("/v1/billing/webhook/stripe", h.handleStripeWebhook)
	mux.HandleFunc("/v1/subscriptions/current", h.handleCurrentSubscription)
	mux.HandleFunc("/v1/tokens/service", h.handleIssueServiceToken)
	mux.HandleFunc("/v1/keys", h.handleCloudAPIKeys)
	mux.HandleFunc("/v1/keys/", h.handleCloudAPIKeyByID)
	mux.HandleFunc("/v1/domains", h.handleDomains)
	mux.HandleFunc("/v1/domains/", h.handleDomainByID)
	mux.HandleFunc("/v1/domains/verify", h.handleVerifyDomain)
	mux.HandleFunc("/v1/domains/dns", h.handleDomainDNS)
	mux.HandleFunc("/v1/inboxes", h.handleInboxes)
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

func (h *Handler) handleOrgRuntime(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.handleGetOrgRuntime(w, r)
	case http.MethodPut:
		h.handleSetOrgRuntime(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleGetOrgRuntime(w http.ResponseWriter, r *http.Request) {
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(r.URL.Query().Get("org_id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	endpoint, err := h.Store.GetOrgMCPEndpoint(r.Context(), orgID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "org not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"org_id":       orgID,
		"mcp_endpoint": endpoint,
	})
}

func (h *Handler) handleSetOrgRuntime(w http.ResponseWriter, r *http.Request) {
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		OrgID       string `json:"org_id"`
		MCPEndpoint string `json:"mcp_endpoint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(req.OrgID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	normalized, err := normalizeMCPEndpoint(req.MCPEndpoint)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	stored, err := h.Store.SetOrgMCPEndpoint(r.Context(), orgID, normalized)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "org not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"org_id":       orgID,
		"mcp_endpoint": stored,
	})
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

	if h.Checkout == nil {
		// Fallback mock for tests
		checkoutURL := fmt.Sprintf("https://checkout.stripe.com/pay/mock?client_reference_id=%s", req.OrgID)
		writeJSON(w, http.StatusOK, map[string]any{
			"checkout_url":        checkoutURL,
			"client_reference_id": req.OrgID,
		})
		return
	}

	result, err := h.Checkout.CreateCheckoutSession(r.Context(), req.OrgID, "", "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checkout_url":        result.CheckoutURL,
		"client_reference_id": result.ClientReferenceID,
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

type cloudAPIKeyResponse struct {
	ID        string     `json:"id"`
	Key       string     `json:"key,omitempty"`
	KeyPrefix string     `json:"key_prefix"`
	Label     string     `json:"label"`
	Scopes    []string   `json:"scopes"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type orgDomainResponse struct {
	ID                string            `json:"id"`
	Domain            string            `json:"domain"`
	Status            string            `json:"status"`
	VerificationToken string            `json:"verification_token,omitempty"`
	DNSRecords        []domains.DNSRecord `json:"dns_records,omitempty"`
	MXVerified        bool              `json:"mx_verified"`
	SPFVerified       bool              `json:"spf_verified"`
	DKIMVerified      bool              `json:"dkim_verified"`
	DMARCVerified     bool              `json:"dmarc_verified"`
	InboundEnabled    bool              `json:"inbound_enabled"`
	DKIMSelector      string            `json:"dkim_selector"`
	DKIMMethod        string            `json:"dkim_method"`
	LastCheckAt       *time.Time        `json:"last_check_at,omitempty"`
	VerifiedAt        *time.Time        `json:"verified_at,omitempty"`
	ExpiresAt         *time.Time        `json:"expires_at,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

type domainVerifyResponse struct {
	Domain orgDomainResponse `json:"domain"`
	Checks map[string]any    `json:"checks"`
}

type inboxResponse struct {
	ID          string    `json:"id"`
	Address     string    `json:"address"`
	Status      string    `json:"status"`
	OrgDomainID *string   `json:"org_domain_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

func (h *Handler) handleCloudAPIKeys(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handleCreateCloudAPIKey(w, r)
	case http.MethodGet:
		h.handleListCloudAPIKeys(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleCloudAPIKeyByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	keyID := strings.TrimPrefix(r.URL.Path, "/v1/keys/")
	if keyID == "" || strings.Contains(keyID, "/") {
		http.Error(w, "missing key id", http.StatusBadRequest)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(r.URL.Query().Get("org_id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	revoked, err := h.Store.RevokeCloudAPIKey(r.Context(), orgID, keyID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !revoked {
		http.Error(w, "key not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "revoked"})
}

func (h *Handler) handleCreateCloudAPIKey(w http.ResponseWriter, r *http.Request) {
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		OrgID  string   `json:"org_id"`
		Label  string   `json:"label"`
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(req.OrgID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Scopes) == 0 {
		http.Error(w, "missing scopes", http.StatusBadRequest)
		return
	}
	for _, scope := range req.Scopes {
		if !allowedCloudKeyScope(scope) {
			http.Error(w, "invalid scope", http.StatusBadRequest)
			return
		}
	}

	rawKey, keyPrefix, keyHash, err := generateCloudAPIKeyMaterial()
	if err != nil {
		http.Error(w, "failed to generate key", http.StatusInternalServerError)
		return
	}

	record, err := h.Store.CreateCloudAPIKey(
		r.Context(),
		orgID,
		keyPrefix,
		keyHash,
		strings.TrimSpace(req.Label),
		req.Scopes,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := cloudAPIKeyResponse{
		ID:        record.ID,
		Key:       rawKey,
		KeyPrefix: record.KeyPrefix,
		Label:     record.Label,
		Scopes:    record.Scopes,
		CreatedAt: record.CreatedAt,
	}
	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) handleListCloudAPIKeys(w http.ResponseWriter, r *http.Request) {
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(r.URL.Query().Get("org_id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	keys, err := h.Store.ListCloudAPIKeys(r.Context(), orgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	response := make([]cloudAPIKeyResponse, 0, len(keys))
	for _, key := range keys {
		item := cloudAPIKeyResponse{
			ID:        key.ID,
			KeyPrefix: key.KeyPrefix,
			Label:     key.Label,
			Scopes:    key.Scopes,
			CreatedAt: key.CreatedAt,
		}
		if key.RevokedAt.Valid {
			revokedAt := key.RevokedAt.Time
			item.RevokedAt = &revokedAt
		}
		response = append(response, item)
	}

	writeJSON(w, http.StatusOK, map[string]any{"keys": response})
}

func (h *Handler) EnforceDomainLimit(ctx context.Context, orgID string) error {
	if h == nil || h.Store == nil || orgID == "" {
		return nil
	}
	ent, err := h.Store.GetOrgEntitlement(ctx, orgID)
	if err != nil {
		return err
	}
	if ent.MaxDomains <= 0 {
		return nil
	}
	count, err := h.Store.CountDomainsByOrg(ctx, orgID)
	if err != nil {
		return err
	}
	if count >= ent.MaxDomains {
		return ErrMaxDomainsExceeded
	}
	return nil
}

func (h *Handler) handleDomains(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handleCreateDomain(w, r)
	case http.MethodGet:
		h.handleListDomains(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleDomainByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	domainID := strings.TrimPrefix(r.URL.Path, "/v1/domains/")
	if domainID == "" || strings.Contains(domainID, "/") {
		http.Error(w, "missing domain id", http.StatusBadRequest)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(r.URL.Query().Get("org_id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	deleted, err := h.Store.DeleteOrgDomainForOrg(r.Context(), orgID, domainID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !deleted {
		http.Error(w, "domain not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

func (h *Handler) handleCreateDomain(w http.ResponseWriter, r *http.Request) {
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		OrgID      string `json:"org_id"`
		Domain     string `json:"domain"`
		DKIMMethod string `json:"dkim_method,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(req.OrgID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	canonical, err := domains.CanonicalizeDomain(req.Domain)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.EnforceDomainLimit(r.Context(), orgID); err != nil {
		if errors.Is(err, ErrMaxDomainsExceeded) {
			http.Error(w, "max domains exceeded", http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, _ = h.Store.ExpirePendingDomains(r.Context())

	verificationToken, err := generateDomainVerificationToken()
	if err != nil {
		http.Error(w, "failed to generate verification token", http.StatusInternalServerError)
		return
	}

	dkimMethod := strings.TrimSpace(req.DKIMMethod)
	if dkimMethod == "" {
		dkimMethod = "cname"
	}
	if dkimMethod != "cname" && dkimMethod != "txt" {
		http.Error(w, "invalid dkim_method", http.StatusBadRequest)
		return
	}

	domainID, err := h.Store.CreateOrgDomain(
		r.Context(),
		orgID,
		canonical,
		verificationToken,
		"nerve",
		"",
		"",
		dkimMethod,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	created, err := h.Store.GetOrgDomainByIDForOrg(r.Context(), orgID, domainID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := orgDomainResponse{
		ID:                created.ID,
		Domain:            created.Domain,
		Status:            created.Status,
		VerificationToken: created.VerificationToken,
		DNSRecords:        domains.DNSInstructions(created.VerificationToken),
		MXVerified:        created.MXVerified,
		SPFVerified:       created.SPFVerified,
		DKIMVerified:      created.DKIMVerified,
		DMARCVerified:     created.DMARCVerified,
		InboundEnabled:    created.InboundEnabled,
		DKIMSelector:      created.DKIMSelector,
		DKIMMethod:        created.DKIMMethod,
		CreatedAt:         created.CreatedAt,
		UpdatedAt:         created.UpdatedAt,
	}
	if created.LastCheckAt.Valid {
		tm := created.LastCheckAt.Time
		resp.LastCheckAt = &tm
	}
	if created.VerifiedAt.Valid {
		tm := created.VerifiedAt.Time
		resp.VerifiedAt = &tm
	}
	if created.ExpiresAt.Valid {
		tm := created.ExpiresAt.Time
		resp.ExpiresAt = &tm
	}

	writeJSON(w, http.StatusOK, map[string]any{"domain": resp})
}

func (h *Handler) handleListDomains(w http.ResponseWriter, r *http.Request) {
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(r.URL.Query().Get("org_id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	items, err := h.Store.ListOrgDomains(r.Context(), orgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := make([]orgDomainResponse, 0, len(items))
	for _, item := range items {
		out := orgDomainResponse{
			ID:             item.ID,
			Domain:         item.Domain,
			Status:         item.Status,
			MXVerified:     item.MXVerified,
			SPFVerified:    item.SPFVerified,
			DKIMVerified:   item.DKIMVerified,
			DMARCVerified:  item.DMARCVerified,
			InboundEnabled: item.InboundEnabled,
			DKIMSelector:   item.DKIMSelector,
			DKIMMethod:     item.DKIMMethod,
			CreatedAt:      item.CreatedAt,
			UpdatedAt:      item.UpdatedAt,
		}
		if item.LastCheckAt.Valid {
			tm := item.LastCheckAt.Time
			out.LastCheckAt = &tm
		}
		if item.VerifiedAt.Valid {
			tm := item.VerifiedAt.Time
			out.VerifiedAt = &tm
		}
		if item.ExpiresAt.Valid {
			tm := item.ExpiresAt.Time
			out.ExpiresAt = &tm
		}
		resp = append(resp, out)
	}

	writeJSON(w, http.StatusOK, map[string]any{"domains": resp})
}

func (h *Handler) handleDomainDNS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(r.URL.Query().Get("org_id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	domainID := strings.TrimSpace(r.URL.Query().Get("domain_id"))
	if domainID == "" {
		http.Error(w, "missing domain_id", http.StatusBadRequest)
		return
	}

	d, err := h.Store.GetOrgDomainByIDForOrg(r.Context(), orgID, domainID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "domain not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"domain_id":   d.ID,
		"domain":      d.Domain,
		"dns_records": domains.DNSInstructions(d.VerificationToken),
	})
}

func (h *Handler) handleVerifyDomain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		OrgID    string `json:"org_id"`
		DomainID string `json:"domain_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(req.OrgID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	domainID := strings.TrimSpace(req.DomainID)
	if domainID == "" {
		http.Error(w, "missing domain_id", http.StatusBadRequest)
		return
	}

	d, err := h.Store.GetOrgDomainByIDForOrg(r.Context(), orgID, domainID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "domain not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if h.Domains == nil {
		h.Domains = domains.NewVerifier(nil)
	}

	result := h.Domains.VerifyOwnership(r.Context(), d.Domain, d.VerificationToken)
	status := d.Status
	if result.Verified {
		status = "active"
	}

	if err := h.Store.UpdateOrgDomainVerification(r.Context(), d.ID, false, false, false, false, status); err != nil {
		if isUniqueViolation(err) {
			http.Error(w, "domain already verified by another org", http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	updated, err := h.Store.GetOrgDomainByIDForOrg(r.Context(), orgID, d.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := orgDomainResponse{
		ID:             updated.ID,
		Domain:         updated.Domain,
		Status:         updated.Status,
		MXVerified:     updated.MXVerified,
		SPFVerified:    updated.SPFVerified,
		DKIMVerified:   updated.DKIMVerified,
		DMARCVerified:  updated.DMARCVerified,
		InboundEnabled: updated.InboundEnabled,
		DKIMSelector:   updated.DKIMSelector,
		DKIMMethod:     updated.DKIMMethod,
		CreatedAt:      updated.CreatedAt,
		UpdatedAt:      updated.UpdatedAt,
	}
	if updated.LastCheckAt.Valid {
		tm := updated.LastCheckAt.Time
		out.LastCheckAt = &tm
	}
	if updated.VerifiedAt.Valid {
		tm := updated.VerifiedAt.Time
		out.VerifiedAt = &tm
	}
	if updated.ExpiresAt.Valid {
		tm := updated.ExpiresAt.Time
		out.ExpiresAt = &tm
	}

	writeJSON(w, http.StatusOK, domainVerifyResponse{
		Domain: out,
		Checks: map[string]any{
			"ownership_verified": result.Verified,
			"details":            result.Details,
		},
	})
}

func (h *Handler) handleInboxes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handleCreateInbox(w, r)
	case http.MethodGet:
		h.handleListInboxes(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *Handler) handleCreateInbox(w http.ResponseWriter, r *http.Request) {
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		OrgID   string `json:"org_id"`
		Address string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(req.OrgID))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	canonical, _, domainPart, err := emailaddr.Canonicalize(req.Address)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := h.EnforceInboxLimit(r.Context(), orgID); err != nil {
		if errors.Is(err, ErrMaxInboxesExceeded) {
			http.Error(w, "max inboxes exceeded", http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if existing, err := h.Store.GetInboxByAddress(r.Context(), canonical); err == nil && existing.ID != "" {
		http.Error(w, "inbox already exists", http.StatusConflict)
		return
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	orgDomainID := ""
	if h.Config.Cloud.Mode {
		d, err := h.Store.GetOrgDomainForSending(r.Context(), domainPart)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "domain not verified", http.StatusBadRequest)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if d.OrgID != orgID {
			// Don't leak domain ownership information.
			http.Error(w, "domain not verified", http.StatusBadRequest)
			return
		}
		orgDomainID = d.ID
	}

	created, err := h.Store.CreateInboxForOrg(r.Context(), orgID, canonical, orgDomainID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var domainID *string
	if created.OrgDomainID.Valid {
		v := created.OrgDomainID.String
		domainID = &v
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"inbox": inboxResponse{
			ID:          created.ID,
			Address:     created.Address,
			Status:      created.Status,
			OrgDomainID: domainID,
			CreatedAt:   created.CreatedAt,
		},
	})
}

func (h *Handler) handleListInboxes(w http.ResponseWriter, r *http.Request) {
	principal, err := h.requireBillingAdmin(r)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	orgID, err := resolveOrgIDForPrincipal(principal, strings.TrimSpace(r.URL.Query().Get("org_id")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	items, err := h.Store.ListInboxRecordsByOrg(r.Context(), orgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := make([]inboxResponse, 0, len(items))
	for _, item := range items {
		var domainID *string
		if item.OrgDomainID.Valid {
			v := item.OrgDomainID.String
			domainID = &v
		}
		resp = append(resp, inboxResponse{
			ID:          item.ID,
			Address:     item.Address,
			Status:      item.Status,
			OrgDomainID: domainID,
			CreatedAt:   item.CreatedAt,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"inboxes": resp})
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
	if h.Checkout == nil {
		// Fallback mock for tests
		portalURL := fmt.Sprintf("https://billing.stripe.com/p/session/mock?org_id=%s", req.OrgID)
		writeJSON(w, http.StatusOK, map[string]any{"url": portalURL})
		return
	}

	result, err := h.Checkout.CreateBillingPortalSession(r.Context(), req.OrgID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": result.URL})
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

func resolveOrgIDForPrincipal(principal auth.Principal, orgIDCandidate string) (string, error) {
	if principal.AuthMethod == "bootstrap_key" {
		if orgIDCandidate == "" {
			return "", errors.New("missing org_id")
		}
		return orgIDCandidate, nil
	}
	if principal.OrgID == "" {
		return "", errors.New("missing org_id")
	}
	if orgIDCandidate != "" && orgIDCandidate != principal.OrgID {
		return "", errors.New("forbidden org_id override")
	}
	return principal.OrgID, nil
}

func allowedCloudKeyScope(scope string) bool {
	switch scope {
	case "nerve:email.read", "nerve:email.search", "nerve:email.draft", "nerve:email.send", "nerve:email.inbox.create":
		return true
	default:
		return false
	}
}

func allowedServiceScope(scope string) bool {
	return scope == "nerve:admin.billing" || allowedCloudKeyScope(scope)
}

func generateCloudAPIKeyMaterial() (raw string, keyPrefix string, keyHash string, err error) {
	random := make([]byte, 32)
	if _, err = rand.Read(random); err != nil {
		return "", "", "", err
	}
	raw = "nrv_live_" + hex.EncodeToString(random)
	prefixLimit := 18
	if len(raw) < prefixLimit {
		prefixLimit = len(raw)
	}
	keyPrefix = raw[:prefixLimit]
	sum := sha256.Sum256([]byte(raw))
	keyHash = hex.EncodeToString(sum[:])
	return raw, keyPrefix, keyHash, nil
}

func normalizeMCPEndpoint(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" {
		return "", nil
	}

	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return "", errors.New("invalid mcp_endpoint")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("invalid mcp_endpoint scheme")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("mcp_endpoint must not include query or fragment")
	}

	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/mcp"
	} else if strings.HasSuffix(parsed.Path, "/") {
		parsed.Path = strings.TrimRight(parsed.Path, "/")
	}
	parsed.RawPath = ""

	return parsed.String(), nil
}

func generateDomainVerificationToken() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "nerve-verification=" + hex.EncodeToString(random), nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
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
