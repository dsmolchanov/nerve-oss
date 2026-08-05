package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMigration27DownRefusesDomainGrants(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 27); err != nil {
			t.Fatal(err)
		}
		st := &Store{db: db, q: db}
		_, _, _, grant := createDomainGrantFixture(t, ctx, st, "rollback-27")

		err := MigrateDownCore(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "cannot roll back core migration 0027") {
			t.Fatalf("down error=%v, want domain grant refusal", err)
		}
		version, versionErr := CurrentVersionCore(ctx, db)
		if versionErr != nil || version != 27 {
			t.Fatalf("version=%d err=%v after refused down", version, versionErr)
		}

		if _, err := db.ExecContext(ctx, `DELETE FROM org_domain_grants WHERE id = $1`, grant.ID); err != nil {
			t.Fatal(err)
		}
		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("clean migration 27 down: %v", err)
		}
	})
}

func TestMigration24DownRefusesDomainGrants(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 24); err != nil {
			t.Fatal(err)
		}
		st := &Store{db: db, q: db}
		ownerID, err := st.CreateOrg(ctx, "rollback-owner")
		if err != nil {
			t.Fatal(err)
		}
		granteeID, err := st.CreateOrg(ctx, "rollback-grantee")
		if err != nil {
			t.Fatal(err)
		}
		domainID, err := st.CreateOrgDomain(ctx, ownerID, "abrolia.com", "verify", "selector", "private", "public", "cname")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOrgDomainStatus(ctx, domainID, "active"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.EnsureOrgDomainGrant(ctx, ownerID, domainID, granteeID, "rollback:grant"); err != nil {
			t.Fatal(err)
		}

		err = MigrateDownCore(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "cannot roll back core migration 0024") {
			t.Fatalf("down error=%v, want domain grant refusal", err)
		}
		version, versionErr := CurrentVersionCore(ctx, db)
		if versionErr != nil || version != 24 {
			t.Fatalf("version=%d err=%v after refused down", version, versionErr)
		}

		if _, err := db.ExecContext(ctx, `DELETE FROM org_domain_grants`); err != nil {
			t.Fatal(err)
		}
		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("clean migration 24 down: %v", err)
		}
	})
}

func TestMigration24DownRefusesExternalReferences(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 24); err != nil {
			t.Fatal(err)
		}
		st := &Store{db: db, q: db}
		org, _, err := st.EnsureOrg(ctx, "referenced-family", "org:referenced-family")
		if err != nil {
			t.Fatal(err)
		}

		err = MigrateDownCore(ctx, db)
		if err == nil || !strings.Contains(err.Error(), "cannot roll back core migration 0024") {
			t.Fatalf("down error=%v, want external reference refusal", err)
		}
		version, versionErr := CurrentVersionCore(ctx, db)
		if versionErr != nil || version != 24 {
			t.Fatalf("version=%d err=%v after refused down", version, versionErr)
		}

		if _, err := db.ExecContext(ctx, `UPDATE orgs SET external_ref = NULL, deleted_at = NULL WHERE id = $1`, org.ID); err != nil {
			t.Fatal(err)
		}
		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatalf("clean migration 24 down: %v", err)
		}
	})
}

func TestDeleteOrgIfEmptyBlocksActiveServiceTokens(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "service-token-owner")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateServiceToken(
			ctx, "00000000-0000-0000-0000-000000000024", orgID, "test-agent",
			[]string{"nerve:admin.billing"}, time.Now().UTC().Add(time.Hour),
		); err != nil {
			t.Fatalf("create service token: %v", err)
		}

		deleted, err := st.DeleteOrgIfEmpty(ctx, orgID)
		if err != nil {
			t.Fatalf("delete org with active service token: %v", err)
		}
		if deleted {
			t.Fatal("org with an active service token was deleted")
		}
		var tombstoned bool
		if err := db.QueryRowContext(ctx, `SELECT deleted_at IS NOT NULL FROM orgs WHERE id = $1`, orgID).Scan(&tombstoned); err != nil {
			t.Fatal(err)
		}
		if tombstoned {
			t.Fatal("org tombstone was written while an active service token existed")
		}

		if err := st.RevokeActiveServiceTokens(ctx, orgID); err != nil {
			t.Fatalf("revoke service token: %v", err)
		}
		deleted, err = st.DeleteOrgIfEmpty(ctx, orgID)
		if err != nil || !deleted {
			t.Fatalf("delete org after token revocation = %t, err=%v", deleted, err)
		}
		if err := st.CreateServiceToken(
			ctx, "00000000-0000-0000-0000-000000000025", orgID, "late-agent",
			nil, time.Now().UTC().Add(time.Hour),
		); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("create token for deleted org error = %v, want sql.ErrNoRows", err)
		}
	})
}

