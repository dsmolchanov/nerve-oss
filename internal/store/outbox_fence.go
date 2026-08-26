package store

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgconn"
)

// CoreOutboundFenceVersion is the Core migration that adds the outbound policy
// epoch and provider-start fence columns to outbox_messages.
const CoreOutboundFenceVersion int64 = 29

// The four Core 0029 columns. Every adapted statement is asserted against this
// list so an edit that reintroduces one on the pre-fence path fails a test
// rather than a production query.
var coreOutboundFenceColumns = []string{
	"autonomous_policy_epoch",
	"provider_started_at",
	"provider_operation_id",
	"provider_resolved_at",
}

// outboundFenceAvailable reports whether the live Core schema has the fence.
//
// Artifact B spans Core [28,29]: it runs legacy behavior on Core 28 before the
// additive Core 0029 migration and full autonomous behavior afterwards. The
// pre-fence statement variants below let the legacy outbox path work on Core 28
// without emitting an unsupported-schema query.
//
// The default is fenced. An unfenced statement on Core 29 would silently skip
// the autonomous policy epoch and provider-start checks, so an undetermined
// schema must fail loudly on Core 28 rather than quietly under-enforce on
// Core 29.
// It distinguishes three states, because only one of them may disable the
// fence:
//
//   - Absent or empty migration history is *undetermined*. It must not be read
//     as "pre-29": with NM_MIGRATE_ON_START=off, or after history loss on an
//     otherwise Core 29 database, selecting pre-fence SQL would drop
//     autonomous_policy_epoch on enqueue, make the claim predicate vacuously
//     true, and send mail with no epoch or revocation check. That is a silent
//     policy bypass, so this returns an error and leaves the fence enabled.
//   - A proven applied version below 29 is the legacy predecessor: unfenced.
//   - A proven applied version at or above 29 is fenced.
//
// It reads through the caller's queryer rather than the pool. A caller inside a
// transaction already holds a connection, so probing on a second one lets the
// pool deadlock: with MaxOpenConns(10), ten concurrent pre-fence enqueues each
// hold one and then all wait for an eleventh.
//
// The applied-version scan mirrors currentVersion: latest record per version
// wins, and only an applied one counts. A plain max(version_id) would be wrong
// in the other direction, since Goose can retain a version 29 row after a
// rollback and the schema would still look fenced.
func outboundFenceAvailable(ctx context.Context, q queryer) (bool, error) {
	var present bool
	if err := q.QueryRowContext(ctx,
		`SELECT to_regclass('public.schema_migrations_core') IS NOT NULL`).Scan(&present); err != nil {
		return true, err
	}
	if !present {
		return true, errors.New("core migration history is absent: outbound fence capability is undetermined")
	}
	rows, err := q.QueryContext(ctx,
		`SELECT version_id, is_applied FROM schema_migrations_core ORDER BY id DESC`)
	if err != nil {
		return true, err
	}
	defer rows.Close()
	seen := make(map[int64]struct{})
	for rows.Next() {
		var version int64
		var applied bool
		if err := rows.Scan(&version, &applied); err != nil {
			return true, err
		}
		if _, ok := seen[version]; ok {
			continue
		}
		seen[version] = struct{}{}
		if applied {
			return version >= CoreOutboundFenceVersion, nil
		}
	}
	if err := rows.Err(); err != nil {
		return true, err
	}
	return true, errors.New("core migration history has no applied version: outbound fence capability is undetermined")
}

// RefreshOutboundFenceCapability records whether the live Core schema carries
// the Core 0029 fence. Call it once after startup migrations settle and before
// the outbox worker claims anything.
func (s *Store) RefreshOutboundFenceCapability(ctx context.Context) error {
	if s.fence == nil {
		s.fence = newEnabledFence()
	}
	available, err := outboundFenceAvailable(ctx, s.q)
	if err != nil {
		s.setOutboundFence(true)
		return err
	}
	s.setOutboundFence(available)
	return nil
}

// OutboundFenceEnabled reports the recorded capability. It is true unless the
// live Core schema was proven to predate 0029. A Store built without a
// capability holder reports fenced, keeping the fail-closed default.
func (s *Store) OutboundFenceEnabled() bool {
	if s.fence == nil {
		return true
	}
	return s.fence.Load()
}

