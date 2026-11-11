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
	"github.com/jackc/pgx/v5/pgconn"
)

// Call only for roles created by this fixture, before its database is closed.
// DROP OWNED can change a policy's pg_class tuple before acquiring the table
// lock. VACUUM acquires those locks in the opposite order. Taking application
// relation locks first prevents that inversion; retries hide the first failure
// and leave cluster-wide roles behind. Keep every caller's original role guard,
// reassignment, SQL and error propagation.
func fixtureRoleCleanup(ctx context.Context, db *sql.DB, statement string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fixtureLockRelations(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, statement); err != nil {
		return err
	}
	return tx.Commit()
}

func fixtureLockRelations(ctx context.Context, tx *sql.Tx) error {
	// The database belongs to this fixture. Include non-public probe schemas,
	// and lock ONLY each relation so inheritance cannot reorder the OID order.
	rows, err := tx.QueryContext(ctx, `SELECT format('LOCK TABLE ONLY %I.%I IN ACCESS EXCLUSIVE MODE',n.nspname,c.relname)
FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
WHERE c.relkind IN ('r','p') AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
ORDER BY c.oid`)
	if err != nil {
		return err
	}
	var statements []string
	for rows.Next() {
		var statement string
		if err := rows.Scan(&statement); err != nil {
			rows.Close()
			return err
		}
		statements = append(statements, statement)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// Exercise the actual wrapper used by all callers. Hold its catalog tuple so
// we can observe the granted relation lock BEFORE catalog mutation proceeds.
func TestFixtureRoleCleanupAcquiresRelationBeforeCatalogMutation(t *testing.T) {
	withTempDatabase(t, func(parent context.Context, db *sql.DB) {
		ctx, cancel := context.WithTimeout(parent, 10*time.Second)
		defer cancel()
		role := "nerve_hybrid_service_cleanup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := db.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN; CREATE SCHEMA "cleanup-quoted"; CREATE TABLE "cleanup-quoted"."quote""table"(id bigint); ALTER TABLE "cleanup-quoted"."quote""table" ENABLE ROW LEVEL SECURITY; CREATE POLICY cleanup_scope ON "cleanup-quoted"."quote""table" TO `+role+` USING(true); GRANT SELECT ON "cleanup-quoted"."quote""table" TO `+role); err != nil {
			t.Fatal(err)
		}
		defer func() {
			var exists bool
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&exists); err != nil {
				t.Error(err)
			} else if exists {
				if err := fixtureRoleCleanup(ctx, db, `DROP OWNED BY `+role+`; DROP ROLE `+role); err != nil {
					t.Error(err)
				}
			}
		}()
		barrier, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer barrier.Rollback()
		var oid int64
		var blocker int
		if err := barrier.QueryRowContext(ctx, `SELECT oid, pg_backend_pid() FROM pg_class WHERE oid='"cleanup-quoted"."quote""table"'::regclass FOR UPDATE`).Scan(&oid, &blocker); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- fixtureRoleCleanup(ctx, db, `DROP OWNED BY `+role+`; DROP ROLE `+role) }()
		consumed := false
		defer func() {
			_ = barrier.Rollback()
			if !consumed {
				if err := <-done; err != nil {
					t.Error(err)
				}
			}
		}()
		observed := false
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation=$1 AND mode='AccessExclusiveLock' AND granted AND $2=ANY(pg_blocking_pids(pid)))`, oid, blocker).Scan(&observed); err != nil {
				t.Fatal(err)
			}
			if observed {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !observed {
			t.Fatal("actual cleanup wrapper did not acquire relation before catalog mutation")
		}
		if err := barrier.Rollback(); err != nil {
			t.Fatal(err)
		}
		err = <-done
		consumed = true
		if err != nil {
			t.Fatal(err)
		}
		var exists bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&exists); err != nil || exists {
			t.Fatalf("cleanup did not commit role removal: exists=%t err=%v", exists, err)
		}
	})
}

func TestFixtureRoleCleanupRollsBackFailedStatement(t *testing.T) {
	withTempDatabase(t, func(ctx context.Context, db *sql.DB) {
		role := "nerve_hybrid_service_cleanup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		if _, err := db.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN; CREATE TABLE cleanup_probe(id bigint); ALTER TABLE cleanup_probe ENABLE ROW LEVEL SECURITY; CREATE POLICY cleanup_scope ON cleanup_probe TO `+role+` USING(true)`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := fixtureRoleCleanup(ctx, db, `DROP OWNED BY `+role+`; DROP ROLE `+role); err != nil {
				t.Error(err)
			}
		}()
		err := fixtureRoleCleanup(ctx, db, `DROP OWNED BY `+role+`; DROP ROLE `+role+`; SELECT 1/0`)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "22012" {
			t.Fatalf("original cleanup error was not propagated: %v", err)
		}
		var retained bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles r JOIN pg_policy p ON r.oid=ANY(p.polroles) WHERE r.rolname=$1 AND p.polname='cleanup_scope')`, role).Scan(&retained); err != nil || !retained {
			t.Fatalf("failed cleanup was not atomic: policy and role retained=%t err=%v", retained, err)
		}
	})
}

// Use the real policy/catalog tuple as a barrier, and actual VACUUM ANALYZE,
// rather than depending on when the autovacuum scheduler happens to run. The
// old order must produce 40P01; relation-first cleanup and VACUUM must both
// complete. Either participant may be chosen as the old-order deadlock victim.
func TestFixtureRoleCleanupOrdersPolicyLocksBeforeVacuum(t *testing.T) {
	for _, command := range []string{"SELECT", "UPDATE", "PUBLIC"} {
		for _, ordered := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/relations_first_%t", command, ordered), func(t *testing.T) {
				withTempDatabase(t, func(parent context.Context, db *sql.DB) {
					ctx, cancel := context.WithTimeout(parent, 10*time.Second)
					defer cancel()
					role := "nerve_hybrid_service_cleanup_" + strings.ReplaceAll(uuid.NewString(), "-", "")
					policyCommand, grantee := command, role
					if command == "PUBLIC" {
						policyCommand, grantee = "SELECT", "PUBLIC"
					}
					if _, err := db.ExecContext(ctx, `CREATE ROLE `+role+` NOLOGIN; CREATE TABLE public.cleanup_probe(id bigint); INSERT INTO public.cleanup_probe VALUES(1); ALTER TABLE public.cleanup_probe ENABLE ROW LEVEL SECURITY; CREATE POLICY cleanup_scope ON public.cleanup_probe FOR `+policyCommand+` TO `+grantee+` USING(true); GRANT SELECT,UPDATE ON public.cleanup_probe TO `+role); err != nil {
						t.Fatal(err)
					}
					defer func() {
						var exists bool
						if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)`, role).Scan(&exists); err != nil {
							t.Error(err)
						} else if exists {
							if err := fixtureRoleCleanup(ctx, db, `DROP OWNED BY `+role+`; DROP ROLE `+role); err != nil {
								t.Error(err)
							}
						}
					}()
					tx, err := db.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					defer tx.Rollback()
					if _, err := tx.ExecContext(ctx, `SET LOCAL deadlock_timeout='100ms'; SET LOCAL statement_timeout='6s'`); err != nil {
						t.Fatal(err)
					}
					if ordered {
						if err := fixtureLockRelations(ctx, tx); err != nil {
							t.Fatal(err)
						}
					}
					var oid int64
					if err := tx.QueryRowContext(ctx, `SELECT oid FROM pg_class WHERE oid='public.cleanup_probe'::regclass FOR UPDATE`).Scan(&oid); err != nil {
						t.Fatal(err)
					}
					vacuum, err := db.Conn(ctx)
					if err != nil {
						t.Fatal(err)
					}
					defer vacuum.Close()
					var pid int
					if err := vacuum.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
						t.Fatal(err)
					}
					if _, err := vacuum.ExecContext(ctx, `SET statement_timeout='6s'; SET deadlock_timeout='100ms'`); err != nil {
						t.Fatal(err)
					}
					done := make(chan error, 1)
					go func() {
						_, err := vacuum.ExecContext(ctx, `VACUUM ANALYZE public.cleanup_probe`)
						done <- err
					}()
					consumed := false
					defer func() {
						if !consumed {
							_ = tx.Rollback()
							cancel()
							<-done
						}
					}()
					deadline := time.Now().Add(3 * time.Second)
					observed := false
					for time.Now().Before(deadline) {
						var waiting bool
						query := `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted AND locktype='transactionid')`
						args := []any{pid}
						if ordered {
							query = `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted AND locktype='relation' AND relation=$2)`
							args = append(args, oid)
						}
						if err := db.QueryRowContext(ctx, query, args...).Scan(&waiting); err != nil {
							t.Fatal(err)
						}
						if waiting {
							observed = true
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					if !observed {
						t.Fatal("actual VACUUM lock wait was not observed")
					}
					_, dropErr := tx.ExecContext(ctx, `DROP OWNED BY `+role+`; DROP ROLE `+role)
					var commitErr error
					if dropErr == nil {
						commitErr = tx.Commit()
					} else {
						_ = tx.Rollback()
					}
					vacuumErr := <-done
					consumed = true
					if ordered {
						if dropErr != nil || commitErr != nil || vacuumErr != nil {
							t.Fatalf("ordered cleanup=%v commit=%v vacuum=%v", dropErr, commitErr, vacuumErr)
						}
					} else {
						isDeadlock := func(err error) bool {
							var pgErr *pgconn.PgError
							return errors.As(err, &pgErr) && pgErr.Code == "40P01"
						}
						if !isDeadlock(dropErr) && !isDeadlock(vacuumErr) {
							t.Fatalf("old order did not reproduce 40P01: cleanup=%v vacuum=%v", dropErr, vacuumErr)
						}
					}
				})
			})
		}
	}
}
