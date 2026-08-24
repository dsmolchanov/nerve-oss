package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

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

func TestCanonicalInboxAddressSQLMatchesGoCanonicalization(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		whitespace := []rune{
			'\t', '\n', '\v', '\f', '\r', ' ', '\u0085', '\u00a0', '\u1680',
			'\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006',
			'\u2007', '\u2008', '\u2009', '\u200a', '\u2028', '\u2029', '\u202f',
			'\u205f', '\u3000',
		}
		inputs := []string{"Mailbox@Example.TEST", "MAILBOX@EXAMPLE.TEST."}
		for _, r := range whitespace {
			inputs = append(inputs, string(r)+"MAILBOX@EXAMPLE.TEST."+string(r))
		}

		for index, input := range inputs {
			want, _, err := canonicalInboxAddress(input)
			if err != nil {
				t.Fatalf("Go canonicalization %d %q: %v", index, input, err)
			}
			var got string
			query := `SELECT ` + canonicalInboxAddressSQL("$1::text")
			if err := db.QueryRowContext(ctx, query, input).Scan(&got); err != nil {
				t.Fatalf("SQL canonicalization %d %q: %v", index, input, err)
			}
			if got != want {
				t.Fatalf("SQL canonicalization %d %q=%q, want %q", index, input, got, want)
			}
		}

		var doubleDot string
		query := `SELECT ` + canonicalInboxAddressSQL("$1::text")
		if err := db.QueryRowContext(ctx, query, "mailbox@example.test..").Scan(&doubleDot); err != nil {
			t.Fatal(err)
		}
		if doubleDot == "mailbox@example.test" {
			t.Fatal("SQL expression removed more than one trailing domain dot")
		}
	})
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

func TestLegacyCanonicalInboxAddressMatrix(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "legacy-canonical-matrix-owner")
		if err != nil {
			t.Fatal(err)
		}
		domainID, err := st.CreateOrgDomain(
			ctx, orgID, "legacy-inbox.example.test", "verify", "selector", "private", "public", "cname",
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

		type boundary struct {
			name         string
			needsDomain  bool
			wantConflict bool
			call         func(context.Context, *Store, string, string, string, string) (string, error)
		}
		boundaries := []boundary{
			{
				name: "GetInboxByAddress",
				call: func(ctx context.Context, st *Store, _, address, _, _ string) (string, error) {
					rec, err := st.GetInboxByAddress(ctx, address)
					return rec.ID, err
				},
			},
			{
				name:        "ResolveReceivingInbox",
				needsDomain: true,
				call: func(ctx context.Context, st *Store, _, address, _, _ string) (string, error) {
					rec, _, err := st.ResolveReceivingInbox(ctx, address)
					return rec.ID, err
				},
			},
			{
				name: "EnsureInbox",
				call: func(ctx context.Context, st *Store, _, address, _, _ string) (string, error) {
					return st.EnsureInbox(ctx, address)
				},
			},
			{
				name: "EnsureDefaultInbox",
				call: func(ctx context.Context, st *Store, _, address, _, _ string) (string, error) {
					return st.EnsureDefaultInbox(ctx, address)
				},
			},
			{
				name:         "CreateInboxForOrg",
				wantConflict: true,
				call: func(ctx context.Context, st *Store, orgID, address, domainID, _ string) (string, error) {
					rec, err := st.CreateInboxForOrg(ctx, orgID, address, domainID)
					return rec.ID, err
				},
			},
			{
				name: "EnsureInboxForOrg",
				call: func(ctx context.Context, st *Store, orgID, address, domainID, externalRef string) (string, error) {
					rec, created, err := st.EnsureInboxForOrg(ctx, orgID, address, domainID, "smtp", externalRef)
					if err == nil && created {
						return "", errors.New("canonical legacy replay created a new inbox")
					}
					return rec.ID, err
				},
			},
		}
		spellings := []struct {
			name   string
			stored func(string) string
			input  func(string) string
		}{
			{name: "case", stored: strings.ToUpper, input: func(value string) string { return value }},
			{
				name:   "outer-unicode-whitespace",
				stored: func(value string) string { return "\u00a0\u2003" + value + "\u3000" },
				input:  func(value string) string { return strings.ToUpper(value) + "." },
			},
			{
				name:   "one-trailing-domain-dot",
				stored: func(value string) string { return value + "." },
				input:  func(value string) string { return "\u202f" + strings.ToUpper(value) + "\u205f" },
			},
		}

		for _, boundary := range boundaries {
			t.Run(boundary.name, func(t *testing.T) {
				for _, spelling := range spellings {
					t.Run(spelling.name, func(t *testing.T) {
						canonical := fmt.Sprintf(
							"%s-%s@legacy-inbox.example.test",
							strings.ToLower(boundary.name), spelling.name,
						)
						stored := spelling.stored(canonical)
						inboxID := uuid.NewString()
						externalRef := "legacy-canonical-matrix:" + strings.ToLower(boundary.name) + ":" + spelling.name
						var storedDomainID any
						callDomainID := ""
						if boundary.needsDomain {
							storedDomainID = domainID
							callDomainID = domainID
						}
						if _, err := db.ExecContext(ctx, `
							INSERT INTO inboxes
							  (id, org_id, org_domain_id, address, status, outbound_provider, external_ref)
							VALUES ($1, $2, $3, $4, 'active', 'smtp', $5)
						`, inboxID, orgID, storedDomainID, stored, externalRef); err != nil {
							t.Fatal(err)
						}

						gotID, callErr := boundary.call(
							ctx, st, orgID, spelling.input(canonical), callDomainID, externalRef,
						)
						if boundary.wantConflict {
							if !errors.Is(callErr, ErrResourceConflict) {
								t.Fatalf("error=%v, want ErrResourceConflict", callErr)
							}
						} else if callErr != nil {
							t.Fatal(callErr)
						} else if gotID != inboxID {
							t.Fatalf("inbox id=%s, want legacy id=%s", gotID, inboxID)
						}

						var persisted string
						if err := db.QueryRowContext(ctx, `SELECT address FROM inboxes WHERE id = $1`, inboxID).Scan(&persisted); err != nil {
							t.Fatal(err)
						}
						if persisted != stored {
							t.Fatalf("stored address=%q, want original bytes %q", persisted, stored)
						}
					})
				}
			})
		}
	})
}

