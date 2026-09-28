package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

type tenantScopeInjectedFailure struct {
	*sql.Tx
	failQuery string
	failAt    int
	seen      *int
}

func (q tenantScopeInjectedFailure) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if strings.Contains(query, q.failQuery) {
		if q.seen != nil {
			*q.seen++
		}
		if q.failAt == 0 || q.seen != nil && *q.seen == q.failAt {
			return nil, errors.New("injected tenant scope failure")
		}
	}
	return q.Tx.ExecContext(ctx, query, args...)
}

func TestWithTxBareStoreCommitsAndRollsBack(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if _, err := db.ExecContext(ctx, `CREATE TABLE tx_probe (value text PRIMARY KEY)`); err != nil {
			t.Fatalf("create probe table: %v", err)
		}
		st := &Store{db: db, q: db}

		if err := st.withTx(ctx, func(scoped *Store) error {
			if !scoped.inTx {
				t.Fatal("transaction-scoped store was not marked inTx")
			}
			_, err := scoped.q.ExecContext(ctx, `INSERT INTO tx_probe (value) VALUES ('committed')`)
			return err
		}); err != nil {
			t.Fatalf("commit transaction: %v", err)
		}

		sentinel := errors.New("rollback")
		err := st.withTx(ctx, func(scoped *Store) error {
			if _, err := scoped.q.ExecContext(ctx, `INSERT INTO tx_probe (value) VALUES ('rolled-back')`); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected rollback sentinel, got %v", err)
		}

		var values []string
		rows, err := db.QueryContext(ctx, `SELECT value FROM tx_probe ORDER BY value`)
		if err != nil {
			t.Fatalf("query probe rows: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				t.Fatalf("scan probe row: %v", err)
			}
			values = append(values, value)
		}
		if len(values) != 1 || values[0] != "committed" {
			t.Fatalf("unexpected committed rows: %v", values)
		}
	})
}

