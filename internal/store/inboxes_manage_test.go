package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestCanonicalInboxAddressNormalizesStorageSpelling(t *testing.T) {
	address, domain, err := canonicalInboxAddress("  Family+TAG@Abrolia.COM.  ")
	if err != nil {
		t.Fatal(err)
	}
	if address != "family+tag@abrolia.com" || domain != "abrolia.com" {
		t.Fatalf("canonical address=%q domain=%q", address, domain)
	}
}

func TestInboxCreationRejectsInvalidAddressBeforeDatabaseAccess(t *testing.T) {
	st := &Store{}
	for _, address := range []string{
		"family@extra@abrolia.com",
		"family@",
		"family name@abrolia.com",
		"family@abrolia.com/path",
	} {
		t.Run(address, func(t *testing.T) {
			if _, err := st.CreateInboxForOrg(context.Background(), "org-id", address, ""); err == nil || strings.Contains(err.Error(), "database") {
				t.Fatalf("CreateInboxForOrg(%q) error=%v", address, err)
			}
			if _, _, err := st.EnsureInboxForOrg(context.Background(), "org-id", address, "", "resend", "invalid-address"); err == nil || strings.Contains(err.Error(), "database") {
				t.Fatalf("EnsureInboxForOrg(%q) error=%v", address, err)
			}
		})
	}
}

func TestInboxCreationStoresCanonicalAddress(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "canonical-inbox-owner")
		if err != nil {
			t.Fatal(err)
		}

		created, err := st.CreateInboxForOrg(ctx, orgID, "  Family@Abrolia.COM.  ", "", "resend")
		if err != nil {
			t.Fatal(err)
		}
		if created.Address != "family@abrolia.com" {
			t.Fatalf("created address=%q", created.Address)
		}

		ensured, wasCreated, err := st.EnsureInboxForOrg(
			ctx, orgID, "  Replay@Abrolia.COM.  ", "", "resend", "canonical-address-replay",
		)
		if err != nil {
			t.Fatal(err)
		}
		if !wasCreated || ensured.Address != "replay@abrolia.com" {
			t.Fatalf("ensured=%+v created=%t", ensured, wasCreated)
		}

		replayed, wasCreated, err := st.EnsureInboxForOrg(
			ctx, orgID, "REPLAY@ABROLIA.COM.", "", "resend", "canonical-address-replay",
		)
		if err != nil {
			t.Fatal(err)
		}
		if wasCreated || replayed.ID != ensured.ID || replayed.Address != "replay@abrolia.com" {
			t.Fatalf("replayed=%+v created=%t; want id=%s", replayed, wasCreated, ensured.ID)
		}

		rows, err := db.QueryContext(ctx, `SELECT address FROM inboxes WHERE org_id=$1 ORDER BY address`, orgID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var addresses []string
		for rows.Next() {
			var address string
			if err := rows.Scan(&address); err != nil {
				t.Fatal(err)
			}
			addresses = append(addresses, address)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if strings.Join(addresses, ",") != "family@abrolia.com,replay@abrolia.com" {
			t.Fatalf("stored addresses=%v", addresses)
		}
	})
}
