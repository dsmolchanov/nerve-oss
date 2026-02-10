package billing

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"neuralmail/internal/config"
	"neuralmail/internal/store"
)

func TestStripeWebhookReplayIsIdempotent(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		insertPlan(t, ctx, st, "pro", 120, 1000, 10)
		insertOrg(t, ctx, st, orgID)

		cfg := config.Default()
		cfg.Billing.StripeWebhookSecret = "whsec_test"
		cfg.Metering.PastDueGraceDays = 7
		svc := NewStripeService(cfg, st)
		svc.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

		payload := []byte(fmt.Sprintf(`{
			"id":"evt_sub_update",
			"type":"customer.subscription.updated",
			"data":{"object":{
				"id":"sub_123",
				"customer":"cus_123",
				"status":"active",
				"current_period_start":1700000000,
				"current_period_end":1702592000,
				"cancel_at_period_end":false,
				"metadata":{"org_id":"%s"},
				"items":{"data":[{"price":{"lookup_key":"pro","id":"price_pro"}}]}
			}}
		}`, orgID))
		header := stripeSignatureHeader(cfg.Billing.StripeWebhookSecret, svc.Now().Unix(), payload)

		if err := svc.ProcessWebhook(ctx, payload, header); err != nil {
			t.Fatalf("process first webhook: %v", err)
		}
		if err := svc.ProcessWebhook(ctx, payload, header); err != nil {
			t.Fatalf("process replay webhook: %v", err)
		}

		var count int
		if err := st.DB().QueryRowContext(ctx, `SELECT count(*) FROM webhook_events WHERE provider = 'stripe' AND external_event_id = 'evt_sub_update'`).Scan(&count); err != nil {
			t.Fatalf("count webhook rows: %v", err)
		}
		if count != 1 {
			t.Fatalf("expected exactly one webhook row, got %d", count)
		}
	})
}

func TestStripeEventStatusMapping(t *testing.T) {
	tests := []struct {
		name          string
		eventType     string
		objectPayload string
		expected      string
		prepare       func(t *testing.T, ctx context.Context, st *store.Store, orgID string)
	}{
		{
			name:      "subscription active",
			eventType: "customer.subscription.updated",
			objectPayload: `{
				"id":"sub_1",
				"customer":"cus_1",
				"status":"active",
				"current_period_start":1700000000,
				"current_period_end":1702592000,
				"metadata":{"org_id":"%s"},
				"items":{"data":[{"price":{"lookup_key":"pro","id":"price_pro"}}]}
			}`,
			expected: "active",
		},
		{
			name:      "subscription past_due",
			eventType: "customer.subscription.updated",
			objectPayload: `{
				"id":"sub_1",
				"customer":"cus_1",
				"status":"past_due",
				"current_period_start":1700000000,
				"current_period_end":1702592000,
				"metadata":{"org_id":"%s"},
				"items":{"data":[{"price":{"lookup_key":"pro","id":"price_pro"}}]}
			}`,
			expected: "past_due",
		},
		{
			name:      "subscription deleted",
			eventType: "customer.subscription.deleted",
			objectPayload: `{
				"id":"sub_1",
				"customer":"cus_1",
				"status":"active",
				"current_period_start":1700000000,
				"current_period_end":1702592000,
				"metadata":{"org_id":"%s"},
				"items":{"data":[{"price":{"lookup_key":"pro","id":"price_pro"}}]}
			}`,
			expected: "canceled",
		},
		{
			name:      "invoice paid",
			eventType: "invoice.paid",
			objectPayload: `{
				"id":"in_1",
				"customer":"cus_1",
				"subscription":"sub_1"
			}`,
			expected: "active",
			prepare:  prepareInvoiceMapping,
		},
		{
			name:      "invoice payment failed",
			eventType: "invoice.payment_failed",
			objectPayload: `{
				"id":"in_2",
				"customer":"cus_1",
				"subscription":"sub_1"
			}`,
			expected: "past_due",
			prepare:  prepareInvoiceMapping,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			withTempStore(t, func(ctx context.Context, st *store.Store) {
				orgID := uuid.NewString()
				insertPlan(t, ctx, st, "pro", 120, 1000, 10)
				insertOrg(t, ctx, st, orgID)
				if tc.prepare != nil {
					tc.prepare(t, ctx, st, orgID)
				}

				cfg := config.Default()
				cfg.Billing.StripeWebhookSecret = "whsec_test"
				cfg.Metering.PastDueGraceDays = 7
				svc := NewStripeService(cfg, st)
				svc.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

				object := tc.objectPayload
				if strings.Contains(object, "%s") {
					object = fmt.Sprintf(object, orgID)
				}
				payload := []byte(fmt.Sprintf(`{
					"id":"evt_%s",
					"type":"%s",
					"data":{"object":%s}
				}`, strings.ReplaceAll(tc.name, " ", "_"), tc.eventType, object))
				header := stripeSignatureHeader(cfg.Billing.StripeWebhookSecret, svc.Now().Unix(), payload)
				if err := svc.ProcessWebhook(ctx, payload, header); err != nil {
					t.Fatalf("process webhook: %v", err)
				}

				var status string
				if err := st.DB().QueryRowContext(ctx, `SELECT subscription_status FROM org_entitlements WHERE org_id = $1`, orgID).Scan(&status); err != nil {
					t.Fatalf("read entitlement status: %v", err)
				}
				if status != tc.expected {
					t.Fatalf("expected status %s, got %s", tc.expected, status)
				}
			})
		})
	}
}