func TestRotateServiceTokenIsAtomic(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "service-token-rotation")
		if err != nil {
			t.Fatal(err)
		}
		oldTokenID := "00000000-0000-0000-0000-000000000026"
		if err := st.CreateServiceToken(
			ctx, oldTokenID, orgID, "test-agent", []string{"nerve:admin.billing"}, time.Now().UTC().Add(time.Hour),
		); err != nil {
			t.Fatalf("create old service token: %v", err)
		}

		// A duplicate id makes the replacement insert fail after the transaction
		// has attempted to revoke the old token. The revocation must roll back.
		err = st.CreateServiceTokenWithRotation(
			ctx, oldTokenID, orgID, "replacement-agent", []string{"nerve:admin.billing"}, time.Now().UTC().Add(time.Hour), true,
		)
		if err == nil {
			t.Fatal("rotation with duplicate token id succeeded")
		}
		oldToken, err := st.GetServiceToken(ctx, oldTokenID)
		if err != nil {
			t.Fatalf("get old service token after failed rotation: %v", err)
		}
		if oldToken.RevokedAt.Valid {
			t.Fatal("failed rotation revoked the old service token")
		}

		newTokenID := "00000000-0000-0000-0000-000000000027"
		if err := st.CreateServiceTokenWithRotation(
			ctx, newTokenID, orgID, "replacement-agent", []string{"nerve:admin.billing"}, time.Now().UTC().Add(time.Hour), true,
		); err != nil {
			t.Fatalf("rotate service token: %v", err)
		}
		oldToken, err = st.GetServiceToken(ctx, oldTokenID)
		if err != nil {
			t.Fatalf("get old service token after rotation: %v", err)
		}
		if !oldToken.RevokedAt.Valid {
			t.Fatal("successful rotation left the old service token active")
		}
		newToken, err := st.GetServiceToken(ctx, newTokenID)
		if err != nil {
			t.Fatalf("get new service token after rotation: %v", err)
		}
		if newToken.RevokedAt.Valid {
			t.Fatal("successful rotation created a revoked replacement token")
		}
		deleted, err := st.DeleteOrgIfEmpty(ctx, orgID)
		if err != nil {
			t.Fatalf("delete org after rotation: %v", err)
		}
		if deleted {
			t.Fatal("org with the rotated active service token was deleted")
		}
	})
}

func TestEnsureOrgDomainConcurrentReplay(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "concurrent-domain-owner")
		if err != nil {
			t.Fatal(err)
		}

		const workers = 12
		start := make(chan struct{})
		results := make(chan struct {
			domain  OrgDomain
			created bool
			err     error
		}, workers)
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				domain, created, ensureErr := st.EnsureOrgDomain(
					ctx, orgID, "abrolia.com", "verify", "selector", "private", "public", "cname", "domain:abrolia.com",
				)
				results <- struct {
					domain  OrgDomain
					created bool
					err     error
				}{domain: domain, created: created, err: ensureErr}
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		var id string
		createdCount := 0
		for result := range results {
			if result.err != nil {
				t.Fatalf("concurrent domain ensure: %v", result.err)
			}
			if id == "" {
				id = result.domain.ID
			} else if result.domain.ID != id {
				t.Fatalf("domain replay returned id %s, want %s", result.domain.ID, id)
			}
			if result.created {
				createdCount++
			}
		}
		if createdCount != 1 {
			t.Fatalf("created count = %d, want 1", createdCount)
		}
	})
}

