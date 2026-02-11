package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

// stripePriceID is the lookup_key-based price for the Pro plan.
// Stripe resolves this via lookup_key when creating checkout sessions.
const stripePriceID = "price_1SzW5LDPvkk7SvtZKJImtxzx"

const stripeProvider = "stripe"

type StripeService struct {
	Config config.Config
	Store  *store.Store
	Now    func() time.Time
}

func NewStripeService(cfg config.Config, st *store.Store) *StripeService {
	return &StripeService{
		Config: cfg,
		Store:  st,
		Now:    func() time.Time { return time.Now().UTC() },
	}
}

type stripeEvent struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Data struct {
		Object json.RawMessage `json:"object"`
	} `json:"data"`
}

type stripeCheckoutSession struct {
	ID                string            `json:"id"`
	ClientReferenceID string            `json:"client_reference_id"`
	Customer          string            `json:"customer"`
	Subscription      string            `json:"subscription"`
	Metadata          map[string]string `json:"metadata"`
}

type stripePrice struct {
	ID        string `json:"id"`
	LookupKey string `json:"lookup_key"`
}

type stripeSubscriptionItem struct {
	Price stripePrice `json:"price"`
}

type stripeSubscriptionItems struct {
	Data []stripeSubscriptionItem `json:"data"`
}

type stripeSubscription struct {
	ID                 string                  `json:"id"`
	Customer           string                  `json:"customer"`
	Status             string                  `json:"status"`
	CurrentPeriodStart int64                   `json:"current_period_start"`
	CurrentPeriodEnd   int64                   `json:"current_period_end"`
	CancelAtPeriodEnd  bool                    `json:"cancel_at_period_end"`
	Metadata           map[string]string       `json:"metadata"`
	Items              stripeSubscriptionItems `json:"items"`
}

type stripeInvoice struct {
	ID           string `json:"id"`
	Customer     string `json:"customer"`
	Subscription string `json:"subscription"`
}

func (s *StripeService) ProcessWebhook(ctx context.Context, payload []byte, signatureHeader string) error {
	if s == nil || s.Store == nil {
		return errors.New("stripe service not configured")
	}
	if err := s.verifySignature(payload, signatureHeader); err != nil {
		return err
	}

	var event stripeEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return err
	}
	if event.ID == "" || event.Type == "" {
		return errors.New("invalid stripe event payload")
	}

	payloadHash := sha256Hex(payload)
	inserted, existingStatus, err := s.Store.InsertWebhookEventIfAbsent(ctx, stripeProvider, event.ID, event.Type, payloadHash)
	if err != nil {
		return err
	}
	if !inserted && existingStatus == "processed" {
		return nil
	}

	if err := s.applyEvent(ctx, event); err != nil {
		_ = s.Store.UpdateWebhookEventStatus(ctx, stripeProvider, event.ID, "failed", err.Error())
		return err
	}
	if err := s.Store.UpdateWebhookEventStatus(ctx, stripeProvider, event.ID, "processed", ""); err != nil {
		return err
	}
	return nil
}

func (s *StripeService) applyEvent(ctx context.Context, event stripeEvent) error {
	switch event.Type {
	case "checkout.session.completed":
		var session stripeCheckoutSession
		if err := json.Unmarshal(event.Data.Object, &session); err != nil {
			return err
		}
		orgID := strings.TrimSpace(session.ClientReferenceID)
		if orgID == "" {
			return errors.New("checkout session missing client_reference_id org mapping")
		}
		if strings.TrimSpace(session.Subscription) == "" {
			return nil
		}
		return s.Store.UpsertSubscription(ctx, store.SubscriptionRecord{
			OrgID:                  orgID,
			Provider:               stripeProvider,
			ExternalCustomerID:     session.Customer,
			ExternalSubscriptionID: session.Subscription,
			Status:                 "checkout_completed",
		})
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		var sub stripeSubscription
		if err := json.Unmarshal(event.Data.Object, &sub); err != nil {
			return err
		}
		return s.applySubscriptionSnapshot(ctx, sub, event.Type == "customer.subscription.deleted")
	case "invoice.paid":
		var invoice stripeInvoice
		if err := json.Unmarshal(event.Data.Object, &invoice); err != nil {
			return err
		}
		return s.applyInvoiceStatus(ctx, invoice, "active")
	case "invoice.payment_failed":
		var invoice stripeInvoice
		if err := json.Unmarshal(event.Data.Object, &invoice); err != nil {
			return err
		}
		return s.applyInvoiceStatus(ctx, invoice, "past_due")
	default:
		return nil
	}
}

