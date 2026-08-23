package cloudapi

// Regression coverage for the proof-aware legacy domain release contract:
// DELETE /v1/domains/{id} finalizes the local row only after an authoritative
// exact-ID absence readback. A 2xx provider DELETE, an ambiguous DELETE
// failure, or an ambiguous readback failure must all leave the local row and
// its releasing claim in place for scheduled reconciliation.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"neuralmail/internal/auth"
	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

const legacyDeleteTestDomainID = "d_del1"

func legacyDeletePresentPayload() string {
	return fmt.Sprintf(`{"data":{"id":%q,"name":"acme.com","status":"verified",`+
		`"records":[],"capabilities":{"sending":"enabled","receiving":"enabled"}}}`,
		legacyDeleteTestDomainID)
}

// enableLegacyLifecycleSchemaForTest installs the Cloud-9+ relations the OSS
// migration bundle omits and stamps the temp database as Cloud schema 10, so
// the delete tests exercise the proof-aware lifecycle path end to end. The
// shapes mirror internal/store's own lifecycle fixture.
func enableLegacyLifecycleSchemaForTest(t *testing.T, st *store.Store) {
	t.Helper()
	_, err := st.DB().ExecContext(context.Background(), `
		CREATE TABLE IF NOT EXISTS agent_onboardings (
		  id uuid PRIMARY KEY
		);

		CREATE TABLE IF NOT EXISTS managed_mailbox_platform_domains (
		  org_domain_id     uuid        PRIMARY KEY REFERENCES org_domains(id) ON DELETE RESTRICT,
		  owner_org_id      uuid        NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
		  canonical_domain  text        NOT NULL UNIQUE,
		  state             text        NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'disabled')),
		  validated_at      timestamptz NOT NULL,
		  disabled_at       timestamptz,
		  created_at        timestamptz NOT NULL DEFAULT now(),
		  updated_at        timestamptz NOT NULL DEFAULT now()
		);

		CREATE TABLE IF NOT EXISTS domain_ownership_claims (
		  canonical_domain  text        PRIMARY KEY
		                                 CHECK (canonical_domain = lower(canonical_domain)
		                                        AND canonical_domain NOT LIKE '%.'),
		  org_domain_id     uuid        NOT NULL UNIQUE REFERENCES org_domains(id) ON DELETE RESTRICT,
		  org_id            uuid        NOT NULL REFERENCES orgs(id) ON DELETE RESTRICT,
		  onboarding_id     uuid        REFERENCES agent_onboardings(id) ON DELETE RESTRICT,
		  owner_kind        text        NOT NULL CHECK (owner_kind IN ('legacy', 'autonomous')),
		  state             text        NOT NULL CHECK (state IN ('pending', 'provider_owned', 'releasing')),
		  workflow_version  bigint      NOT NULL DEFAULT 1 CHECK (workflow_version > 0),
		  lease_owner       text,
		  lease_expires_at  timestamptz,
		  claim_expires_at  timestamptz,
		  created_at        timestamptz NOT NULL DEFAULT now(),
		  updated_at        timestamptz NOT NULL DEFAULT now(),
		  CHECK ((lease_owner IS NULL AND lease_expires_at IS NULL)
		      OR (lease_owner IS NOT NULL AND lease_expires_at IS NOT NULL)),
		  CHECK ((owner_kind = 'legacy' AND onboarding_id IS NULL)
		      OR (owner_kind = 'autonomous' AND onboarding_id IS NOT NULL))
		);

		INSERT INTO schema_migrations_cloud (version_id, is_applied, tstamp)
		SELECT 10, true, now()
		WHERE NOT EXISTS (SELECT 1 FROM schema_migrations_cloud WHERE version_id = 10);
	`)
	if err != nil {
		t.Fatalf("enable legacy provider lifecycle schema: %v", err)
	}
}

func newLegacyDeleteHandler(t *testing.T, st *store.Store, providerURL string) (*http.ServeMux, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Security.APIKey = "bootstrap-admin"
	cfg.Cloud.Mode = true
	if providerURL != "" {
		cfg.Resend.APIKey = "re_test_key"
		cfg.Resend.BaseURL = providerURL
	}
	handler := NewHandler(cfg, st, auth.NewService(cfg, st), &stubBilling{}, &stubTokenIssuer{})
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	orgID, err := st.CreateOrg(context.Background(), "delete-org")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	return mux, orgID
}

func serveLegacyDelete(t *testing.T, mux *http.ServeMux, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("build %s request: %v", method, err)
	}
	req.Header.Set("X-API-Key", "bootstrap-admin")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// createLegacyDeleteDomain provisions one provider-backed acme.com row through