func TestEnsureOrgDomainReturnsResourceConflictForLegacyActiveDomain(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "legacy-domain-owner")
		if err != nil {
			t.Fatal(err)
		}
		domainID, err := st.CreateOrgDomain(ctx, orgID, "abrolia.com", "verify", "selector", "private", "public", "cname")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOrgDomainStatus(ctx, domainID, "active"); err != nil {
			t.Fatal(err)
		}
		_, _, err = st.EnsureOrgDomain(
			ctx, orgID, "ABROLIA.COM", "verify", "selector", "private", "public", "cname", "domain:new-ref",
		)
		if !errors.Is(err, ErrResourceConflict) {
			t.Fatalf("legacy active domain collision error=%v, want resource conflict", err)
		}
	})
}

func TestEnsureOrgDomainSerializesWithOrgDeletion(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "serialized-domain-owner")
		if err != nil {
			t.Fatal(err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "org:"+orgID); err != nil {
			t.Fatal(err)
		}
		ensureResult := make(chan error, 1)
		go func() {
			_, _, ensureErr := st.EnsureOrgDomain(
				ctx, orgID, "abrolia.com", "verify", "selector", "private", "public", "cname", "serialized:domain",
			)
			ensureResult <- ensureErr
		}()

		select {
		case err := <-ensureResult:
			t.Fatalf("domain ensure did not wait for reconciliation lock: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if _, err := tx.ExecContext(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, orgID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-ensureResult; !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("domain ensure after serialized deletion error=%v, want no rows", err)
		}
		var domains int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_domains WHERE org_id = $1`, orgID).Scan(&domains); err != nil {
			t.Fatal(err)
		}
		if domains != 0 {
			t.Fatalf("deleted org received %d domains", domains)
		}
	})
}

func TestEnsureOrgWebhookConcurrentReplay(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "concurrent-webhook-owner")
		if err != nil {
			t.Fatal(err)
		}

		const workers = 12
		start := make(chan struct{})
		results := make(chan struct {
			webhook OrgWebhook
			created bool
			err     error
		}, workers)
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				webhook, created, ensureErr := st.EnsureOrgWebhook(
					ctx, orgID, "https://example.com/inbound", []string{"message.received"}, "webhook:inbound",
				)
				results <- struct {
					webhook OrgWebhook
					created bool
					err     error
				}{webhook: webhook, created: created, err: ensureErr}
			}()
		}
		close(start)
		wg.Wait()
		close(results)

		var id string
		createdCount := 0
		for result := range results {
			if result.err != nil {
				t.Fatalf("concurrent webhook ensure: %v", result.err)
			}
			if id == "" {
				id = result.webhook.ID
			} else if result.webhook.ID != id {
				t.Fatalf("webhook replay returned id %s, want %s", result.webhook.ID, id)
			}
			if result.created {
				createdCount++
			} else if result.webhook.Secret != "" {
				t.Fatal("webhook replay exposed the existing signing secret")
			}
		}
		if createdCount != 1 {
			t.Fatalf("created count = %d, want 1", createdCount)
		}
	})
}

func TestEnsureOrgWebhookCanonicalReplayAndURLConflict(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "canonical-webhook-owner")
		if err != nil {
			t.Fatal(err)
		}

		created, wasCreated, err := st.EnsureOrgWebhook(
			ctx, orgID, "https://example.com/inbound",
			[]string{"email.received", "email.delivered", "email.received"}, "webhook:canonical",
		)
		if err != nil || !wasCreated {
			t.Fatalf("initial ensure: created=%v err=%v", wasCreated, err)
		}
		replayed, wasCreated, err := st.EnsureOrgWebhook(
			ctx, orgID, "https://example.com/inbound",
			[]string{"email.delivered", "email.received"}, "webhook:canonical",
		)
		if err != nil || wasCreated || replayed.ID != created.ID {
			t.Fatalf("canonical replay: webhook=%+v created=%v err=%v", replayed, wasCreated, err)
		}
		if replayed.Secret != "" {
			t.Fatal("canonical replay exposed the existing signing secret")
		}

		_, _, err = st.EnsureOrgWebhook(
			ctx, orgID, "https://example.com/inbound",
			[]string{"email.received"}, "webhook:different-ref",
		)
		if !errors.Is(err, ErrResourceConflict) {
			t.Fatalf("URL collision error=%v, want resource conflict", err)
		}
	})
}

func TestCreateInboxCanonicalAddressConflict(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		firstOrgID, err := st.CreateOrg(ctx, "first-inbox-owner")
		if err != nil {
			t.Fatal(err)
		}
		secondOrgID, err := st.CreateOrg(ctx, "second-inbox-owner")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInboxForOrg(ctx, firstOrgID, "family@abrolia.com", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := st.CreateInboxForOrg(ctx, secondOrgID, "Family@Abrolia.com", ""); !errors.Is(err, ErrResourceConflict) {
			t.Fatalf("duplicate inbox error=%v, want resource conflict", err)
		}
		first, err := st.GetInboxByAddress(ctx, "family@abrolia.com")
		if err != nil {
			t.Fatal(err)
		}
		if disabled, err := st.DisableInboxForOrg(ctx, firstOrgID, first.ID); err != nil || !disabled {
			t.Fatalf("disable first inbox: disabled=%v err=%v", disabled, err)
		}
		if _, err := st.CreateInboxForOrg(ctx, secondOrgID, "Family@Abrolia.com", ""); err != nil {
			t.Fatalf("recreate disabled canonical address: %v", err)
		}
	})
}

func TestEnsureOrgWebhookSerializesWithOrgDeletion(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "serialized-webhook-owner")
		if err != nil {
			t.Fatal(err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "org:"+orgID); err != nil {
			t.Fatal(err)
		}
		ensureResult := make(chan error, 1)
		go func() {
			_, _, ensureErr := st.EnsureOrgWebhook(
				ctx, orgID, "https://example.com/inbound", []string{"email.received"}, "serialized:webhook",
			)
			ensureResult <- ensureErr
		}()

		select {
		case err := <-ensureResult:
			t.Fatalf("webhook ensure did not wait for reconciliation lock: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if _, err := tx.ExecContext(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, orgID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-ensureResult; !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("webhook ensure after serialized deletion error=%v, want no rows", err)
		}
		var webhooks int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_webhooks WHERE org_id = $1`, orgID).Scan(&webhooks); err != nil {
			t.Fatal(err)
		}
		if webhooks != 0 {
			t.Fatalf("deleted org received %d webhooks", webhooks)
		}
	})
}