func TestCanonicalInboxDisabledHistorySemantics(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		firstOrgID, err := st.CreateOrg(ctx, "canonical-disabled-history-first")
		if err != nil {
			t.Fatal(err)
		}
		secondOrgID, err := st.CreateOrg(ctx, "canonical-disabled-history-second")
		if err != nil {
			t.Fatal(err)
		}

		for index, ensure := range []struct {
			name string
			call func(string) (string, error)
		}{
			{name: "EnsureInbox", call: func(address string) (string, error) {
				return st.EnsureInbox(ctx, address)
			}},
			{name: "EnsureDefaultInbox", call: func(address string) (string, error) {
				return st.EnsureDefaultInbox(ctx, address)
			}},
		} {
			t.Run(ensure.name+"/prefers-active", func(t *testing.T) {
				canonical := fmt.Sprintf("preferred-%d@example.test", index)
				disabledID := uuid.NewString()
				activeID := uuid.NewString()
				if _, err := db.ExecContext(ctx, `
					INSERT INTO inboxes (id, org_id, address, status)
					VALUES ($1, $2, $3, 'disabled'), ($4, $2, $5, 'active')
				`, disabledID, firstOrgID, strings.ToUpper(canonical), activeID, canonical+"."); err != nil {
					t.Fatal(err)
				}
				gotID, err := ensure.call("\u00a0" + strings.ToUpper(canonical) + "\u3000")
				if err != nil || gotID != activeID {
					t.Fatalf("id=%s err=%v, want active id=%s", gotID, err, activeID)
				}
			})

			t.Run(ensure.name+"/replays-disabled-when-no-active", func(t *testing.T) {
				canonical := fmt.Sprintf("disabled-replay-%d@example.test", index)
				disabledID := uuid.NewString()
				legacyAddress := "\u2003" + strings.ToUpper(canonical) + ".\u202f"
				if _, err := db.ExecContext(ctx, `
					INSERT INTO inboxes (id, org_id, address, status)
					VALUES ($1, $2, $3, 'disabled')
				`, disabledID, firstOrgID, legacyAddress); err != nil {
					t.Fatal(err)
				}
				gotID, err := ensure.call(canonical)
				if err != nil || gotID != disabledID {
					t.Fatalf("id=%s err=%v, want disabled id=%s", gotID, err, disabledID)
				}
				var persisted string
				if err := db.QueryRowContext(ctx, `SELECT address FROM inboxes WHERE id = $1`, disabledID).Scan(&persisted); err != nil {
					t.Fatal(err)
				}
				if persisted != legacyAddress {
					t.Fatalf("disabled replay rewrote address=%q, want %q", persisted, legacyAddress)
				}
			})
		}

		t.Run("CreateInboxForOrg/disabled-different-ref-does-not-reserve", func(t *testing.T) {
			canonical := "create-disabled@example.test"
			if _, err := db.ExecContext(ctx, `
				INSERT INTO inboxes (id, org_id, address, status, external_ref)
				VALUES ($1, $2, $3, 'disabled', 'disabled:create:history')
			`, uuid.NewString(), firstOrgID, strings.ToUpper(canonical)+"."); err != nil {
				t.Fatal(err)
			}
			created, err := st.CreateInboxForOrg(ctx, secondOrgID, canonical, "")
			if err != nil || created.Status != "active" {
				t.Fatalf("created=%+v err=%v, want new active inbox", created, err)
			}
		})

		t.Run("EnsureInboxForOrg/disabled-same-ref-replays", func(t *testing.T) {
			canonical := "ensure-disabled-replay@example.test"
			disabledID := uuid.NewString()
			externalRef := "disabled:ensure:replay"
			legacyAddress := "\u00a0" + strings.ToUpper(canonical) + ".\u3000"
			if _, err := db.ExecContext(ctx, `
				INSERT INTO inboxes (id, org_id, address, status, outbound_provider, external_ref)
				VALUES ($1, $2, $3, 'disabled', 'smtp', $4)
			`, disabledID, firstOrgID, legacyAddress, externalRef); err != nil {
				t.Fatal(err)
			}
			replayed, created, err := st.EnsureInboxForOrg(
				ctx, firstOrgID, canonical, "", "smtp", externalRef,
			)
			if err != nil || created || replayed.ID != disabledID || replayed.Status != "disabled" {
				t.Fatalf("replayed=%+v created=%t err=%v, want disabled id=%s", replayed, created, err, disabledID)
			}
		})

		t.Run("EnsureInboxForOrg/disabled-different-ref-does-not-reserve", func(t *testing.T) {
			canonical := "ensure-disabled-new@example.test"
			if _, err := db.ExecContext(ctx, `
				INSERT INTO inboxes (id, org_id, address, status, outbound_provider, external_ref)
				VALUES ($1, $2, $3, 'disabled', 'smtp', 'disabled:ensure:history')
			`, uuid.NewString(), firstOrgID, strings.ToUpper(canonical)+"."); err != nil {
				t.Fatal(err)
			}
			createdInbox, created, err := st.EnsureInboxForOrg(
				ctx, secondOrgID, canonical, "", "smtp", "disabled:ensure:new",
			)
			if err != nil || !created || createdInbox.Status != "active" {
				t.Fatalf("inbox=%+v created=%t err=%v, want new active inbox", createdInbox, created, err)
			}
		})
	})
}