// the public API so the delete tests exercise real stored provider identity.
func createLegacyDeleteDomain(t *testing.T, mux *http.ServeMux, orgID string) string {
	t.Helper()
	req := jsonRequest(t, http.MethodPost, "/v1/domains", map[string]any{
		"org_id": orgID,
		"domain": "acme.com",
	})
	req.Header.Set("X-API-Key", "bootstrap-admin")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected provider-backed domain create success, got %d body=%s", rec.Code, rec.Body.String())
	}
	var created struct {
		Domain struct {
			ID string `json:"id"`
		} `json:"domain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode domain create response: %v", err)
	}
	if created.Domain.ID == "" {
		t.Fatalf("expected non-empty local domain id, body=%s", rec.Body.String())
	}
	return created.Domain.ID
}

func TestLegacyDomainDeleteFinalizesOnlyOnAuthoritativeAbsence(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		enableLegacyLifecycleSchemaForTest(t, st)
		var deletes atomic.Int32
		var providerDeleted atomic.Bool
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/domains":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fmt.Sprintf(`{"data":{"id":%q,"name":"acme.com","status":"not_started","records":[]}}`, legacyDeleteTestDomainID)))
			case r.Method == http.MethodGet && r.URL.Path == "/domains/"+legacyDeleteTestDomainID:
				w.Header().Set("Content-Type", "application/json")
				if providerDeleted.Load() {
					w.WriteHeader(http.StatusNotFound)
					_, _ = w.Write([]byte(`{"name":"not_found","message":"domain not found"}`))
					return
				}
				_, _ = w.Write([]byte(legacyDeletePresentPayload()))
			case r.Method == http.MethodPatch && r.URL.Path == "/domains/"+legacyDeleteTestDomainID:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fmt.Sprintf(`{"data":{"id":%q,"name":"acme.com","status":"verified","capabilities":{"sending":"enabled","receiving":"disabled"}}}`, legacyDeleteTestDomainID)))
			case r.Method == http.MethodDelete && r.URL.Path == "/domains/"+legacyDeleteTestDomainID:
				deletes.Add(1)
				providerDeleted.Store(true)
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			}
		}))
		defer provider.Close()

		mux, orgID := newLegacyDeleteHandler(t, st, provider.URL)
		domainID := createLegacyDeleteDomain(t, mux, orgID)

		delTarget := "/v1/domains/" + domainID + "?org_id=" + url.QueryEscape(orgID)
		rec := serveLegacyDelete(t, mux, http.MethodDelete, delTarget)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected delete success after authoritative absence, got %d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"deleted"`) {
			t.Fatalf("expected deleted status, body=%s", rec.Body.String())
		}
		if deletes.Load() != 1 {
			t.Fatalf("expected exactly one provider DELETE, got %d", deletes.Load())
		}
		if _, err := st.GetOrgDomainByIDForOrg(ctx, orgID, domainID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected finalized local deletion after exact-id absence, got err=%v", err)
		}
		if _, err := st.GetDomainOwnershipClaim(ctx, "acme.com"); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected released ownership claim removal, got err=%v", err)
		}
	})
}

func TestLegacyDomainDeleteKeepsRowWhenReadbackShowsPresence(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		enableLegacyLifecycleSchemaForTest(t, st)
		var deletes atomic.Int32
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/domains":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fmt.Sprintf(`{"data":{"id":%q,"name":"acme.com","status":"not_started","records":[]}}`, legacyDeleteTestDomainID)))
			case r.Method == http.MethodGet && r.URL.Path == "/domains/"+legacyDeleteTestDomainID:
				// The exact ID remains present even after DELETE 2xx.
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(legacyDeletePresentPayload()))
			case r.Method == http.MethodPatch && r.URL.Path == "/domains/"+legacyDeleteTestDomainID:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fmt.Sprintf(`{"data":{"id":%q,"name":"acme.com","status":"verified","capabilities":{"sending":"enabled","receiving":"disabled"}}}`, legacyDeleteTestDomainID)))
			case r.Method == http.MethodDelete && r.URL.Path == "/domains/"+legacyDeleteTestDomainID:
				deletes.Add(1)
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			}
		}))
		defer provider.Close()

		mux, orgID := newLegacyDeleteHandler(t, st, provider.URL)
		domainID := createLegacyDeleteDomain(t, mux, orgID)

		delTarget := "/v1/domains/" + domainID + "?org_id=" + url.QueryEscape(orgID)
		rec := serveLegacyDelete(t, mux, http.MethodDelete, delTarget)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("expected fail-closed delete without absence proof, got %d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "not confirmed") {
			t.Fatalf("expected not-confirmed failure detail, body=%s", rec.Body.String())
		}
		if deletes.Load() != 1 {
			t.Fatalf("expected exactly one provider DELETE attempt, got %d", deletes.Load())
		}
		local, err := st.GetOrgDomainByIDForOrg(ctx, orgID, domainID)
		if err != nil {
			t.Fatalf("expected local row retention without absence proof, got err=%v", err)
		}
		if local.Status != "failed" {
			t.Fatalf("expected retained failed release state, got %q", local.Status)
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "acme.com")
		if err != nil || claim.State != "releasing" {
			t.Fatalf("expected retained releasing claim, claim=%+v err=%v", claim, err)
		}
	})
}

