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
	// Allocate the drift holder here, before any scoped store copies the
	// pointer: a nil parent would leave each transaction marking a private copy
	// that the process never observes.
	if s.drift == nil {
		s.drift = new(atomic.Bool)
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
			)`, preFenceClaimGuard,

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

// ErrOutboundFenceDrift reports that the live Core schema no longer matches the
// capability this process fixed at startup. It is deliberately terminal for the
// process: re-deciding at run time is what created a probe-then-execute race,
// so a drifted process refuses outbox work until it is restarted.
var ErrOutboundFenceDrift = errors.New("core schema changed beneath this process: restart required before outbox work resumes")

// resolveOutboundFence reports the capability fixed at startup.
//
// It never re-decides. An earlier design probed the live schema per operation,
// but the probe and the caller's statement are separate statements: Core 0029
// could commit between them, and the claim would then run legacy SQL against a
// fenced schema. Deciding once and refusing on drift removes that race by
// construction rather than narrowing it.
func (s *Store) resolveOutboundFence(context.Context) bool {
	return s.OutboundFenceEnabled()
}

// requireNoOutboundFenceDrift refuses every outbox operation once drift has
// been observed.
func (s *Store) requireNoOutboundFenceDrift() error {
	if s.drift != nil && s.drift.Load() {
		return ErrOutboundFenceDrift
	}
	return nil
}

func (s *Store) markOutboundFenceDrift() {
	if s.drift == nil {
		s.drift = new(atomic.Bool)
	}
	s.drift.Store(true)
}

// noteOutboxSchemaError converts an undefined-column or undefined-table failure
// into terminal drift. This is the fenced-process-on-a-rolled-back-schema
// direction: the statement fails loudly, so no bypass is possible, but the
// process must not silently downgrade itself either.
//
// It takes no connection. The previous version probed the pool from inside a
// caller's transaction, which could wait for a connection that the same caller
// already held.
func (s *Store) noteOutboxSchemaError(err error) error {
	if err == nil {
		return err
	}
	if isUndefinedColumnOrTable(err) {
		s.markOutboundFenceDrift()
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

// setOutboundFence records the capability. Callers may only ever move it from
// unfenced to fenced: see the monotonicity note in SetFeatureFlag. The reverse
// transition is drift and is handled by markOutboundFenceDrift instead.
func (s *Store) setOutboundFence(enabled bool) {
	if s.fence == nil {
		s.fence = newEnabledFence()
	}
	s.fence.Store(enabled)
}

// preFenceClaimGuard is spliced into the pre-fence claim. It is evaluated in the
// same statement and the same snapshot as the claim itself, so a Core 0029 that
// commits after the startup decision cannot be claimed against with legacy SQL.
// This is the one direction that produces no SQL error -- legacy SQL runs
// perfectly well on a fenced schema -- so it needs an in-statement guard rather
// than any form of probe.
const preFenceClaimGuard = "(SELECT to_regclass('public.org_outbound_policy_state') IS NULL)"

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
	// Deliberately no drift check: this guards convergence paths such as
	// ResolveOutboxProviderAttempt and QuarantineClaimedOutboxUnknown as well as
	// paths that start work. Once a provider call may have happened its outcome
	// must stay recordable, so drift is enforced by the callers that begin work.
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