func TestDomainGrantCreationSerializesWithOrgDeletion(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		ownerID, err := st.CreateOrg(ctx, "serialized-owner")
		if err != nil {
			t.Fatal(err)
		}
		granteeID, err := st.CreateOrg(ctx, "serialized-grantee")
		if err != nil {
			t.Fatal(err)
		}
		domainID, err := st.CreateOrgDomain(ctx, ownerID, "abrolia.com", "verify", "selector", "private", "public", "cname")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOrgDomainStatus(ctx, domainID, "active"); err != nil {
			t.Fatal(err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, "org:"+granteeID); err != nil {
			t.Fatal(err)
		}
		grantResult := make(chan error, 1)
		go func() {
			_, _, ensureErr := st.EnsureOrgDomainGrant(ctx, ownerID, domainID, granteeID, "serialized:grant")
			grantResult <- ensureErr
		}()

		select {
		case err := <-grantResult:
			t.Fatalf("grant creation did not wait for reconciliation lock: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		if _, err := tx.ExecContext(ctx, `UPDATE orgs SET deleted_at = now() WHERE id = $1`, granteeID); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		if err := <-grantResult; !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("grant after serialized deletion error=%v, want no rows", err)
		}
		var grants int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM org_domain_grants WHERE grantee_org_id = $1`, granteeID).Scan(&grants); err != nil {
			t.Fatal(err)
		}
		if grants != 0 {
			t.Fatalf("deleted grantee received %d active grants", grants)
		}
	})
}

func TestOwnerScopedGrantRevokeSeesGranteeInbox(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		ownerID, err := st.CreateOrg(ctx, "platform-owner")
		if err != nil {
			t.Fatal(err)
		}
		granteeID, err := st.CreateOrg(ctx, "family-grantee")
		if err != nil {
			t.Fatal(err)
		}
		domainID, err := st.CreateOrgDomain(ctx, ownerID, "abrolia.com", "verify", "selector", "private", "public", "cname")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOrgDomainStatus(ctx, domainID, "active"); err != nil {
			t.Fatal(err)
		}
		grant, _, err := st.EnsureOrgDomainGrant(ctx, ownerID, domainID, granteeID, "family:abrolia.com")
		if err != nil {
			t.Fatal(err)
		}
		var inbox InboxRecord
		if err := st.RunAsOrg(ctx, granteeID, func(scoped *Store) error {
			var createErr error
			inbox, createErr = scoped.CreateInboxForOrg(ctx, granteeID, "family@abrolia.com", domainID, "resend")
			return createErr
		}); err != nil {
			t.Fatal(err)
		}

		err = st.RunAsOrg(ctx, ownerID, func(scoped *Store) error {
			_, revokeErr := scoped.RevokeOrgDomainGrant(ctx, grant.ID)
			return revokeErr
		})
		if !errors.Is(err, ErrResourceConflict) {
			t.Fatalf("owner-scoped revoke error=%v, want resource conflict", err)
		}
		if current, err := st.GetOrgDomainGrantByExternalRef(ctx, grant.ExternalRef); err != nil || current.Status != "active" {
			t.Fatalf("grant after refused revoke=%+v err=%v", current, err)
		}

		if err := st.UpdateOrgDomainStatus(ctx, domainID, "failed"); err != nil {
			t.Fatal(err)
		}
		if err := st.RunAsOrg(ctx, granteeID, func(scoped *Store) error {
			_, disableErr := scoped.DisableInboxForOrg(ctx, granteeID, inbox.ID)
			return disableErr
		}); err != nil {
			t.Fatal(err)
		}
		if err := st.RunAsOrg(ctx, ownerID, func(scoped *Store) error {
			revoked, revokeErr := scoped.RevokeOrgDomainGrant(ctx, grant.ID)
			if revokeErr == nil && !revoked {
				return errors.New("grant was not revoked")
			}
			return revokeErr
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGranteeAppRoleCanCreateInboxWithDomainGrant(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		adminStore := &Store{db: db, q: db}
		ownerID, granteeID, domainID, _ := createDomainGrantFixture(t, ctx, adminStore, "app-role")
		appStore := openDomainGrantAppStore(t, ctx, db)

		var inbox InboxRecord
		var cloudMode string
		err := appStore.RunAsOrg(ctx, granteeID, func(scoped *Store) error {
			var createErr error
			inbox, createErr = scoped.CreateInboxForOrg(
				ctx, granteeID, "app-role@abrolia.com", domainID, "resend",
			)
			if createErr != nil {
				return createErr
			}
			return scoped.q.QueryRowContext(ctx, `SELECT current_setting('app.cloud_mode', true)`).Scan(&cloudMode)
		})
		if err != nil {
			t.Fatalf("create grantee inbox as app role: %v", err)
		}
		if cloudMode != "true" {
			t.Fatalf("trigger restored app.cloud_mode=%q, want true", cloudMode)
		}
		ungrantedID, err := adminStore.CreateOrg(ctx, "domain-ungranted-app-role")
		if err != nil {
			t.Fatal(err)
		}
		if err := appStore.RunAsOrg(ctx, ungrantedID, func(scoped *Store) error {
			if _, createErr := scoped.q.ExecContext(ctx, `
				CREATE TEMP TABLE org_domains (
					id uuid, org_id uuid, status text
				) ON COMMIT DROP
			`); createErr != nil {
				return createErr
			}
			if _, createErr := scoped.q.ExecContext(ctx, `
				CREATE TEMP TABLE org_domain_grants (
					org_domain_id uuid, grantee_org_id uuid, status text
				) ON COMMIT DROP
			`); createErr != nil {
				return createErr
			}
			if _, createErr := scoped.q.ExecContext(ctx, `
				INSERT INTO pg_temp.org_domains (id, org_id, status)
				VALUES ($1, $2, 'active')
			`, domainID, ownerID); createErr != nil {
				return createErr
			}
			if _, createErr := scoped.q.ExecContext(ctx, `
				INSERT INTO pg_temp.org_domain_grants (org_domain_id, grantee_org_id, status)
				VALUES ($1, $2, 'active')
			`, domainID, ungrantedID); createErr != nil {
				return createErr
			}
			_, createErr := scoped.CreateInboxForOrg(
				ctx, ungrantedID, "temp-spoof@abrolia.com", domainID, "resend",
			)
			return createErr
		}); err == nil {
			t.Fatal("security-definer trigger trusted caller pg_temp grant tables")
		}
		if err := appStore.RunAsOrg(ctx, granteeID, func(scoped *Store) error {
			_, createErr := scoped.CreateInboxForOrg(
				ctx, ownerID, "cross-org@abrolia.com", domainID, "resend",
			)
			return createErr
		}); err == nil {
			t.Fatal("security-definer trigger bypassed inbox tenant RLS")
		}

		var rows int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*) FROM inboxes
			WHERE id = $1 AND org_id = $2 AND org_domain_id = $3 AND status = 'active'
		`, inbox.ID, granteeID, domainID).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("created inbox rows=%d, want 1", rows)
		}
	})
}