func TestInboxInsertHelpersRequireTransaction(t *testing.T) {
	st := &Store{}
	if _, err := st.createInboxForOrg(context.Background(), "org", "a@example.test", "", "smtp", ""); err == nil || !strings.Contains(err.Error(), "requires an explicit transaction") {
		t.Fatalf("createInboxForOrg error=%v, want transaction requirement", err)
	}
	if _, err := st.createInboxForOrgWithID(context.Background(), uuid.NewString(), "org", "a@example.test", "", "smtp", ""); err == nil || !strings.Contains(err.Error(), "requires an explicit transaction") {
		t.Fatalf("createInboxForOrgWithID error=%v, want transaction requirement", err)
	}
	if _, _, err := st.ensureInboxForOrgLocked(context.Background(), "org", "a@example.test", "", "smtp", "ref"); err == nil || !strings.Contains(err.Error(), "requires an explicit transaction") {
		t.Fatalf("ensureInboxForOrgLocked error=%v, want transaction requirement", err)
	}
}

func TestCanonicalInboxAddressBoundariesRejectAmbiguousLegacyRows(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "ambiguous-canonical-owner")
		if err != nil {
			t.Fatal(err)
		}
		domainID, err := st.CreateOrgDomain(
			ctx, orgID, "ambiguous.example.test", "verify", "selector", "private", "public", "cname",
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

		boundaries := []struct {
			name        string
			needsDomain bool
			call        func(string, string, string) error
		}{
			{
				name: "GetInboxByAddress",
				call: func(address, _, _ string) error {
					_, err := st.GetInboxByAddress(ctx, address)
					return err
				},
			},
			{
				name:        "ResolveReceivingInbox",
				needsDomain: true,
				call: func(address, _, _ string) error {
					_, _, err := st.ResolveReceivingInbox(ctx, address)
					return err
				},
			},
			{
				name: "EnsureInbox",
				call: func(address, _, _ string) error {
					_, err := st.EnsureInbox(ctx, address)
					return err
				},
			},
			{
				name: "EnsureDefaultInbox",
				call: func(address, _, _ string) error {
					_, err := st.EnsureDefaultInbox(ctx, address)
					return err
				},
			},
			{
				name: "CreateInboxForOrg",
				call: func(address, domainID, _ string) error {
					_, err := st.CreateInboxForOrg(ctx, orgID, address, domainID)
					return err
				},
			},
			{
				name: "EnsureInboxForOrg",
				call: func(address, domainID, externalRef string) error {
					_, _, err := st.EnsureInboxForOrg(ctx, orgID, address, domainID, "smtp", externalRef)
					return err
				},
			},
		}

		for _, boundary := range boundaries {
			t.Run(boundary.name, func(t *testing.T) {
				canonical := strings.ToLower(boundary.name) + "@ambiguous.example.test"
				var rowDomainID any
				callDomainID := ""
				if boundary.needsDomain {
					rowDomainID = domainID
					callDomainID = domainID
				}
				for index, stored := range []string{strings.ToUpper(canonical), canonical + "."} {
					if _, err := db.ExecContext(ctx, `
						INSERT INTO inboxes
						  (id, org_id, org_domain_id, address, status, outbound_provider, external_ref)
						VALUES ($1, $2, $3, $4, 'active', 'smtp', $5)
					`, uuid.NewString(), orgID, rowDomainID, stored,
						fmt.Sprintf("ambiguous:%s:%d", strings.ToLower(boundary.name), index)); err != nil {
						t.Fatal(err)
					}
				}
				err := boundary.call(canonical, callDomainID, "ambiguous:new:"+strings.ToLower(boundary.name))
				if !errors.Is(err, ErrCanonicalInboxAddressAmbiguous) {
					t.Fatalf("error=%v, want ErrCanonicalInboxAddressAmbiguous", err)
				}
			})
		}
	})
}

