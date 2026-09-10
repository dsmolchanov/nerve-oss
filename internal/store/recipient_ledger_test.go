package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func recipientFixture(t *testing.T, ctx context.Context, db *sql.DB, limit sql.NullInt64) (*Store, RecipientPeriod) {
	t.Helper()
	if err := MigrateUpToCore(ctx, db, 30); err != nil {
		t.Fatal(err)
	}
	p := RecipientPeriod{OrgID: uuid.NewString(), PeriodID: uuid.NewString(), StartsAt: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond), EndsAt: time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond), Limit: limit}
	if _, err := db.ExecContext(ctx, `INSERT INTO orgs(id,name) VALUES($1,'recipient fixture')`, p.OrgID); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db, q: db}
	if err := s.RunInTx(ctx, func(tx *Store) error { return tx.InstallRecipientPeriod(ctx, p) }); err != nil {
		t.Fatal(err)
	}
	return s, p
}
func recipientReserve(ctx context.Context, s *Store, p RecipientPeriod, outbox string, count int64) (state string, err error) {
	err = s.RunInTx(ctx, func(tx *Store) error {
		var e error
		state, e = tx.ReserveRecipients(ctx, p.OrgID, p.PeriodID, outbox, count)
		return e
	})
	return
}
func recipientResolve(ctx context.Context, s *Store, p RecipientPeriod, outbox, state string) error {
	return s.RunInTx(ctx, func(tx *Store) error { return tx.ResolveRecipients(ctx, p.OrgID, p.PeriodID, outbox, state) })
}
func assertRecipientCounters(t *testing.T, ctx context.Context, db *sql.DB, p RecipientPeriod, wantReserved, wantCommitted int64) {
	t.Helper()
	var reserved, committed int64
	if err := db.QueryRowContext(ctx, `SELECT reserved,committed FROM org_recipient_periods WHERE org_id=$1 AND period_id=$2`, p.OrgID, p.PeriodID).Scan(&reserved, &committed); err != nil {
		t.Fatal(err)
	}
	if reserved != wantReserved || committed != wantCommitted {
		t.Fatalf("reserved/committed=%d/%d want %d/%d", reserved, committed, wantReserved, wantCommitted)
	}
	var ledgerReserved, ledgerCommitted int64
	if err := db.QueryRowContext(ctx, `SELECT coalesce(sum(recipient_count) FILTER(WHERE state='reserved'),0),coalesce(sum(recipient_count) FILTER(WHERE state='committed'),0) FROM recipient_reservations WHERE org_id=$1 AND period_id=$2`, p.OrgID, p.PeriodID).Scan(&ledgerReserved, &ledgerCommitted); err != nil {
		t.Fatal(err)
	}
	if ledgerReserved != reserved || ledgerCommitted != committed {
		t.Fatalf("ledger drift: %d/%d vs counters %d/%d", ledgerReserved, ledgerCommitted, reserved, committed)
	}
}
func TestRecipientLedgerConcurrency(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, p := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 10, Valid: true})
		db.SetMaxOpenConns(20)
		var accepted atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make(chan error, 100)
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := recipientReserve(ctx, s, p, uuid.NewString(), 1)
				if err == nil {
					accepted.Add(1)
				} else if !errors.Is(err, ErrRecipientLimit) {
					errs <- err
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if accepted.Load() != 10 {
			t.Fatalf("accepted=%d want 10", accepted.Load())
		}
		assertRecipientCounters(t, ctx, db, p, 10, 0)
	})
}
func TestRecipientLedgerReplayTerminalAndRollback(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, p := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 10, Valid: true})
		outbox := uuid.NewString()
		for i := 0; i < 2; i++ {
			if state, err := recipientReserve(ctx, s, p, outbox, 2); err != nil || state != "reserved" {
				t.Fatalf("%s %v", state, err)
			}
		}
		if _, err := recipientReserve(ctx, s, p, outbox, 3); !errors.Is(err, ErrRecipientLedgerConflict) {
			t.Fatalf("count replay %v", err)
		}
		other := p
		other.PeriodID = uuid.NewString()
		if err := s.RunInTx(ctx, func(tx *Store) error { return tx.InstallRecipientPeriod(ctx, other) }); err != nil {
			t.Fatal(err)
		}
		if _, err := recipientReserve(ctx, s, other, outbox, 2); !errors.Is(err, ErrRecipientLedgerConflict) {
			t.Fatalf("period replay %v", err)
		}
		assertRecipientCounters(t, ctx, db, other, 0, 0)
		sentinel := errors.New("outer rollback")
		err := s.RunAsOrg(ctx, p.OrgID, func(tx *Store) error {
			if _, err := tx.ReserveRecipients(ctx, p.OrgID, p.PeriodID, uuid.NewString(), 3); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, p, 2, 0)
		for _, state := range []string{"committed", "released"} {
			id := outbox
			if state == "released" {
				id = uuid.NewString()
				if _, err := recipientReserve(ctx, s, p, id, 3); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if err := recipientResolve(ctx, s, p, id, state); err != nil {
					t.Fatal(err)
				}
			}
			opposite := "released"
			if state == "released" {
				opposite = "committed"
			}
			if err := recipientResolve(ctx, s, p, id, opposite); !errors.Is(err, ErrRecipientLedgerConflict) {
				t.Fatalf("terminal conflict %v", err)
			}
			if got, err := recipientReserve(ctx, s, p, id, map[string]int64{"committed": 2, "released": 3}[state]); err != nil || got != state {
				t.Fatalf("terminal replay %s %v", got, err)
			}
		}
		assertRecipientCounters(t, ctx, db, p, 0, 2)
	})
}
func TestRecipientLedgerUnknownPastPeriodAndClose(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, p := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 10, Valid: true})
		id := uuid.NewString()
		if _, err := recipientReserve(ctx, s, p, id, 4); err != nil {
			t.Fatal(err)
		}
		// Test-only temporal transition; production never rewrites paid boundaries.
		if _, err := db.ExecContext(ctx, `UPDATE org_recipient_periods SET ends_at=clock_timestamp()-interval '1 second',admission_closed=true WHERE org_id=$1 AND period_id=$2`, p.OrgID, p.PeriodID); err != nil {
			t.Fatal(err)
		}
		if _, err := recipientReserve(ctx, s, p, uuid.NewString(), 1); !errors.Is(err, ErrRecipientLimit) {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, p, 4, 0)
		if state, err := recipientReserve(ctx, s, p, id, 4); err != nil || state != "reserved" {
			t.Fatalf("unknown replay %s %v", state, err)
		}
		if err := recipientResolve(ctx, s, p, id, "unknown"); err == nil {
			t.Fatal("accepted unknown resolution")
		}
		if err := recipientResolve(ctx, s, p, id, "committed"); err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, p, 0, 4)
	})
}
func TestRecipientLedgerConcurrentIdentityAndResolution(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, p := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 10, Valid: true})
		other := p
		other.PeriodID = uuid.NewString()
		if err := s.RunInTx(ctx, func(tx *Store) error { return tx.InstallRecipientPeriod(ctx, other) }); err != nil {
			t.Fatal(err)
		}
		id := uuid.NewString()
		var wg sync.WaitGroup
		var accepted atomic.Int64
		errs := make(chan error, 20)
		for i := 0; i < 20; i++ {
			candidate := p
			if i%2 == 1 {
				candidate = other
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := recipientReserve(ctx, s, candidate, id, 1)
				if err == nil {
					accepted.Add(1)
				} else if !errors.Is(err, ErrRecipientLedgerConflict) {
					errs <- err
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if accepted.Load() != 10 {
			t.Fatalf("identity successes=%d", accepted.Load())
		}
		var winningPeriod string
		if err := db.QueryRowContext(ctx, `SELECT period_id FROM recipient_reservations WHERE org_id=$1 AND outbox_id=$2`, p.OrgID, id).Scan(&winningPeriod); err != nil {
			t.Fatal(err)
		}
		winner := p
		loser := other
		if winningPeriod == other.PeriodID {
			winner, loser = other, p
		}
		outcomes := make(chan error, 2)
		for _, state := range []string{"released", "committed"} {
			wg.Add(1)
			go func() { defer wg.Done(); outcomes <- recipientResolve(ctx, s, winner, id, state) }()
		}
		wg.Wait()
		close(outcomes)
		successes, conflicts := 0, 0
		for err := range outcomes {
			if err == nil {
				successes++
			} else if errors.Is(err, ErrRecipientLedgerConflict) {
				conflicts++
			} else {
				t.Fatal(err)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("terminal race %d successes/%d conflicts", successes, conflicts)
		}
		assertRecipientCounters(t, ctx, db, loser, 0, 0)
		var committed int64
		if err := db.QueryRowContext(ctx, `SELECT committed FROM org_recipient_periods WHERE org_id=$1 AND period_id=$2`, winner.OrgID, winner.PeriodID).Scan(&committed); err != nil {
			t.Fatal(err)
		}
		assertRecipientCounters(t, ctx, db, winner, 0, committed)
	})
}
func TestRecipientLedgerLimitAndPeriodContract(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		for _, limit := range []sql.NullInt64{{Int64: 0, Valid: true}, {}} {
			s, p := recipientFixture(t, ctx, db, limit)
			_, err := recipientReserve(ctx, s, p, uuid.NewString(), 1)
			if limit.Valid {
				if !errors.Is(err, ErrRecipientLimit) {
					t.Fatal(err)
				}
				assertRecipientCounters(t, ctx, db, p, 0, 0)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertRecipientCounters(t, ctx, db, p, 1, 0)
			}
			if err := s.RunInTx(ctx, func(tx *Store) error { return tx.InstallRecipientPeriod(ctx, p) }); err != nil {
				t.Fatal(err)
			}
			for _, change := range []func(*RecipientPeriod){func(q *RecipientPeriod) { q.Limit = sql.NullInt64{Int64: 20, Valid: true} }, func(q *RecipientPeriod) { q.EndsAt = q.EndsAt.Add(time.Hour) }} {
				altered := p
				change(&altered)
				if err := s.RunInTx(ctx, func(tx *Store) error { return tx.InstallRecipientPeriod(ctx, altered) }); !errors.Is(err, ErrRecipientLedgerConflict) {
					t.Fatalf("period mutation: %v", err)
				}
			}
			for _, change := range []func(*RecipientPeriod){func(q *RecipientPeriod) { q.EndsAt = q.StartsAt }, func(q *RecipientPeriod) { q.Limit = sql.NullInt64{Int64: -1, Valid: true} }} {
				q := p
				q.PeriodID = uuid.NewString()
				change(&q)
				if err := s.RunInTx(ctx, func(tx *Store) error { return tx.InstallRecipientPeriod(ctx, q) }); err == nil {
					t.Fatal("invalid period accepted")
				}
			}
			if err := s.InstallRecipientPeriod(ctx, p); err == nil {
				t.Fatal("autocommit install")
			}
			if _, err := s.ReserveRecipients(ctx, p.OrgID, p.PeriodID, uuid.NewString(), 1); err == nil {
				t.Fatal("autocommit reserve")
			}
			if err := s.ResolveRecipients(ctx, p.OrgID, p.PeriodID, uuid.NewString(), "committed"); err == nil {
				t.Fatal("autocommit resolve")
			}
		}
	})
}
func TestRecipientLedgerMigrationAndConstraints(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if err := MigrateUpToCore(ctx, db, 29); err != nil {
			t.Fatal(err)
		}
		legacy := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO orgs(id,name) VALUES($1,'legacy recipient upgrade')`, legacy); err != nil {
			t.Fatal(err)
		}

		_, legacyInbox, _ := seedAttachmentMessageParents(t, ctx, db, "ledger-upgrade")
		legacyOutbox := uuid.NewString()
		if _, err := db.ExecContext(ctx, `INSERT INTO outbox_messages(id,org_id,inbox_id,provider,idempotency_key,"to","from",subject,text_body)
            SELECT $1,org_id,id,'smtp','ledger-upgrade','to@example.com','from@example.com','legacy','unchanged body' FROM inboxes WHERE id=$2`, legacyOutbox, legacyInbox); err != nil {
			t.Fatal(err)
		}
		if err := MigrateUpToCore(ctx, db, 30); err != nil {
			t.Fatal(err)
		}
		var body string
		if err := db.QueryRowContext(ctx, `SELECT text_body FROM outbox_messages WHERE id=$1`, legacyOutbox).Scan(&body); err != nil || body != "unchanged body" {
			t.Fatalf("legacy outbox changed: %s %v", body, err)
		}
		var enrolled int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM recipient_reservations`).Scan(&enrolled); err != nil || enrolled != 0 {
			t.Fatalf("legacy outbox unexpectedly enrolled: %d %v", enrolled, err)
		}
		var name string
		if err := db.QueryRowContext(ctx, `SELECT name FROM orgs WHERE id=$1`, legacy).Scan(&name); err != nil || name != "legacy recipient upgrade" {
			t.Fatalf("legacy preservation %s %v", name, err)
		}
		if err := MigrateDownCore(ctx, db); err != nil {
			t.Fatal(err)
		}
		if err := MigrateUpToCore(ctx, db, 30); err != nil {
			t.Fatal(err)
		}
		s, p := recipientFixture(t, ctx, db, sql.NullInt64{Int64: 10, Valid: true})
		if err := MigrateDownCore(ctx, db); err == nil || !strings.Contains(err.Error(), "recipient ledger rows exist") {
			t.Fatalf("down guard %v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM orgs WHERE id=$1`, p.OrgID); err == nil {
			t.Fatal("org delete erased ledger")
		}
		for _, fragment := range []string{"starts_at='-infinity'", "ends_at='infinity'", "ends_at=starts_at", "recipient_limit=-1", "reserved=11", "committed=11", "meter_version=1"} {
			if _, err := db.ExecContext(ctx, `UPDATE org_recipient_periods SET `+fragment+` WHERE org_id=$1 AND period_id=$2`, p.OrgID, p.PeriodID); err == nil {
				t.Fatalf("accepted invalid %s", fragment)
			}
		}
		if _, err := recipientReserve(ctx, s, p, uuid.NewString(), 2); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE org_recipient_periods SET recipient_limit=1 WHERE org_id=$1 AND period_id=$2`, p.OrgID, p.PeriodID); err == nil {
			t.Fatal("lowered allowance below reserved")
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO recipient_reservations(org_id,outbox_id,period_id,recipient_count) VALUES($1,$2,$3,1)`, legacy, uuid.NewString(), p.PeriodID); err == nil {
			t.Fatal("cross-tenant period accepted")
		}
		assertRecipientCounters(t, ctx, db, p, 2, 0)
	})
}
func TestRecipientLedgerRLS(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		s, a := recipientFixture(t, ctx, db, sql.NullInt64{})
		_, b := recipientFixture(t, ctx, db, sql.NullInt64{})
		for _, p := range []RecipientPeriod{a, b} {
			if _, err := recipientReserve(ctx, s, p, uuid.NewString(), 1); err != nil {
				t.Fatal(err)
			}
		}
		role := "recipient_rls_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := db.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = db.ExecContext(context.Background(), `DROP OWNED BY `+role)
			_, _ = db.ExecContext(context.Background(), `DROP ROLE `+role)
		})
		if _, err := db.ExecContext(ctx, `GRANT SELECT,INSERT,UPDATE,DELETE ON org_recipient_periods,recipient_reservations TO `+role); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"org_recipient_periods", "recipient_reservations"} {
			if err := s.RunAsOrg(ctx, a.OrgID, func(tx *Store) error {
				if _, err := tx.q.ExecContext(ctx, `SET LOCAL ROLE `+role); err != nil {
					return err
				}
				var n int
				if err := tx.q.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
					return err
				}
				if n != 1 {
					return fmt.Errorf("visible %s=%d", table, n)
				}
				result, err := tx.q.ExecContext(ctx, `DELETE FROM `+table+` WHERE org_id=$1`, b.OrgID)
				if err != nil {
					return err
				}
				n64, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if n64 != 0 {
					return errors.New("cross-tenant mutation")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		err := s.RunAsOrg(ctx, a.OrgID, func(tx *Store) error {
			if _, err := tx.q.ExecContext(ctx, `SET LOCAL ROLE `+role); err != nil {
				return err
			}
			return tx.InstallRecipientPeriod(ctx, RecipientPeriod{OrgID: b.OrgID, PeriodID: uuid.NewString(), StartsAt: b.StartsAt, EndsAt: b.EndsAt})
		})
		if err == nil {
			t.Fatal("cross-tenant insert")
		}
	})
}