func TestInboxCreationAndGrantRevocationSerialize(t *testing.T) {
	t.Run("legacy revocation row lock rejects waiting inbox", func(t *testing.T) {
		withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
			migrateToLatest(t, ctx, db)
			adminStore := &Store{db: db, q: db}
			_, granteeID, domainID, grant := createDomainGrantFixture(t, ctx, adminStore, "revoke-first")
			appStore := openDomainGrantAppStore(t, ctx, db)
			testCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			tx, err := db.BeginTx(testCtx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			var lockedGrantID string
			if err := tx.QueryRowContext(testCtx, `
				SELECT id::text FROM org_domain_grants WHERE id = $1 AND status = 'active' FOR UPDATE
			`, grant.ID).Scan(&lockedGrantID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(testCtx, `
				UPDATE org_domain_grants SET status = 'revoked', revoked_at = now() WHERE id = $1
			`, grant.ID); err != nil {
				t.Fatal(err)
			}

			createResult := make(chan error, 1)
			go func() {
				createResult <- appStore.RunAsOrg(testCtx, granteeID, func(scoped *Store) error {
					_, createErr := scoped.CreateInboxForOrg(
						testCtx, granteeID, "revoke-first@abrolia.com", domainID, "resend",
					)
					return createErr
				})
			}()

			select {
			case err := <-createResult:
				t.Fatalf("inbox creation did not wait for grant lock: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-createResult; err == nil || !strings.Contains(err.Error(), "no active access to domain") {
				t.Fatalf("inbox error=%v, want inactive-grant rejection", err)
			}
			var rows int
			if err := db.QueryRowContext(testCtx, `SELECT count(*) FROM inboxes WHERE org_id = $1`, granteeID).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if rows != 0 {
				t.Fatalf("revoked grantee received %d inboxes", rows)
			}
		})
	})

	t.Run("committed inbox makes waiting revocation fail", func(t *testing.T) {
		withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
			migrateToLatest(t, ctx, db)
			adminStore := &Store{db: db, q: db}
			ownerID, granteeID, domainID, grant := createDomainGrantFixture(t, ctx, adminStore, "inbox-first")
			appStore := openDomainGrantAppStore(t, ctx, db)
			testCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()

			inserted := make(chan struct{})
			release := make(chan struct{})
			createResult := make(chan error, 1)
			go func() {
				createResult <- appStore.RunAsOrg(testCtx, granteeID, func(scoped *Store) error {
					if _, err := scoped.CreateInboxForOrg(
						testCtx, granteeID, "inbox-first@abrolia.com", domainID, "resend",
					); err != nil {
						return err
					}
					close(inserted)
					select {
					case <-release:
						return nil
					case <-testCtx.Done():
						return testCtx.Err()
					}
				})
			}()
			select {
			case <-inserted:
			case <-testCtx.Done():
				t.Fatal(testCtx.Err())
			}

			revokeResult := make(chan error, 1)
			go func() {
				revokeResult <- adminStore.RunAsOrg(testCtx, ownerID, func(scoped *Store) error {
					_, revokeErr := scoped.RevokeOrgDomainGrant(testCtx, grant.ID)
					return revokeErr
				})
			}()
			select {
			case err := <-revokeResult:
				t.Fatalf("grant revocation did not wait for inbox lock: %v", err)
			case <-time.After(100 * time.Millisecond):
			}

			close(release)
			if err := <-createResult; err != nil {
				t.Fatalf("commit inbox: %v", err)
			}
			if err := <-revokeResult; !errors.Is(err, ErrResourceConflict) {
				t.Fatalf("revocation error=%v, want resource conflict", err)
			}
			current, err := adminStore.GetOrgDomainGrantByExternalRef(testCtx, grant.ExternalRef)
			if err != nil || current.Status != "active" {
				t.Fatalf("grant after refused revoke=%+v err=%v", current, err)
			}
		})
	})
}