func TestResolveReceivingInboxRejectsAmbiguityBeforeReadinessFilter(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "receiving-ambiguity-before-readiness")
		if err != nil {
			t.Fatal(err)
		}
		domainID, err := st.CreateOrgDomain(
			ctx, orgID, "receiving-ambiguity.example.test", "verify", "selector", "private", "public", "cname",
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
		canonical := "route@receiving-ambiguity.example.test"
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, org_domain_id, address, status)
			VALUES ($1, $2, $3, $4, 'active'), ($5, $2, NULL, $6, 'active')
		`, uuid.NewString(), orgID, domainID, canonical, uuid.NewString(), canonical+"."); err != nil {
			t.Fatal(err)
		}

		_, _, err = st.ResolveReceivingInbox(ctx, canonical)
		if !errors.Is(err, ErrCanonicalInboxAddressAmbiguous) {
			t.Fatalf("error=%v, want ambiguity before the unlinked candidate is filtered", err)
		}
	})
}

func TestReactivateInboxForOrgUsesCanonicalIdentityWithoutRewritingLegacyBytes(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "reactivate-canonical-owner")
		if err != nil {
			t.Fatal(err)
		}
		legacyID := uuid.NewString()
		legacyAddress := "\u00a0Reactivate@Example.TEST.\u3000"
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, $3, 'disabled')
		`, legacyID, orgID, legacyAddress); err != nil {
			t.Fatal(err)
		}

		changed, err := st.ReactivateInboxForOrg(ctx, orgID, legacyID)
		if err != nil || !changed {
			t.Fatalf("first reactivation changed=%t err=%v", changed, err)
		}
		var storedAddress, status string
		if err := db.QueryRowContext(ctx, `SELECT address, status FROM inboxes WHERE id = $1`, legacyID).Scan(&storedAddress, &status); err != nil {
			t.Fatal(err)
		}
		if storedAddress != legacyAddress || status != "active" {
			t.Fatalf("reactivated address/status=%q/%q, want original bytes/active", storedAddress, status)
		}

		if _, err := db.ExecContext(ctx, `UPDATE inboxes SET status = 'disabled' WHERE id = $1`, legacyID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'reactivate@example.test', 'active')
		`, uuid.NewString(), orgID); err != nil {
			t.Fatal(err)
		}
		changed, err = st.ReactivateInboxForOrg(ctx, orgID, legacyID)
		if changed || !errors.Is(err, ErrResourceConflict) {
			t.Fatalf("conflicting reactivation changed=%t err=%v, want resource conflict", changed, err)
		}
		if err := db.QueryRowContext(ctx, `SELECT address, status FROM inboxes WHERE id = $1`, legacyID).Scan(&storedAddress, &status); err != nil {
			t.Fatal(err)
		}
		if storedAddress != legacyAddress || status != "disabled" {
			t.Fatalf("failed reactivation changed stored row to %q/%q", storedAddress, status)
		}
	})
}

func TestReactivateInboxForOrgMapsCanonicalUniqueBackstop(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "reactivate-canonical-index-backstop")
		if err != nil {
			t.Fatal(err)
		}
		inboxID := uuid.NewString()
		if _, err := db.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, 'backstop@example.test', 'disabled')
		`, inboxID, orgID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `
			CREATE FUNCTION insert_reactivate_canonical_conflict() RETURNS trigger AS $$
			BEGIN
				IF OLD.id = '`+inboxID+`'::uuid AND NEW.status = 'active' THEN
					INSERT INTO inboxes (id, org_id, address, status)
					VALUES (gen_random_uuid(), NEW.org_id, lower(NEW.address), 'active');
				END IF;
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER insert_reactivate_canonical_conflict
			BEFORE UPDATE OF status ON inboxes
			FOR EACH ROW EXECUTE FUNCTION insert_reactivate_canonical_conflict();
		`); err != nil {
			t.Fatal(err)
		}

		changed, err := st.ReactivateInboxForOrg(ctx, orgID, inboxID)
		if changed || !errors.Is(err, ErrResourceConflict) {
			t.Fatalf("changed=%t err=%v, want canonical resource conflict", changed, err)
		}
		var status string
		if err := db.QueryRowContext(ctx, `SELECT status FROM inboxes WHERE id = $1`, inboxID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "disabled" {
			t.Fatalf("status=%q, want disabled after failed reactivation", status)
		}
	})
}

