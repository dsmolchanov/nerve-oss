package store

import (
	"context"
	"database/sql"
	"strings"
	"sync/atomic"
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
// It reads the *applied* version through the same latest-record semantics as
// CurrentVersionCore. A plain max(version_id) would be wrong: Goose retains a
// version 29 row after a clean down-migration, so a rolled-back schema would
// still look fenced and every statement would then fail against objects that
// migration 0029 had just dropped.
func outboundFenceAvailable(ctx context.Context, db *sql.DB) (bool, error) {
	version, err := CurrentVersionCore(ctx, db)
	if err != nil {
		return true, err
	}
	return version >= CoreOutboundFenceVersion, nil
}

// RefreshOutboundFenceCapability records whether the live Core schema carries
// the Core 0029 fence. Call it once after startup migrations settle and before
// the outbox worker claims anything.
func (s *Store) RefreshOutboundFenceCapability(ctx context.Context) error {
	if s.fence == nil {
		s.fence = newEnabledFence()
	}
	available, err := outboundFenceAvailable(ctx, s.db)
	if err != nil {
		s.fence.Store(true)
		return err
	}
	s.fence.Store(available)
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

// outboxSQL returns the statement appropriate to the live Core schema.
func (s *Store) outboxSQL(fenced string) string {
	if s.OutboundFenceEnabled() {
		return fenced
	}
	return preFenceOutboxSQL.Replace(fenced)
}

// requireOutboundFence guards the autonomous-only provider fence operations.
// They are unreachable before Core 0029 because no autonomous generation can
// exist, so a call on Core 28 is a programming error, not a legacy path.
func (s *Store) requireOutboundFence(operation string) error {
	if s.OutboundFenceEnabled() {
		return nil
	}
	return &UnsupportedSchemaError{Operation: operation, RequiresCore: CoreOutboundFenceVersion}
}

// outboxArgs drops the trailing fence parameter when the pre-fence statement
// omits it. The fence value is always last so no other position shifts.
func (s *Store) outboxArgs(args ...any) []any {
	if s.OutboundFenceEnabled() {
		return args
	}
	return args[:len(args)-1]
}

// newEnabledFence returns the fail-closed default capability holder.
func newEnabledFence() *atomic.Bool {
	fence := new(atomic.Bool)
	fence.Store(true)
	return fence
}
