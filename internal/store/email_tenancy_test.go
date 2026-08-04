package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

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