func (s *StripeService) applySubscriptionSnapshot(ctx context.Context, sub stripeSubscription, forceCanceled bool) error {
	orgID, err := s.resolveOrgID(ctx, sub.Metadata["org_id"], sub.Customer, sub.ID)
	if err != nil {
		return err
	}
	planCode := extractPlanCode(sub)
	if planCode == "" {
		return errors.New("subscription event missing plan code")
	}
	plan, err := s.Store.GetPlanEntitlement(ctx, planCode)
	if err != nil {
		return err
	}

	periodStart := fromUnixOrNow(sub.CurrentPeriodStart, s.Now)
	periodEnd := fromUnixOrDefault(sub.CurrentPeriodEnd, periodStart.Add(30*24*time.Hour))
	status := normalizeSubscriptionStatus(sub.Status, forceCanceled)

	subRecord := store.SubscriptionRecord{
		OrgID:                  orgID,
		Provider:               stripeProvider,
		ExternalCustomerID:     sub.Customer,
		ExternalSubscriptionID: sub.ID,
		Status:                 status,
		CurrentPeriodStart:     sql.NullTime{Time: periodStart, Valid: true},
		CurrentPeriodEnd:       sql.NullTime{Time: periodEnd, Valid: true},
		CancelAtPeriodEnd:      sub.CancelAtPeriodEnd,
	}
	if err := s.Store.UpsertSubscription(ctx, subRecord); err != nil {
		return err
	}

	ent := store.OrgEntitlement{
		OrgID:              orgID,
		PlanCode:           plan.PlanCode,
		SubscriptionStatus: status,
		MCPRPM:             plan.MCPRPM,
		MonthlyUnits:       plan.MonthlyUnits,
		MaxInboxes:         plan.MaxInboxes,
		UsagePeriodStart:   periodStart,
		UsagePeriodEnd:     periodEnd,
		GraceUntil:         graceUntilForStatus(status, periodEnd, s.Config.Metering.PastDueGraceDays),
	}
	if err := s.Store.UpsertOrgEntitlement(ctx, ent); err != nil {
		return err
	}
	return s.Store.EnsureOrgUsageCounter(ctx, orgID, "mcp_units", periodStart, periodEnd)
}

func (s *StripeService) applyInvoiceStatus(ctx context.Context, invoice stripeInvoice, mappedStatus string) error {
	orgID, err := s.resolveOrgID(ctx, "", invoice.Customer, invoice.Subscription)
	if err != nil {
		return err
	}

	if invoice.Subscription != "" {
		if err := s.Store.UpdateSubscriptionStatusByExternalSubscriptionID(ctx, invoice.Subscription, mappedStatus); err != nil {
			return err
		}
	} else if invoice.Customer != "" {
		if err := s.Store.UpdateSubscriptionStatusByExternalCustomerID(ctx, invoice.Customer, mappedStatus); err != nil {
			return err
		}
	}

	ent, err := s.Store.GetOrgEntitlement(ctx, orgID)
	if err != nil {
		return err
	}
	ent.SubscriptionStatus = mappedStatus
	ent.GraceUntil = graceUntilForStatus(mappedStatus, ent.UsagePeriodEnd, s.Config.Metering.PastDueGraceDays)
	return s.Store.UpsertOrgEntitlement(ctx, ent)
}

func (s *StripeService) resolveOrgID(ctx context.Context, directOrgID, customerID, subscriptionID string) (string, error) {
	if orgID := strings.TrimSpace(directOrgID); orgID != "" {
		return orgID, nil
	}
	if subscriptionID != "" {
		orgID, err := s.Store.FindOrgByExternalSubscriptionID(ctx, subscriptionID)
		if err == nil {
			return orgID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	if customerID != "" {
		orgID, err := s.Store.FindOrgByExternalCustomerID(ctx, customerID)
		if err == nil {
			return orgID, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	return "", errors.New("unable to resolve org for stripe event")
}

func (s *StripeService) verifySignature(payload []byte, signatureHeader string) error {
	secret := strings.TrimSpace(s.Config.Billing.StripeWebhookSecret)
	if secret == "" {
		return errors.New("stripe webhook secret not configured")
	}

	timestamp, signature, err := parseStripeSignatureHeader(signatureHeader)
	if err != nil {
		return err
	}

	signedPayload := []byte(timestamp + "." + string(payload))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(signedPayload)
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return errors.New("invalid stripe signature")
	}

	tsInt, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return err
	}
	if delta := s.Now().Sub(time.Unix(tsInt, 0)); delta > 5*time.Minute || delta < -5*time.Minute {
		return errors.New("stripe signature timestamp outside tolerance")
	}
	return nil
}

func parseStripeSignatureHeader(header string) (string, string, error) {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			ts = kv[1]
		case "v1":
			sig = kv[1]
		}
	}
	if ts == "" || sig == "" {
		return "", "", errors.New("invalid stripe signature header")
	}
	return ts, sig, nil
}

