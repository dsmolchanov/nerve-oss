package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

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