func waitForCanonicalDomainLockWaiter(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var waiters int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND wait_event = 'advisory'
		`).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("canonical variant writer did not wait on the advisory domain lock")
}

func TestCanonicalVariantWritersSerializeBeforePreinsertCheck(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}
		orgID, err := st.CreateOrg(ctx, "canonical-writer-serialization-owner")
		if err != nil {
			t.Fatal(err)
		}

		tests := []struct {
			name   string
			first  func(*Store, string) (string, error)
			second func(string) (string, error)
		}{
			{
				name: "CreateInboxForOrg",
				first: func(scoped *Store, address string) (string, error) {
					rec, err := scoped.CreateInboxForOrg(ctx, orgID, address, "")
					return rec.ID, err
				},
				second: func(address string) (string, error) {
					rec, err := st.CreateInboxForOrg(ctx, orgID, address, "")
					return rec.ID, err
				},
			},
			{
				name: "EnsureInbox",
				first: func(scoped *Store, address string) (string, error) {
					return scoped.EnsureInbox(ctx, address)
				},
				second: func(address string) (string, error) {
					return st.EnsureInbox(ctx, address)
				},
			},
			{
				name: "EnsureDefaultInbox",
				first: func(scoped *Store, address string) (string, error) {
					return scoped.EnsureDefaultInbox(ctx, address)
				},
				second: func(address string) (string, error) {
					return st.EnsureDefaultInbox(ctx, address)
				},
			},
			{
				name: "EnsureInboxForOrg",
				first: func(scoped *Store, address string) (string, error) {
					rec, _, err := scoped.EnsureInboxForOrg(ctx, orgID, address, "", "smtp", "serialization:first")
					return rec.ID, err
				},
				second: func(address string) (string, error) {
					rec, _, err := st.EnsureInboxForOrg(ctx, orgID, address, "", "smtp", "serialization:second")
					return rec.ID, err
				},
			},
		}

		for index, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				canonical := fmt.Sprintf("serialization-%d@example.test", index)
				_, domain, err := canonicalInboxAddress(canonical)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				scoped := &Store{db: db, q: tx, inTx: true}
				if err := scoped.lockCanonicalDomain(ctx, domain); err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}

				type result struct {
					id  string
					err error
				}
				secondResult := make(chan result, 1)
				go func() {
					id, err := test.second("\u00a0" + strings.ToUpper(canonical) + ".\u3000")
					secondResult <- result{id: id, err: err}
				}()
				waitForCanonicalDomainLockWaiter(t, ctx, db)

				firstID, err := test.first(scoped, canonical)
				if err != nil {
					_ = tx.Rollback()
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}

				select {
				case got := <-secondResult:
					if test.name == "CreateInboxForOrg" || test.name == "EnsureInboxForOrg" {
						if !errors.Is(got.err, ErrResourceConflict) {
							t.Fatalf("second writer error=%v, want resource conflict", got.err)
						}
					} else if got.err != nil || got.id != firstID {
						t.Fatalf("second writer id=%s err=%v, want id=%s", got.id, got.err, firstID)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("second canonical writer did not finish after lock release")
				}

				var count int
				if err := db.QueryRowContext(ctx, `
					SELECT count(*) FROM inboxes WHERE `+canonicalInboxAddressSQL("address")+` = $1
				`, canonical).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != 1 {
					t.Fatalf("canonical writer row count=%d, want 1", count)
				}
			})
		}
	})
}

func waitForAdvisoryWaiters(t *testing.T, ctx context.Context, db *sql.DB, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var waiters int
		if err := db.QueryRowContext(ctx, `
			SELECT count(*)
			FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND wait_event = 'advisory'
		`).Scan(&waiters); err != nil {
			t.Fatal(err)
		}
		if waiters >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("advisory waiters did not reach %d", want)
}

func assertCanonicalDomainLockHeld(t *testing.T, ctx context.Context, db *sql.DB, domain string) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var acquired bool
	if err := tx.QueryRowContext(ctx, `
		SELECT pg_try_advisory_xact_lock(hashtextextended('canonical-domain:' || $1, 0))
	`, domain).Scan(&acquired); err != nil {
		t.Fatal(err)
	}
	if acquired {
		t.Fatalf("canonical domain lock %q was not held before the status trigger", domain)
	}
}

func installSyntheticCloudInboxStatusTrigger(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE inbox_lifecycle_test_pauses (
		  inbox_id uuid PRIMARY KEY,
		  gate_key text NOT NULL
		);
		CREATE FUNCTION pause_inbox_lifecycle_test() RETURNS trigger AS $$
		DECLARE
		  selected_gate text;
		BEGIN
		  SELECT gate_key INTO selected_gate
		  FROM inbox_lifecycle_test_pauses
		  WHERE inbox_id = NEW.id;
		  IF selected_gate IS NOT NULL THEN
		    PERFORM pg_advisory_xact_lock(hashtextextended(selected_gate, 0));
		  END IF;
		  RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER aaa_pause_inbox_lifecycle_test
		  BEFORE UPDATE OF status ON inboxes
		  FOR EACH ROW EXECUTE FUNCTION pause_inbox_lifecycle_test();

		CREATE FUNCTION synthetic_cloud_inbox_status_domain_lock_test() RETURNS trigger AS $$
		DECLARE
		  candidate_domain text;
		  linked_domain text;
		BEGIN
		  candidate_domain := lower(rtrim(reverse(split_part(reverse(
		    btrim(NEW.address, U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000')
		  ), '@', 1)), '.'));
		  linked_domain := '';
		  IF NEW.org_domain_id IS NOT NULL THEN
		    SELECT domain INTO linked_domain FROM org_domains WHERE id = NEW.org_domain_id;
		    IF linked_domain IS NOT NULL THEN
		      linked_domain := lower(rtrim(
		        btrim(linked_domain, U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000'), '.'
		      ));
		    END IF;
		  END IF;
		  IF candidate_domain <> '' AND linked_domain <> '' AND candidate_domain <> linked_domain THEN
		    IF candidate_domain COLLATE "C" < linked_domain COLLATE "C" THEN
		      PERFORM pg_advisory_xact_lock(hashtextextended('canonical-domain:' || candidate_domain, 0));
		      PERFORM pg_advisory_xact_lock(hashtextextended('canonical-domain:' || linked_domain, 0));
		    ELSE
		      PERFORM pg_advisory_xact_lock(hashtextextended('canonical-domain:' || linked_domain, 0));
		      PERFORM pg_advisory_xact_lock(hashtextextended('canonical-domain:' || candidate_domain, 0));
		    END IF;
		  ELSIF candidate_domain <> '' THEN
		    PERFORM pg_advisory_xact_lock(hashtextextended('canonical-domain:' || candidate_domain, 0));
		  ELSE
		    PERFORM pg_advisory_xact_lock(hashtextextended('canonical-domain:' || linked_domain, 0));
		  END IF;
		  RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER zzz_synthetic_cloud_inbox_status_domain_lock_test
		  BEFORE UPDATE OF status ON inboxes
		  FOR EACH ROW EXECUTE FUNCTION synthetic_cloud_inbox_status_domain_lock_test();
	`); err != nil {
		t.Fatal(err)
	}
}