func TestLegacyDomainDeleteKeepsRowOnAmbiguousReadbackFailure(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		enableLegacyLifecycleSchemaForTest(t, st)
		var deletes atomic.Int32
		var providerDeleted atomic.Bool
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodPost && r.URL.Path == "/domains":
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fmt.Sprintf(`{"data":{"id":%q,"name":"acme.com","status":"not_started","records":[]}}`, legacyDeleteTestDomainID)))
			case r.Method == http.MethodGet && r.URL.Path == "/domains/"+legacyDeleteTestDomainID:
				if providerDeleted.Load() {
					// Ambiguous absence evidence: the readback itself failed.
					http.Error(w, `{"name":"provider_error"}`, http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(legacyDeletePresentPayload()))
			case r.Method == http.MethodPatch && r.URL.Path == "/domains/"+legacyDeleteTestDomainID:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(fmt.Sprintf(`{"data":{"id":%q,"name":"acme.com","status":"verified","capabilities":{"sending":"enabled","receiving":"disabled"}}}`, legacyDeleteTestDomainID)))
			case r.Method == http.MethodDelete && r.URL.Path == "/domains/"+legacyDeleteTestDomainID:
				deletes.Add(1)
				providerDeleted.Store(true)
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected provider request: %s %s", r.Method, r.URL.Path)
				http.Error(w, "unexpected provider request", http.StatusInternalServerError)
			}
		}))
		defer provider.Close()

		mux, orgID := newLegacyDeleteHandler(t, st, provider.URL)
		domainID := createLegacyDeleteDomain(t, mux, orgID)

		delTarget := "/v1/domains/" + domainID + "?org_id=" + url.QueryEscape(orgID)
		rec := serveLegacyDelete(t, mux, http.MethodDelete, delTarget)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("expected fail-closed delete on ambiguous readback, got %d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "not confirmed") {
			t.Fatalf("expected not-confirmed failure detail, body=%s", rec.Body.String())
		}
		if _, err := st.GetOrgDomainByIDForOrg(ctx, orgID, domainID); err != nil {
			t.Fatalf("expected local row retention on ambiguous readback, got err=%v", err)
		}
		claim, err := st.GetDomainOwnershipClaim(ctx, "acme.com")
		if err != nil || claim.State != "releasing" {
			t.Fatalf("expected retained releasing claim, claim=%+v err=%v", claim, err)
		}
	})
}

func TestLegacyDomainLocalOnlyDeleteNeedsNoProviderProof(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		enableLegacyLifecycleSchemaForTest(t, st)
		var providerCalls atomic.Int32
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			providerCalls.Add(1)
			http.Error(w, "local-only delete must not contact the provider", http.StatusInternalServerError)
		}))
		defer provider.Close()

		mux, orgID := newLegacyDeleteHandler(t, st, provider.URL)
		localID, err := st.CreateOrgDomain(context.Background(), orgID, "acme.com",
			"nerve-verification=local-only", "selector", "", "", "txt")
		if err != nil {
			t.Fatalf("create local-only domain: %v", err)
		}

		delTarget := "/v1/domains/" + localID + "?org_id=" + url.QueryEscape(orgID)
		rec := serveLegacyDelete(t, mux, http.MethodDelete, delTarget)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected local-only delete success, got %d body=%s", rec.Code, rec.Body.String())
		}
		if providerCalls.Load() != 0 {
			t.Fatalf("expected zero provider calls for local-only release, got %d", providerCalls.Load())
		}
		if _, err := st.GetOrgDomainByIDForOrg(ctx, orgID, localID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("expected finalized local-only deletion, got err=%v", err)
		}

		missing := serveLegacyDelete(t, mux, http.MethodDelete, "/v1/domains/00000000-0000-4000-8000-000000000001?org_id="+url.QueryEscape(orgID))
		if missing.Code != http.StatusOK || !strings.Contains(missing.Body.String(), "absent") {
			t.Fatalf("expected idempotent absent status for missing domain, got %d body=%s", missing.Code, missing.Body.String())
		}
	})
}
