package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

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