// preFenceOutboxSQL rewrites a fenced statement into its Core 28 equivalent.
//
// Replacements are exact and ordered. They are deliberately literal rather than
// clever: TestPreFenceOutboxStatementsDropEveryFenceColumn proves that every
// adapted statement loses all four columns, so an edit to the fenced SQL that
// escapes a replacement fails in CI instead of at run time.
var preFenceOutboxSQL = strings.NewReplacer(
	// Enqueue: the fence column is last in both lists, so the pre-fence form
	// drops it together with its trailing parameter.
	",\n\t\t\t\tautonomous_policy_epoch", "",
	",\n\t\t\t\tnullif($18, 0)", "",

	// Claim: no row can carry an epoch or an unresolved provider start before
	// Core 0029, so the whole policy predicate is vacuously satisfied.
	`(
				outbox.autonomous_policy_epoch IS NULL
				OR (outbox.provider_started_at IS NOT NULL AND outbox.provider_resolved_at IS NULL)
				OR (
					EXISTS (
						SELECT 1
						FROM org_outbound_policy_state policy_state
						WHERE policy_state.org_id = outbox.org_id
						  AND policy_state.policy_epoch = outbox.autonomous_policy_epoch
					)
					AND EXISTS (
						SELECT 1 FROM org_feature_flags enabled
						WHERE enabled.org_id = outbox.org_id
						  AND enabled.flag = 'autonomous_outbound_policy'
						  AND enabled.enabled
					)
					AND EXISTS (
						SELECT 1 FROM org_feature_flags suspended
						WHERE suspended.org_id = outbox.org_id
						  AND suspended.flag = 'email_outbound_suspended'
						  AND NOT suspended.enabled
					)
				)
			)`, "true",

	// Projections keep their column count and types so every Scan target and
	// every caller sees the same shape on both schemas.
	"coalesce(o.autonomous_policy_epoch, 0), o.provider_started_at, o.provider_operation_id, o.provider_resolved_at",
	"0::bigint, NULL::timestamptz, NULL::text, NULL::timestamptz",

	// Terminal updates: there is no start evidence to resolve before the fence.
	",\n\t\t    provider_resolved_at = CASE WHEN provider_started_at IS NOT NULL THEN now() ELSE provider_resolved_at END", "",
	",\n\t\t    provider_resolved_at = CASE WHEN $7 AND provider_started_at IS NOT NULL THEN now() ELSE provider_resolved_at END", "",

	// A claim taken before the fence carries no operation id, so the CAS
	// reduces to the caller asserting it holds no operation identity.
	"(($3 = '' AND provider_operation_id IS NULL) OR provider_operation_id = $3)", "$3 = ''",

	// Requeue reads keep their shape; nothing can be fenced before Core 0029.
	"SELECT org_id::text, autonomous_policy_epoch", "SELECT org_id::text, NULL::bigint",
	"SELECT autonomous_policy_epoch, provider_started_at, provider_resolved_at",
	"SELECT NULL::bigint, NULL::timestamptz, NULL::timestamptz",
)

// resolveOutboundFence reports the capability to use for one operation.
//
// The two directions are deliberately asymmetric, because their failure modes
// are not comparable:
//
//   - Believing we are pre-fence when the schema has moved to Core 29 is a
//     policy bypass. The legacy claim predicate is vacuously true, the epoch
//     projects as zero, and provider-start takes the legacy fast path, so an
//     autonomous row can be sent with no epoch or revocation check. Phase 9
//     creates exactly this window: Artifact B is deployed on Core 28 and Core
//     0029 is applied while it is still running. So the pre-fence path
//     revalidates against the live schema before every operation, and the cost
//     falls entirely inside that short-lived predecessor window.
//   - Believing we are fenced when the schema has rolled back to Core 28 is an
//     outage, not a bypass: the statement fails loudly with undefined_column.
//     That direction stays optimistic and self-heals through
//     noteOutboxSchemaError, so the steady Core 29 state pays nothing.
func (s *Store) resolveOutboundFence(ctx context.Context) bool {
	if s.OutboundFenceEnabled() {
		return true
	}
	if s.q == nil {
		// Nothing to probe and nothing executable either; keep what is known.
		return false
	}
	available, err := outboundFenceAvailable(ctx, s.q)
	if err != nil {
		// Undetermined: fail closed rather than keep using legacy SQL.
		s.setOutboundFence(true)
		return true
	}
	s.setOutboundFence(available)
	return available
}

// noteOutboxSchemaError re-detects the capability when a statement failed
// because the schema does not carry the objects it referenced. The caller still
// sees the error; the next attempt selects correctly.
func (s *Store) noteOutboxSchemaError(ctx context.Context, err error) error {
	if err == nil || !isUndefinedColumnOrTable(err) {
		return err
	}
	// The caller's transaction, if any, has already failed; probe the pool so a
	// poisoned connection cannot swallow the re-detection.
	if available, probeErr := outboundFenceAvailable(ctx, s.db); probeErr == nil {
		s.setOutboundFence(available)
	} else {
		s.setOutboundFence(true)
	}
	return err
}

func isUndefinedColumnOrTable(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "42703" || pgErr.Code == "42P01"
}

func (s *Store) setOutboundFence(enabled bool) {
	if s.fence == nil {
		s.fence = newEnabledFence()
	}
	s.fence.Store(enabled)
}

// adaptOutboxSQL and trimOutboxArgs take an already-resolved capability so a
// single operation cannot pick the statement under one answer and its arguments
// under another, which would strand a parameter.
func adaptOutboxSQL(fenced bool, statement string) string {
	if fenced {
		return statement
	}
	return preFenceOutboxSQL.Replace(statement)
}

func trimOutboxArgs(fenced bool, args ...any) []any {
	if fenced {
		return args
	}
	return args[:len(args)-1]
}

// requireOutboundFence guards the autonomous-only provider fence operations.
// They are unreachable before Core 0029 because no autonomous generation can
// exist, so a call on Core 28 is a programming error, not a legacy path.
// requireOutboundFence guards the autonomous-only operations. It resolves
// against the live schema rather than the cached flag: after Core 0029 is
// applied beneath a process that started on Core 28, a cached answer would
// refuse these forever until some unrelated adaptable operation happened to
// refresh the shared capability, blocking onboarding and autonomous delivery
// against a perfectly valid schema.
func (s *Store) requireOutboundFence(ctx context.Context, operation string) error {
	if s.resolveOutboundFence(ctx) {
		return nil
	}
	return &UnsupportedSchemaError{Operation: operation, RequiresCore: CoreOutboundFenceVersion}
}

// newEnabledFence returns the fail-closed default capability holder.
func newEnabledFence() *atomic.Bool {
	fence := new(atomic.Bool)
	fence.Store(true)
	return fence
}