func TestInboxLifecycleWritersUseCanonicalDomainBeforeOrgPolicy(t *testing.T) {
	tests := []struct {
		name       string
		pauseFirst func(activeID, disabledID string) string
		first      func(context.Context, *Store, string, string, string) (bool, error)
		second     func(context.Context, *Store, string, string, string) (bool, error)
	}{
		{
			name:       "disable commits first",
			pauseFirst: func(activeID, _ string) string { return activeID },
			first: func(ctx context.Context, st *Store, orgID, activeID, _ string) (bool, error) {
				return st.DisableInboxForOrg(ctx, orgID, activeID)
			},
			second: func(ctx context.Context, st *Store, orgID, _, disabledID string) (bool, error) {
				return st.ReactivateInboxForOrg(ctx, orgID, disabledID)
			},
		},
		{
			name:       "reactivation commits first",
			pauseFirst: func(_, disabledID string) string { return disabledID },
			first: func(ctx context.Context, st *Store, orgID, _, disabledID string) (bool, error) {
				return st.ReactivateInboxForOrg(ctx, orgID, disabledID)
			},
			second: func(ctx context.Context, st *Store, orgID, activeID, _ string) (bool, error) {
				return st.DisableInboxForOrg(ctx, orgID, activeID)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				migrateToLatest(t, ctx, db)
				st := &Store{db: db, q: db}
				orgID, err := st.CreateOrg(ctx, "inbox-lifecycle-lock-order-"+strings.ReplaceAll(test.name, " ", "-"))
				if err != nil {
					t.Fatal(err)
				}
				activeID := uuid.NewString()
				disabledID := uuid.NewString()
				linkedDomain := "a-linked-lifecycle-lock.example"
				addressDomain := "z-address-lifecycle-lock.example"
				linkedDomainID := uuid.NewString()
				if _, err := db.ExecContext(ctx, `
					INSERT INTO org_domains
					  (id, org_id, domain, status, verification_token, dkim_selector, dkim_method, verified_at)
					VALUES ($1, $2, $3, 'active', 'lock-order-token', 'nerve', 'cname', now())
				`, linkedDomainID, orgID, linkedDomain); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, `
					INSERT INTO inboxes (id, org_id, org_domain_id, address, status)
					VALUES ($1, $2, $3, $4, 'active'),
					       ($5, $2, $3, $6, 'disabled')
				`, activeID, orgID, linkedDomainID, "active@"+addressDomain, disabledID, "disabled@"+addressDomain); err != nil {
					t.Fatal(err)
				}
				installSyntheticCloudInboxStatusTrigger(t, ctx, db)

				testCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				gateKey := "inbox-lifecycle-test-gate:" + uuid.NewString()
				if _, err := db.ExecContext(testCtx, `
					INSERT INTO inbox_lifecycle_test_pauses (inbox_id, gate_key)
					VALUES ($1, $2)
				`, test.pauseFirst(activeID, disabledID), gateKey); err != nil {
					t.Fatal(err)
				}
				gateTx, err := db.BeginTx(testCtx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer gateTx.Rollback()
				if _, err := gateTx.ExecContext(testCtx, `
					SELECT pg_advisory_xact_lock(hashtextextended($1, 0))
				`, gateKey); err != nil {
					t.Fatal(err)
				}

				type lifecycleResult struct {
					changed bool
					err     error
				}
				firstDone := make(chan lifecycleResult, 1)
				go func() {
					changed, err := test.first(testCtx, st, orgID, activeID, disabledID)
					firstDone <- lifecycleResult{changed: changed, err: err}
				}()
				waitForAdvisoryWaiters(t, testCtx, db, 1)
				assertCanonicalDomainLockHeld(t, testCtx, db, linkedDomain)
				assertCanonicalDomainLockHeld(t, testCtx, db, addressDomain)

				secondDone := make(chan lifecycleResult, 1)
				go func() {
					changed, err := test.second(testCtx, st, orgID, activeID, disabledID)
					secondDone <- lifecycleResult{changed: changed, err: err}
				}()
				waitForAdvisoryWaiters(t, testCtx, db, 2)

				if err := gateTx.Commit(); err != nil {
					t.Fatal(err)
				}
				for index, done := range []<-chan lifecycleResult{firstDone, secondDone} {
					select {
					case result := <-done:
						if result.err != nil || !result.changed {
							t.Fatalf("writer %d changed=%t err=%v", index+1, result.changed, result.err)
						}
					case <-testCtx.Done():
						t.Fatalf("writer %d did not finish: %v", index+1, testCtx.Err())
					}
				}

				var activeStatus, disabledStatus string
				if err := db.QueryRowContext(testCtx, `
					SELECT a.status, d.status
					FROM inboxes a CROSS JOIN inboxes d
					WHERE a.id = $1 AND d.id = $2
				`, activeID, disabledID).Scan(&activeStatus, &disabledStatus); err != nil {
					t.Fatal(err)
				}
				if activeStatus != "disabled" || disabledStatus != "active" {
					t.Fatalf("final statuses=%s/%s, want disabled/active", activeStatus, disabledStatus)
				}
			})
		})
	}
}
