package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

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