func extractPlanCode(sub stripeSubscription) string {
	if len(sub.Items.Data) == 0 {
		return ""
	}
	price := sub.Items.Data[0].Price
	if strings.TrimSpace(price.LookupKey) != "" {
		return strings.TrimSpace(price.LookupKey)
	}
	return strings.TrimSpace(price.ID)
}

func normalizeSubscriptionStatus(status string, deleted bool) string {
	if deleted {
		return "canceled"
	}
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "trialing", "active", "past_due", "canceled", "unpaid":
		return strings.ToLower(strings.TrimSpace(status))
	default:
		return "unpaid"
	}
}

func fromUnixOrNow(raw int64, now func() time.Time) time.Time {
	if raw <= 0 {
		return now()
	}
	return time.Unix(raw, 0).UTC()
}

func fromUnixOrDefault(raw int64, fallback time.Time) time.Time {
	if raw <= 0 {
		return fallback
	}
	return time.Unix(raw, 0).UTC()
}

func graceUntilForStatus(status string, periodEnd time.Time, graceDays int) sql.NullTime {
	if status != "past_due" {
		return sql.NullTime{}
	}
	if graceDays <= 0 {
		graceDays = 1
	}
	return sql.NullTime{Time: periodEnd.Add(time.Duration(graceDays) * 24 * time.Hour), Valid: true}
}

// ── Checkout & Billing Portal (Stripe REST API) ────────────────

type CheckoutResult struct {
	CheckoutURL       string `json:"checkout_url"`
	ClientReferenceID string `json:"client_reference_id"`
}

func (s *StripeService) CreateCheckoutSession(ctx context.Context, orgID, successURL, cancelURL string) (*CheckoutResult, error) {
	sk := strings.TrimSpace(s.Config.Billing.StripeSecretKey)
	if sk == "" {
		return nil, errors.New("stripe secret key not configured")
	}

	form := "mode=subscription" +
		"&client_reference_id=" + orgID +
		"&line_items[0][price]=" + stripePriceID + "&line_items[0][quantity]=1" +
		"&metadata[org_id]=" + orgID +
		"&subscription_data[metadata][org_id]=" + orgID

	if successURL != "" {
		form += "&success_url=" + successURL
	} else {
		form += "&success_url=https://nerve.email/?checkout=success"
	}
	if cancelURL != "" {
		form += "&cancel_url=" + cancelURL
	} else {
		form += "&cancel_url=https://nerve.email/?checkout=cancel"
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/checkout/sessions", strings.NewReader(form))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(sk, "")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, errors.New("stripe checkout error: " + string(body))
	}

	var session struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, err
	}
	return &CheckoutResult{
		CheckoutURL:       session.URL,
		ClientReferenceID: orgID,
	}, nil
}

type PortalResult struct {
	URL string `json:"url"`
}

func (s *StripeService) CreateBillingPortalSession(ctx context.Context, orgID string) (*PortalResult, error) {
	sk := strings.TrimSpace(s.Config.Billing.StripeSecretKey)
	if sk == "" {
		return nil, errors.New("stripe secret key not configured")
	}

	// Find the Stripe customer ID for this org
	customerID, err := s.Store.FindStripeCustomerByOrg(ctx, orgID)
	if err != nil {
		return nil, errors.New("no billing account found for this organization")
	}

	form := "customer=" + customerID +
		"&return_url=https://nerve.email/billing"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.stripe.com/v1/billing_portal/sessions", strings.NewReader(form))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.SetBasicAuth(sk, "")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, errors.New("stripe portal error: " + string(body))
	}

	var session struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &session); err != nil {
		return nil, err
	}
	return &PortalResult{URL: session.URL}, nil
}

func sha256Hex(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
