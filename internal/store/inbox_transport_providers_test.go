package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
)

// Outside cloud mode the inbox row-level policy deliberately does not filter
// by tenant, so an update keyed on the inbox UUID alone would let one
// organization reroute another's mailbox to a provider of its choosing —
// including through Cloud, where its mail would then be carried by somebody
// else's installation. The organization has to be part of the predicate.
func TestUpdateInboxProvidersRefusesAnotherOrganizationsMailbox(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		migrateToLatest(t, ctx, db)
		st := &Store{db: db, q: db}

		type tenant struct{ org, inbox string }
		tenants := make([]tenant, 2)
		for index := range tenants {
			tenants[index] = tenant{org: uuid.NewString(), inbox: uuid.NewString()}
			if _, err := db.ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`,
				tenants[index].org, "tenant-"+tenants[index].org); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO inboxes (id, org_id, address, status, inbound_provider, outbound_provider)
				 VALUES ($1, $2, $3, 'active', 'jmap', 'smtp')`,
				tenants[index].inbox, tenants[index].org, tenants[index].inbox+"@test.example"); err != nil {
				t.Fatal(err)
			}
		}
		first, second := tenants[0], tenants[1]

		providers := func(inboxID string) (string, string) {
			record, err := st.GetInboxRecordByID(ctx, inboxID)
			if err != nil {
				t.Fatal(err)
			}
			return record.InboundProvider, record.OutboundProvider
		}

		// Both helpers refuse, and say so rather than reporting a no-op.
		for name, update := range map[string]func(orgID, inboxID string) error{
			"UpdateInboxProviders": func(orgID, inboxID string) error {
				return st.UpdateInboxProviders(ctx, orgID, inboxID, "hybrid", "hybrid")
			},
			"UpdateInboxTransportProviders": func(orgID, inboxID string) error {
				return st.UpdateInboxTransportProviders(ctx, orgID, inboxID, "hybrid")
			},
		} {
			t.Run(name+" across tenants", func(t *testing.T) {
				if err := update(first.org, second.inbox); err == nil {
					t.Fatal("one organization rerouted another's mailbox")
				}
				inbound, outbound := providers(second.inbox)
				if inbound != "jmap" || outbound != "smtp" {
					t.Fatalf("the other tenant's mailbox changed to %s/%s", inbound, outbound)
				}
			})
			t.Run(name+" unknown mailbox", func(t *testing.T) {
				if err := update(first.org, uuid.NewString()); err == nil {
					t.Fatal("an update against no row reported success")
				}
			})
		}

		// The owning tenant still works, and each helper writes what it says.
		if err := st.UpdateInboxProviders(ctx, first.org, first.inbox, "hybrid", "resend"); err != nil {
			t.Fatal(err)
		}
		if inbound, outbound := providers(first.inbox); inbound != "hybrid" || outbound != "resend" {
			t.Fatalf("independent update wrote %s/%s", inbound, outbound)
		}
		if err := st.UpdateInboxTransportProviders(ctx, first.org, first.inbox, "hybrid"); err != nil {
			t.Fatal(err)
		}
		if inbound, outbound := providers(first.inbox); inbound != "hybrid" || outbound != "hybrid" {
			t.Fatalf("transport update wrote %s/%s", inbound, outbound)
		}
		// And the other tenant is untouched throughout.
		if inbound, outbound := providers(second.inbox); inbound != "jmap" || outbound != "smtp" {
			t.Fatalf("the other tenant's mailbox drifted to %s/%s", inbound, outbound)
		}
	})
}