func TestRunAsOrgInsideTransactionKeepsTenantScopeAndCallerRollback(t *testing.T) {
	withTempDatabase(t, func(parent context.Context, db *sql.DB) {
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		defer cancel()
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(ctx, `CREATE TABLE tenant_scope_probe(value text PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		st := &Store{db: db, q: db}
		orgID := "00000000-0000-0000-0000-000000000001"
		rollback := errors.New("outer rollback")
		err := st.RunInTx(ctx, func(tx *Store) error {
			if err := tx.RunAsOrg(ctx, orgID, func(scoped *Store) error {
				if scoped != tx {
					t.Fatal("tenant scope left the webhook transaction")
				}
				var mode, scopedOrg string
				if err := scoped.q.QueryRowContext(ctx, `SELECT current_setting('app.cloud_mode', true),
					current_setting('app.current_org_id', true)`).Scan(&mode, &scopedOrg); err != nil {
					return err
				}
				if mode != "true" || scopedOrg != orgID {
					t.Fatalf("tenant scope mode=%q org=%q", mode, scopedOrg)
				}
				_, err := scoped.q.ExecContext(ctx, `INSERT INTO tenant_scope_probe(value) VALUES('scoped')`)
				return err
			}); err != nil {
				return err
			}
			var restoredOrg string
			if err := tx.q.QueryRowContext(ctx, `SELECT coalesce(current_setting('app.current_org_id', true),'')`).Scan(&restoredOrg); err != nil {
				return err
			}
			if restoredOrg != "" {
				t.Fatalf("tenant scope leaked after callback: %q", restoredOrg)
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatalf("outer rollback=%v", err)
		}
		var count int
		if err := db.QueryRowContext(parent, `SELECT count(*) FROM tenant_scope_probe`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("tenant-scoped write escaped rollback: count=%d err=%v", count, err)
		}
	})
}

func TestRunAsOrgInsideTransactionRestoresScopeAfterHandledError(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		db.SetMaxOpenConns(1)
		st := &Store{db: db, q: db}
		orgA := "00000000-0000-0000-0000-000000000001"
		orgB := "00000000-0000-0000-0000-000000000002"
		sentinel := errors.New("handled business error")
		err := st.RunInTx(ctx, func(tx *Store) error {
			if _, err := tx.q.ExecContext(ctx, `SELECT set_config('app.cloud_mode','false',true)`); err != nil {
				return err
			}
			if _, err := tx.q.ExecContext(ctx, `SELECT set_config('app.current_org_id',$1,true)`, orgA); err != nil {
				return err
			}
			if err := tx.RunAsOrg(ctx, orgB, func(scoped *Store) error {
				var actualOrg string
				if err := scoped.q.QueryRowContext(ctx, `SELECT current_setting('app.current_org_id',true)`).Scan(&actualOrg); err != nil {
					return err
				}
				if actualOrg != orgB {
					t.Fatalf("nested org=%q, want %q", actualOrg, orgB)
				}
				return sentinel
			}); !errors.Is(err, sentinel) {
				return err
			}
			var mode, actualOrg string
			if err := tx.q.QueryRowContext(ctx, `SELECT current_setting('app.cloud_mode',true),
				current_setting('app.current_org_id',true)`).Scan(&mode, &actualOrg); err != nil {
				return err
			}
			if mode != "false" || actualOrg != orgA {
				t.Fatalf("handled error leaked tenant scope: mode=%q org=%q", mode, actualOrg)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("outer transaction could not commit after handled error: %v", err)
		}
	})
}

func TestRunAsOrgRestorationFailureCannotBeHandledAsBusinessError(t *testing.T) {
	withTempDatabase(t, func(parent context.Context, db *sql.DB) {
		db.SetMaxOpenConns(1)
		if _, err := db.ExecContext(parent, `CREATE TABLE tenant_restore_probe(value text PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		st := &Store{db: db, q: db}
		sentinel := errors.New("handled business error")
		err := st.RunInTx(parent, func(tx *Store) error {
			if _, err := tx.q.ExecContext(parent, `INSERT INTO tenant_restore_probe(value) VALUES('must roll back')`); err != nil {
				return err
			}
			child, cancel := context.WithCancel(parent)
			defer cancel()
			innerErr := tx.RunAsOrg(child, "00000000-0000-0000-0000-000000000002", func(*Store) error {
				cancel()
				return sentinel
			})
			if errors.Is(innerErr, sentinel) {
				return nil // The caller would incorrectly commit on a handled error.
			}
			return innerErr
		})
		if !errors.Is(err, ErrTenantScopeRestoreFailed) || errors.Is(err, sentinel) {
			t.Fatalf("restoration failure was handleable as business error: %v", err)
		}
		var count int
		if err := db.QueryRowContext(parent, `SELECT count(*) FROM tenant_restore_probe`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("outer transaction committed with leaked scope: count=%d err=%v", count, err)
		}
	})
}

func TestRunAsOrgSetupFailureCannotCommitPreviousTenantScope(t *testing.T) {
	for _, failQuery := range []string{"set_config('app.cloud_mode'", "set_config('app.current_org_id'"} {
		t.Run(failQuery, func(t *testing.T) {
			withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
				if _, err := db.ExecContext(ctx, `CREATE TABLE tenant_setup_probe(value text PRIMARY KEY)`); err != nil {
					t.Fatal(err)
				}
				st := &Store{db: db, q: db}
				err := st.RunInTx(ctx, func(tx *Store) error {
					if _, err := tx.q.ExecContext(ctx, `INSERT INTO tenant_setup_probe(value) VALUES('must roll back')`); err != nil {
						return err
					}
					underlying := tx.q.(*sql.Tx)
					if _, err := underlying.ExecContext(ctx, `SELECT set_config('app.cloud_mode','false',true)`); err != nil {
						return err
					}
					if _, err := underlying.ExecContext(ctx, `SELECT set_config('app.current_org_id','00000000-0000-0000-0000-000000000001',true)`); err != nil {
						return err
					}
					tx.q = tenantScopeInjectedFailure{Tx: underlying, failQuery: failQuery}
					innerErr := tx.RunAsOrg(ctx, "00000000-0000-0000-0000-000000000002", func(*Store) error {
						t.Fatal("callback ran after scope setup failed")
						return nil
					})
					if !errors.Is(innerErr, ErrTenantScopeRestoreFailed) {
						t.Fatalf("setup error was handleable: %v", innerErr)
					}
					return nil // Simulate a caller that handles and ignores the error.
				})
				if !errors.Is(err, sql.ErrTxDone) {
					t.Fatalf("outer transaction committed after setup failure: %v", err)
				}
				var count int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tenant_setup_probe`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("stale tenant transaction committed: count=%d err=%v", count, err)
				}
			})
		})
	}
}

func TestRunAsOrgRestorationFailureAbortsEvenWhenCallerIgnoresError(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if _, err := db.ExecContext(ctx, `CREATE TABLE tenant_restore_ignored(value text PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		st := &Store{db: db, q: db}
		err := st.RunInTx(ctx, func(tx *Store) error {
			if _, err := tx.q.ExecContext(ctx, `INSERT INTO tenant_restore_ignored(value) VALUES('must roll back')`); err != nil {
				return err
			}
			seen := 0
			tx.q = tenantScopeInjectedFailure{Tx: tx.q.(*sql.Tx), failQuery: "set_config('app.current_org_id', $1, true)", failAt: 2, seen: &seen}
			_ = tx.RunAsOrg(ctx, "00000000-0000-0000-0000-000000000002", func(*Store) error { return nil })
			if seen != 2 {
				t.Fatalf("injection did not reach restoration: %d", seen)
			}
			return nil
		})
		if !errors.Is(err, sql.ErrTxDone) {
			t.Fatalf("caller ignored scope error and committed: %v", err)
		}
		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tenant_restore_ignored`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("scope failure committed: count=%d err=%v", count, err)
		}
	})
}

func TestWithTxInsideRunAsOrgUsesCallerTransaction(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		if _, err := db.ExecContext(ctx, `CREATE TABLE tx_probe (value text PRIMARY KEY)`); err != nil {
			t.Fatalf("create probe table: %v", err)
		}
		st := &Store{db: db, q: db}
		sentinel := errors.New("outer rollback")

		err := st.RunAsOrg(ctx, "00000000-0000-0000-0000-000000000001", func(scoped *Store) error {
			if !scoped.inTx {
				t.Fatal("RunAsOrg store was not marked inTx")
			}
			return scoped.withTx(ctx, func(inner *Store) error {
				if inner != scoped {
					t.Fatal("nested withTx opened a different transaction-scoped store")
				}
				if _, err := inner.q.ExecContext(ctx, `INSERT INTO tx_probe (value) VALUES ('nested')`); err != nil {
					return err
				}
				return sentinel
			})
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected outer rollback sentinel, got %v", err)
		}

		var count int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM tx_probe`).Scan(&count); err != nil {
			t.Fatalf("count probe rows: %v", err)
		}
		if count != 0 {
			t.Fatalf("nested transaction escaped caller rollback: %d rows", count)
		}
	})
}