func TestFailedWebhookStoredAndCanBeReprocessed(t *testing.T) {
	withTempStore(t, func(ctx context.Context, st *store.Store) {
		orgID := uuid.NewString()
		insertOrg(t, ctx, st, orgID)

		cfg := config.Default()
		cfg.Billing.StripeWebhookSecret = "whsec_test"
		cfg.Metering.PastDueGraceDays = 7
		svc := NewStripeService(cfg, st)
		svc.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }

		payload := []byte(fmt.Sprintf(`{
			"id":"evt_retry",
			"type":"customer.subscription.updated",
			"data":{"object":{
				"id":"sub_retry",
				"customer":"cus_retry",
				"status":"active",
				"current_period_start":1700000000,
				"current_period_end":1702592000,
				"metadata":{"org_id":"%s"},
				"items":{"data":[{"price":{"lookup_key":"missing_plan","id":"price_missing"}}]}
			}}
		}`, orgID))
		header := stripeSignatureHeader(cfg.Billing.StripeWebhookSecret, svc.Now().Unix(), payload)

		if err := svc.ProcessWebhook(ctx, payload, header); err == nil {
			t.Fatalf("expected first processing to fail due to missing plan")
		}
		assertWebhookStatus(t, ctx, st, "evt_retry", "failed")

		insertPlan(t, ctx, st, "missing_plan", 120, 1000, 10)
		if err := svc.ProcessWebhook(ctx, payload, header); err != nil {
			t.Fatalf("expected reprocessing to succeed: %v", err)
		}
		assertWebhookStatus(t, ctx, st, "evt_retry", "processed")
	})
}

func prepareInvoiceMapping(t *testing.T, ctx context.Context, st *store.Store, orgID string) {
	t.Helper()
	if err := st.UpsertSubscription(ctx, store.SubscriptionRecord{
		OrgID:                  orgID,
		Provider:               stripeProvider,
		ExternalCustomerID:     "cus_1",
		ExternalSubscriptionID: "sub_1",
		Status:                 "active",
		CurrentPeriodStart:     sql.NullTime{Time: time.Unix(1_700_000_000, 0).UTC(), Valid: true},
		CurrentPeriodEnd:       sql.NullTime{Time: time.Unix(1_702_592_000, 0).UTC(), Valid: true},
	}); err != nil {
		t.Fatalf("insert subscription mapping: %v", err)
	}
	if err := st.UpsertOrgEntitlement(ctx, store.OrgEntitlement{
		OrgID:              orgID,
		PlanCode:           "pro",
		SubscriptionStatus: "active",
		MCPRPM:             120,
		MonthlyUnits:       1000,
		MaxInboxes:         10,
		UsagePeriodStart:   time.Unix(1_700_000_000, 0).UTC(),
		UsagePeriodEnd:     time.Unix(1_702_592_000, 0).UTC(),
	}); err != nil {
		t.Fatalf("insert org entitlement mapping: %v", err)
	}
}

