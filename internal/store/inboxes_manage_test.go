package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/google/uuid"
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

func TestInboxAddressSiblingBoundariesRejectInvalidAddressBeforeDatabaseAccess(t *testing.T) {
	st := &Store{}
	tests := []struct {
		name string
		call func(string) error
	}{
		{
			name: "GetInboxByAddress",
			call: func(address string) error {
				_, err := st.GetInboxByAddress(context.Background(), address)
				return err
			},
		},
		{
			name: "EnsureInbox",
			call: func(address string) error {
				_, err := st.EnsureInbox(context.Background(), address)
				return err
			},
		},
		{
			name: "EnsureDefaultInbox",
			call: func(address string) error {
				_, err := st.EnsureDefaultInbox(context.Background(), address)
				return err
			},
		},
		{
			name: "ResolveReceivingInbox",
			call: func(address string) error {
				_, _, err := st.ResolveReceivingInbox(context.Background(), address)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, address := range []string{
				"family@extra@abrolia.com",
				"family@",
				"family name@abrolia.com",
				"family@abrolia.com/path",
			} {
				if err := test.call(address); err == nil || strings.Contains(err.Error(), "database") {
					t.Fatalf("%s(%q) error=%v", test.name, address, err)
				}
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

func TestInboxAddressBoundariesCanonicalizeEquivalentSpellings(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "canonical-boundary-owner")
		if err != nil {
			t.Fatal(err)
		}

		lookupInbox, err := st.CreateInboxForOrg(ctx, orgID, "lookup@abrolia.com", "")
		if err != nil {
			t.Fatal(err)
		}
		ensureID, err := st.EnsureInbox(ctx, "ensure@abrolia.com")
		if err != nil {
			t.Fatal(err)
		}
		defaultID, err := st.EnsureDefaultInbox(ctx, "default@abrolia.com")
		if err != nil {
			t.Fatal(err)
		}

		domainID, err := st.CreateOrgDomain(
			ctx, orgID, "receiving.abrolia.com", "verify", "selector", "private", "public", "cname",
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOrgDomainStatus(ctx, domainID, "active"); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateOrgDomainResendReceiving(ctx, domainID, true); err != nil {
			t.Fatal(err)
		}
		receivingInbox, err := st.CreateInboxForOrg(ctx, orgID, "route@receiving.abrolia.com", domainID)
		if err != nil {
			t.Fatal(err)
		}

		boundaries := []struct {
			name      string
			canonical string
			wantID    string
			lookup    func(string) (string, error)
		}{
			{
				name:      "GetInboxByAddress",
				canonical: "lookup@abrolia.com",
				wantID:    lookupInbox.ID,
				lookup: func(address string) (string, error) {
					rec, err := st.GetInboxByAddress(ctx, address)
					return rec.ID, err
				},
			},
			{
				name:      "EnsureInbox",
				canonical: "ensure@abrolia.com",
				wantID:    ensureID,
				lookup: func(address string) (string, error) {
					return st.EnsureInbox(ctx, address)
				},
			},
			{
				name:      "EnsureDefaultInbox",
				canonical: "default@abrolia.com",
				wantID:    defaultID,
				lookup: func(address string) (string, error) {
					return st.EnsureDefaultInbox(ctx, address)
				},
			},
			{
				name:      "ResolveReceivingInbox",
				canonical: "route@receiving.abrolia.com",
				wantID:    receivingInbox.ID,
				lookup: func(address string) (string, error) {
					rec, _, err := st.ResolveReceivingInbox(ctx, address)
					return rec.ID, err
				},
			},
		}
		spellings := []struct {
			name  string
			apply func(string) string
		}{
			{name: "case", apply: strings.ToUpper},
			{name: "whitespace", apply: func(address string) string { return "  " + address + "  " }},
			{name: "trailing-dot", apply: func(address string) string { return address + "." }},
		}

		for _, boundary := range boundaries {
			t.Run(boundary.name, func(t *testing.T) {
				for _, spelling := range spellings {
					t.Run(spelling.name, func(t *testing.T) {
						gotID, err := boundary.lookup(spelling.apply(boundary.canonical))
						if err != nil {
							t.Fatal(err)
						}
						if gotID != boundary.wantID {
							t.Fatalf("inbox id=%s, want %s", gotID, boundary.wantID)
						}
					})
				}
			})
		}

		var inboxCount int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM inboxes`).Scan(&inboxCount); err != nil {
			t.Fatal(err)
		}
		if inboxCount != len(boundaries) {
			t.Fatalf("inbox count=%d, want %d; a canonical replay inserted a duplicate", inboxCount, len(boundaries))
		}
	})
}

func TestEnsureInboxForOrgReplaysLegacyNonCanonicalStoredAddress(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "legacy-canonical-replay-owner")
		if err != nil {
			t.Fatal(err)
		}
		legacyRows := []struct {
			name      string
			stored    string
			canonical string
		}{
			{name: "case", stored: "Case@Abrolia.COM", canonical: "case@abrolia.com"},
			{name: "whitespace", stored: "  whitespace@abrolia.com  ", canonical: "whitespace@abrolia.com"},
			{name: "trailing-dot", stored: "dot@abrolia.com.", canonical: "dot@abrolia.com"},
		}

		for _, legacy := range legacyRows {
			t.Run(legacy.name, func(t *testing.T) {
				inboxID := uuid.NewString()
				externalRef := "legacy-canonical-address-replay:" + legacy.name
				if _, err := db.ExecContext(ctx, `
					INSERT INTO inboxes (id, org_id, address, status, outbound_provider, external_ref)
					VALUES ($1, $2, $3, 'active', 'smtp', $4)
				`, inboxID, orgID, legacy.stored, externalRef); err != nil {
					t.Fatal(err)
				}

				for _, address := range []string{
					legacy.canonical,
					strings.ToUpper(legacy.canonical),
					"  " + legacy.canonical + "  ",
					legacy.canonical + ".",
				} {
					replayed, created, err := st.EnsureInboxForOrg(
						ctx, orgID, address, "", "smtp", externalRef,
					)
					if err != nil {
						t.Fatalf("replay %q: %v", address, err)
					}
					if created || replayed.ID != inboxID {
						t.Fatalf("replay %q returned id=%s created=%t, want id=%s created=false", address, replayed.ID, created, inboxID)
					}
				}

				var persistedAddress string
				if err := db.QueryRowContext(ctx, `SELECT address FROM inboxes WHERE id = $1`, inboxID).Scan(&persistedAddress); err != nil {
					t.Fatal(err)
				}
				if persistedAddress != legacy.stored {
					t.Fatalf("legacy address was rewritten to %q, want original %q", persistedAddress, legacy.stored)
				}
			})
		}
	})
}
