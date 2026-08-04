package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

func TestOrgDomainLegacyCreateAndReadRemainCompatibleAtCore19(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToVersion(t, ctx, db, 19)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'core19-domain')`, orgID); err != nil {
			t.Fatal(err)
		}
		domainID, err := st.CreateOrgDomain(ctx, orgID, "core19.example", "verification", "nerve", "", "", "cname")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE org_domains SET status = 'active' WHERE id = $1`, domainID); err != nil {
			t.Fatal(err)
		}
		domain, err := st.GetOrgDomain(ctx, "core19.example")
		if err != nil {
			t.Fatal(err)
		}
		if domain.ID != domainID || domain.ExternalRef.Valid {
			t.Fatalf("domain=%+v", domain)
		}
	})
}

func TestOrgWebhookLegacyCreateAndReadRemainCompatibleAtCore19(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToVersion(t, ctx, db, 19)
		st := &Store{db: db, q: db}
		orgID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, 'core19-webhook')`, orgID); err != nil {
			t.Fatal(err)
		}
		created, err := st.CreateOrgWebhook(ctx, orgID, "https://hooks.example/core19", []string{"email.received"})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := st.GetOrgWebhook(ctx, orgID, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.ID != created.ID || loaded.ExternalRef.Valid {
			t.Fatalf("webhook=%+v", loaded)
		}
	})
}