func assertWebhookStatus(t *testing.T, ctx context.Context, st *store.Store, eventID, expected string) {
	t.Helper()
	var status string
	if err := st.DB().QueryRowContext(ctx, `SELECT status FROM webhook_events WHERE provider = 'stripe' AND external_event_id = $1`, eventID).Scan(&status); err != nil {
		t.Fatalf("read webhook status: %v", err)
	}
	if status != expected {
		t.Fatalf("expected webhook status %s, got %s", expected, status)
	}
}

func insertOrg(t *testing.T, ctx context.Context, st *store.Store, orgID string) {
	t.Helper()
	if _, err := st.DB().ExecContext(ctx, `INSERT INTO orgs (id, name) VALUES ($1, $2)`, orgID, "billing-test"); err != nil {
		t.Fatalf("insert org: %v", err)
	}
}

func insertPlan(t *testing.T, ctx context.Context, st *store.Store, code string, rpm int, monthlyUnits int64, maxInboxes int) {
	t.Helper()
	if _, err := st.DB().ExecContext(ctx, `
		INSERT INTO plan_entitlements (plan_code, mcp_rpm, monthly_units, max_inboxes)
		VALUES ($1, $2, $3, $4)
	`, code, rpm, monthlyUnits, maxInboxes); err != nil {
		t.Fatalf("insert plan entitlement: %v", err)
	}
}

func stripeSignatureHeader(secret string, timestamp int64, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", timestamp, string(payload))))
	signature := hex.EncodeToString(mac.Sum(nil))
	return fmt.Sprintf("t=%d,v1=%s", timestamp, signature)
}

func withTempStore(t *testing.T, run func(ctx context.Context, st *store.Store)) {
	t.Helper()

	baseDSN := os.Getenv("NM_TEST_DB_DSN")
	if baseDSN == "" {
		baseDSN = "postgres://neuralmail:neuralmail@127.0.0.1:54320/neuralmail?sslmode=disable"
	}
	adminDSN, err := dsnWithDatabase(baseDSN, "postgres")
	if err != nil {
		t.Fatalf("build admin dsn: %v", err)
	}
	adminDB, err := sql.Open("pgx", adminDSN)
	if err != nil {
		t.Fatalf("open admin db: %v", err)
	}
	defer adminDB.Close()

	pingCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := adminDB.PingContext(pingCtx); err != nil {
		t.Skipf("postgres unavailable for billing tests: %v", err)
	}

	dbName := "nerve_bill_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminDB.ExecContext(context.Background(), fmt.Sprintf(`CREATE DATABASE %s`, dbName)); err != nil {
		t.Fatalf("create test db: %v", err)
	}

	testDSN, err := dsnWithDatabase(baseDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	st, err := store.Open(testDSN)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	goose.SetDialect("postgres")
	goose.SetTableName("schema_migrations")
	if err := goose.UpContext(context.Background(), st.DB(), migrationDir(t)); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Cleanup(func() {
		_, _ = adminDB.ExecContext(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, dbName)
		_, _ = adminDB.ExecContext(context.Background(), fmt.Sprintf(`DROP DATABASE IF EXISTS %s`, dbName))
	})

	run(context.Background(), st)
}

func dsnWithDatabase(rawDSN, dbName string) (string, error) {
	parsed, err := url.Parse(rawDSN)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func migrationDir(t *testing.T) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("resolve migration dir: missing caller")
	}
	return filepath.Join(filepath.Dir(currentFile), "..", "store", "migrations")
}