func createDomainGrantFixture(t *testing.T, ctx context.Context, st *Store, suffix string) (string, string, string, OrgDomainGrant) {
	t.Helper()
	ownerID, err := st.CreateOrg(ctx, "domain-owner-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	granteeID, err := st.CreateOrg(ctx, "domain-grantee-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	domainID, err := st.CreateOrgDomain(ctx, ownerID, suffix+".abrolia.com", "verify", "selector", "private", "public", "cname")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateOrgDomainStatus(ctx, domainID, "active"); err != nil {
		t.Fatal(err)
	}
	grant, _, err := st.EnsureOrgDomainGrant(ctx, ownerID, domainID, granteeID, "grant:"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	return ownerID, granteeID, domainID, grant
}

func openDomainGrantAppStore(t *testing.T, ctx context.Context, db *sql.DB) *Store {
	t.Helper()
	roleName := "email_grant_rls_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD 'rls_email_grant'`, roleName)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, roleName)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`GRANT SELECT ON org_domains, org_domain_grants TO %s`, roleName)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE ON inboxes TO %s`, roleName)); err != nil {
		t.Fatal(err)
	}

	var databaseName string
	if err := db.QueryRowContext(ctx, `SELECT current_database()`).Scan(&databaseName); err != nil {
		t.Fatal(err)
	}
	baseDSN := os.Getenv("NM_TEST_DB_DSN")
	if baseDSN == "" {
		baseDSN = "postgres://neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
	}
	appDSN, err := dsnWithDatabase(baseDSN, databaseName)
	if err != nil {
		t.Fatal(err)
	}
	appDSN, err = dsnWithCredentials(appDSN, roleName, "rls_email_grant")
	if err != nil {
		t.Fatal(err)
	}
	appDB, err := sql.Open("pgx", appDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = appDB.Close()
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP OWNED BY %s`, roleName))
		_, _ = db.ExecContext(context.Background(), fmt.Sprintf(`DROP ROLE IF EXISTS %s`, roleName))
	})
	return &Store{db: appDB, q: appDB}
}
