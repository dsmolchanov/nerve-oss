package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"neuralmail/internal/domains"
	"neuralmail/internal/emailaddr"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type InboxRecord struct {
	ID          string
	OrgID       string
	OrgDomainID sql.NullString
	Address     string
	Status      string
	CreatedAt   time.Time
	ExternalRef sql.NullString

	InboundProvider           string
	OutboundProvider          string
	InboundProviderConfigRef  sql.NullString
	OutboundProviderConfigRef sql.NullString

	ForwardTo sql.NullString
}

// ErrCanonicalInboxAddressAmbiguous means legacy rows collapse to the same
// canonical address. Picking one would make routing and replay depend on row
// order, so every canonical-address boundary fails closed instead.
var ErrCanonicalInboxAddressAmbiguous = fmt.Errorf(
	"%w: multiple inboxes have the same canonical address", ErrResourceConflict,
)

// canonicalInboxAddressSQLTemplate is the single database-side identity
// expression for legacy inbox bytes. The explicit trim set is Go's Unicode
// White_Space set used by strings.TrimSpace; regexp_replace removes exactly
// one final domain dot, matching domains.CanonicalizeDomain. Callers must
// substitute only a static SQL column expression.
const canonicalInboxAddressSQLTemplate = `lower(regexp_replace(btrim({address}, U&'\0009\000A\000B\000C\000D\0020\0085\00A0\1680\2000\2001\2002\2003\2004\2005\2006\2007\2008\2009\200A\2028\2029\202F\205F\3000'), '\.$', ''))`

func canonicalInboxAddressSQL(column string) string {
	return strings.Replace(canonicalInboxAddressSQLTemplate, "{address}", column, 1)
}

func (s *Store) GetInboxRecordByID(ctx context.Context, inboxID string) (InboxRecord, error) {
	var rec InboxRecord
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes
		WHERE id = $1
	`, inboxID)
	if err := row.Scan(
		&rec.ID,
		&rec.OrgID,
		&rec.OrgDomainID,
		&rec.Address,
		&rec.Status,
		&rec.CreatedAt,
		&rec.InboundProvider,
		&rec.OutboundProvider,
		&rec.InboundProviderConfigRef,
		&rec.OutboundProviderConfigRef,
		&rec.ForwardTo,
		&rec.ExternalRef,
	); err != nil {
		return rec, err
	}
	return rec, nil
}

func (s *Store) GetInboxRecordByIDForOrg(ctx context.Context, orgID string, inboxID string) (InboxRecord, error) {
	var rec InboxRecord
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes
		WHERE id = $1 AND org_id = $2
	`, inboxID, orgID)
	if err := row.Scan(
		&rec.ID,
		&rec.OrgID,
		&rec.OrgDomainID,
		&rec.Address,
		&rec.Status,
		&rec.CreatedAt,
		&rec.InboundProvider,
		&rec.OutboundProvider,
		&rec.InboundProviderConfigRef,
		&rec.OutboundProviderConfigRef,
		&rec.ForwardTo,
		&rec.ExternalRef,
	); err != nil {
		return rec, err
	}
	return rec, nil
}

func (s *Store) ListInboxRecordsByOrg(ctx context.Context, orgID string) ([]InboxRecord, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes
		WHERE org_id = $1
		ORDER BY created_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []InboxRecord
	for rows.Next() {
		var rec InboxRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.OrgID,
			&rec.OrgDomainID,
			&rec.Address,
			&rec.Status,
			&rec.CreatedAt,
			&rec.InboundProvider,
			&rec.OutboundProvider,
			&rec.InboundProviderConfigRef,
			&rec.OutboundProviderConfigRef,
			&rec.ForwardTo,
			&rec.ExternalRef,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) GetInboxByAddress(ctx context.Context, address string) (InboxRecord, error) {
	canonicalAddress, _, err := canonicalInboxAddress(address)
	if err != nil {
		return InboxRecord{}, err
	}
	return s.getInboxByCanonicalAddress(ctx, canonicalAddress, true)
}

func (s *Store) getInboxByCanonicalAddress(ctx context.Context, canonicalAddress string, activeOnly bool) (InboxRecord, error) {
	statusPredicate := ""
	if activeOnly {
		statusPredicate = " AND status = 'active'"
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes
		WHERE `+canonicalInboxAddressSQL("address")+` = $1`+statusPredicate+`
		ORDER BY created_at, id
		LIMIT 2
	`, canonicalAddress)
	if err != nil {
		return InboxRecord{}, err
	}
	defer rows.Close()

	var records []InboxRecord
	for rows.Next() {
		var rec InboxRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.OrgID,
			&rec.OrgDomainID,
			&rec.Address,
			&rec.Status,
			&rec.CreatedAt,
			&rec.InboundProvider,
			&rec.OutboundProvider,
			&rec.InboundProviderConfigRef,
			&rec.OutboundProviderConfigRef,
			&rec.ForwardTo,
			&rec.ExternalRef,
		); err != nil {
			return InboxRecord{}, err
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		return InboxRecord{}, err
	}
	if len(records) == 0 {
		return InboxRecord{}, sql.ErrNoRows
	}
	if len(records) > 1 {
		return InboxRecord{}, ErrCanonicalInboxAddressAmbiguous
	}
	return records[0], nil
}

// globalActiveInboxByCanonicalAddress reports only whether an active canonical
// identity exists. When called inside RunAsOrg it temporarily disables tenant
// RLS for this read, then restores the exact transaction-local mode before the
// caller can mutate anything. Returning no row data keeps the privileged scope
// limited to the global uniqueness decision.
func (s *Store) withGlobalRLSRead(ctx context.Context, fn func() error) (resultErr error) {
	if err := s.requireTx(); err != nil {
		return err
	}
	if fn == nil {
		return errors.New("global RLS read callback is required")
	}
	var cloudMode, currentOrgID string
	if err := s.q.QueryRowContext(ctx, `
		SELECT coalesce(current_setting('app.cloud_mode', true), ''),
		       coalesce(current_setting('app.current_org_id', true), '')
	`).Scan(&cloudMode, &currentOrgID); err != nil {
		return err
	}
	bypassed := strings.EqualFold(cloudMode, "true")
	if bypassed {
		if currentOrgID == "" {
			return errors.New("missing current org for global inbox identity read")
		}
		if _, err := s.q.ExecContext(ctx, `SELECT set_config('app.cloud_mode', 'false', true)`); err != nil {
			return err
		}
		defer func() {
			_, restoreErr := s.q.ExecContext(ctx, `SELECT set_config('app.cloud_mode', $1, true)`, cloudMode)
			resultErr = errors.Join(resultErr, restoreErr)
		}()
	}
	return fn()
}

func (s *Store) globalActiveInboxByCanonicalAddress(ctx context.Context, canonicalAddress string) (exists bool, resultErr error) {
	resultErr = s.withGlobalRLSRead(ctx, func() error {
		return s.q.QueryRowContext(ctx, `
			SELECT EXISTS (
			  SELECT 1
			  FROM inboxes
			  WHERE `+canonicalInboxAddressSQL("address")+` = $1
			    AND status = 'active'
			)
		`, canonicalAddress).Scan(&exists)
	})
	return exists, resultErr
}

func (s *Store) canonicalOrgDomainByID(ctx context.Context, orgDomainID string) (canonical string, resultErr error) {
	orgDomainID = strings.TrimSpace(orgDomainID)
	if orgDomainID == "" {
		return "", nil
	}
	var stored string
	if err := s.withGlobalRLSRead(ctx, func() error {
		return s.q.QueryRowContext(ctx, `SELECT domain FROM org_domains WHERE id = $1::uuid`, orgDomainID).Scan(&stored)
	}); err != nil {
		return "", err
	}
	canonical, resultErr = domains.CanonicalizeDomain(stored)
	return canonical, resultErr
}

func (s *Store) lockCanonicalNamespaces(ctx context.Context, namespaces ...string) error {
	keys := append([]string(nil), namespaces...)
	sort.Strings(keys)
	previous := ""
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || key == previous {
			continue
		}
		if err := s.lockCanonicalDomain(ctx, key); err != nil {
			return err
		}
		previous = key
	}
	return nil
}

func (s *Store) lockInboxCanonicalNamespaces(ctx context.Context, addressDomain, orgDomainID string) (string, error) {
	linkedDomain, err := s.canonicalOrgDomainByID(ctx, orgDomainID)
	if err != nil {
		return "", err
	}
	if err := s.lockCanonicalNamespaces(ctx, addressDomain, linkedDomain); err != nil {
		return "", err
	}
	if orgDomainID != "" {
		lockedLinkedDomain, err := s.canonicalOrgDomainByID(ctx, orgDomainID)
		if err != nil {
			return "", err
		}
		if lockedLinkedDomain != linkedDomain {
			return "", ErrResourceConflict
		}
	}
	return linkedDomain, nil
}

type inboxLifecycleIdentity struct {
	CanonicalAddress string
	AddressDomain    string
	OrgDomainID      sql.NullString
	LinkedDomain     string
	Status           string
}

func (s *Store) readInboxLifecycleIdentity(ctx context.Context, orgID, inboxID string, forUpdate bool) (inboxLifecycleIdentity, error) {
	var identity inboxLifecycleIdentity
	var address string
	query := `
		SELECT address, status, org_domain_id::text
		FROM inboxes
		WHERE id = $1 AND org_id = $2`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	if err := s.q.QueryRowContext(ctx, query, inboxID, orgID).Scan(
		&address, &identity.Status, &identity.OrgDomainID,
	); err != nil {
		return inboxLifecycleIdentity{}, err
	}
	canonicalAddress, addressDomain, err := canonicalInboxAddress(address)
	if err != nil {
		return inboxLifecycleIdentity{}, err
	}
	identity.CanonicalAddress = canonicalAddress
	identity.AddressDomain = addressDomain
	if identity.OrgDomainID.Valid {
		identity.LinkedDomain, err = s.canonicalOrgDomainByID(ctx, identity.OrgDomainID.String)
		if err != nil {
			return inboxLifecycleIdentity{}, err
		}
	}
	return identity, nil
}

func (identity inboxLifecycleIdentity) sameCanonicalLockIdentity(other inboxLifecycleIdentity) bool {
	return identity.CanonicalAddress == other.CanonicalAddress &&
		identity.AddressDomain == other.AddressDomain &&
		identity.OrgDomainID.Valid == other.OrgDomainID.Valid &&
		(!identity.OrgDomainID.Valid || identity.OrgDomainID.String == other.OrgDomainID.String) &&
		identity.LinkedDomain == other.LinkedDomain
}

func (s *Store) lockInboxLifecycleIdentity(ctx context.Context, identity inboxLifecycleIdentity) error {
	return s.lockCanonicalNamespaces(ctx, identity.AddressDomain, identity.LinkedDomain)
}

// getPreferredInboxByCanonicalAddress preserves the legacy EnsureInbox
// contract: an active inbox wins over disabled history. Only when there is no
// active equivalent do callers replay a single disabled equivalent. Either
// active or disabled ambiguity fails closed.
func (s *Store) getPreferredInboxByCanonicalAddress(ctx context.Context, canonicalAddress string) (InboxRecord, error) {
	rec, err := s.getInboxByCanonicalAddress(ctx, canonicalAddress, true)
	if !errors.Is(err, sql.ErrNoRows) {
		return rec, err
	}
	return s.getInboxByCanonicalAddress(ctx, canonicalAddress, false)
}

func (s *Store) CreateInboxForOrg(ctx context.Context, orgID string, address string, orgDomainID string, outboundProviders ...string) (InboxRecord, error) {
	canonicalAddress, canonicalDomain, err := canonicalInboxAddress(address)
	if err != nil {
		return InboxRecord{}, err
	}
	outboundProvider := "smtp"
	if len(outboundProviders) > 0 && strings.TrimSpace(outboundProviders[0]) != "" {
		outboundProvider = outboundProviders[0]
	}
	var rec InboxRecord
	err = s.withTx(ctx, func(scoped *Store) error {
		if _, err := scoped.lockInboxCanonicalNamespaces(ctx, canonicalDomain, orgDomainID); err != nil {
			return err
		}
		if _, err := scoped.getInboxByCanonicalAddress(ctx, canonicalAddress, true); err == nil {
			return ErrResourceConflict
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		conflict, err := scoped.globalActiveInboxByCanonicalAddress(ctx, canonicalAddress)
		if err != nil {
			return err
		}
		if conflict {
			return ErrResourceConflict
		}
		var createErr error
		rec, createErr = scoped.createInboxForOrg(
			ctx, orgID, canonicalAddress, orgDomainID, outboundProvider, "",
		)
		return createErr
	})
	return rec, err
}

func (s *Store) createInboxForOrg(ctx context.Context, orgID string, address string, orgDomainID string, outboundProvider string, externalRef string) (InboxRecord, error) {
	return s.createInboxForOrgWithID(ctx, uuid.NewString(), orgID, address, orgDomainID, outboundProvider, externalRef)
}

func (s *Store) createInboxForOrgWithID(ctx context.Context, inboxID string, orgID string, address string, orgDomainID string, outboundProvider string, externalRef string) (InboxRecord, error) {
	if err := s.requireTx(); err != nil {
		return InboxRecord{}, err
	}
	if outboundProvider == "" {
		outboundProvider = "smtp"
	}
	rec := InboxRecord{
		ID:      inboxID,
		OrgID:   orgID,
		Address: address,
		Status:  "active",

		InboundProvider:  "jmap",
		OutboundProvider: outboundProvider,
	}

	var domainRef any
	if orgDomainID == "" {
		domainRef = nil
	} else {
		domainRef = orgDomainID
		rec.OrgDomainID = sql.NullString{String: orgDomainID, Valid: true}
	}

	row := s.q.QueryRowContext(ctx, `
		INSERT INTO inboxes (id, org_id, org_domain_id, address, status, outbound_provider, external_ref)
		VALUES ($1, $2, $3, $4, 'active', $5, nullif($6, ''))
		RETURNING created_at
	`, rec.ID, rec.OrgID, domainRef, rec.Address, outboundProvider, externalRef)
	if err := row.Scan(&rec.CreatedAt); err != nil {
		if isCanonicalInboxAddressConflict(err) {
			return InboxRecord{}, ErrResourceConflict
		}
		return InboxRecord{}, err
	}
	return rec, nil
}

// EnsureInboxForOrg is the idempotent provisioning path. The external_ref may
// be replayed only with the same org/canonical-address/domain/provider tuple.
func (s *Store) EnsureInboxForOrg(ctx context.Context, orgID, address, orgDomainID, outboundProvider, externalRef string) (InboxRecord, bool, error) {
	canonicalAddress, canonicalDomain, err := canonicalInboxAddress(address)
	if err != nil {
		return InboxRecord{}, false, err
	}
	externalRef = strings.TrimSpace(externalRef)
	if externalRef == "" {
		return InboxRecord{}, false, errors.New("missing external_ref")
	}
	var rec InboxRecord
	created := false
	err = s.withTx(ctx, func(scoped *Store) error {
		if _, err := scoped.lockInboxCanonicalNamespaces(ctx, canonicalDomain, orgDomainID); err != nil {
			return err
		}
		if err := scoped.lockActiveOrgForReconciliation(ctx, orgID); err != nil {
			return err
		}
		existing, existingErr := scoped.getInboxByCanonicalAddress(ctx, canonicalAddress, true)
		if existingErr == nil {
			if existing.ExternalRef.Valid && existing.ExternalRef.String == externalRef {
				if !inboxReplayMatches(existing, orgID, canonicalAddress, orgDomainID, outboundProvider) {
					return ErrIdempotencyConflict
				}
				rec, created = existing, false
				return nil
			}
			return ErrResourceConflict
		}
		if !errors.Is(existingErr, sql.ErrNoRows) {
			return existingErr
		}
		conflict, err := scoped.globalActiveInboxByCanonicalAddress(ctx, canonicalAddress)
		if err != nil {
			return err
		}
		if conflict {
			return ErrResourceConflict
		}
		var ensureErr error
		rec, created, ensureErr = scoped.ensureInboxForOrgLocked(
			ctx, orgID, canonicalAddress, orgDomainID, outboundProvider, externalRef,
		)
		return ensureErr
	})
	return rec, created, err
}

func canonicalInboxAddress(address string) (string, string, error) {
	canonicalAddress, _, canonicalDomain, err := emailaddr.Canonicalize(address)
	if err != nil {
		return "", "", err
	}
	return canonicalAddress, canonicalDomain, nil
}

func (s *Store) ensureInbox(ctx context.Context, address string) (string, error) {
	canonicalAddress, canonicalDomain, err := canonicalInboxAddress(address)
	if err != nil {
		return "", err
	}
	orgID, err := s.EnsureDefaultOrg(ctx)
	if err != nil {
		return "", err
	}

	var inboxID string
	err = s.withTx(ctx, func(scoped *Store) error {
		rec, lookupErr := scoped.getPreferredInboxByCanonicalAddress(ctx, canonicalAddress)
		if lookupErr == nil {
			inboxID, err = scoped.replayEnsuredInbox(ctx, rec, canonicalAddress, canonicalDomain, orgID, false)
			return err
		}
		if !errors.Is(lookupErr, sql.ErrNoRows) {
			return lookupErr
		}
		if err := scoped.lockCanonicalDomain(ctx, canonicalDomain); err != nil {
			return err
		}
		if rec, err := scoped.getPreferredInboxByCanonicalAddress(ctx, canonicalAddress); err == nil {
			inboxID, err = scoped.replayEnsuredInbox(ctx, rec, canonicalAddress, canonicalDomain, orgID, true)
			return err
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		inboxID = uuid.NewString()
		_, err := scoped.q.ExecContext(ctx, `
			INSERT INTO inboxes (id, org_id, address, status)
			VALUES ($1, $2, $3, 'active')
		`, inboxID, orgID, canonicalAddress)
		if isCanonicalInboxAddressConflict(err) {
			return ErrResourceConflict
		}
		return err
	})
	return inboxID, err
}

func (s *Store) replayEnsuredInbox(
	ctx context.Context,
	rec InboxRecord,
	canonicalAddress, canonicalDomain, defaultOrgID string,
	addressDomainLocked bool,
) (string, error) {
	if err := s.requireTx(); err != nil {
		return "", err
	}
	initialIdentity, err := s.readInboxLifecycleIdentity(ctx, rec.OrgID, rec.ID, false)
	if err != nil {
		return "", err
	}
	if initialIdentity.CanonicalAddress != canonicalAddress || initialIdentity.AddressDomain != canonicalDomain {
		return "", ErrResourceConflict
	}
	if addressDomainLocked && initialIdentity.LinkedDomain != "" && initialIdentity.LinkedDomain < canonicalDomain {
		// The row appeared only after the address-domain lock. Acquiring an
		// earlier-sorting linked namespace now would invert the global order.
		return "", ErrResourceConflict
	}
	if err := s.lockInboxLifecycleIdentity(ctx, initialIdentity); err != nil {
		return "", err
	}
	lockedIdentity, err := s.readInboxLifecycleIdentity(ctx, rec.OrgID, rec.ID, true)
	if err != nil {
		return "", err
	}
	if !initialIdentity.sameCanonicalLockIdentity(lockedIdentity) {
		return "", ErrResourceConflict
	}
	lockedRec, err := s.getPreferredInboxByCanonicalAddress(ctx, canonicalAddress)
	if err != nil {
		return "", err
	}
	if lockedRec.ID != rec.ID {
		return "", ErrResourceConflict
	}
	if _, err := s.q.ExecContext(ctx, `
		UPDATE inboxes SET org_id = COALESCE(org_id, $2) WHERE id = $1
	`, rec.ID, defaultOrgID); err != nil {
		return "", err
	}
	return rec.ID, nil
}

func inboxReplayMatches(rec InboxRecord, orgID, canonicalAddress, orgDomainID, outboundProvider string) bool {
	if outboundProvider == "" {
		outboundProvider = "smtp"
	}
	storedAddress, _, err := canonicalInboxAddress(rec.Address)
	return err == nil && rec.OrgID == orgID && storedAddress == canonicalAddress &&
		rec.OrgDomainID.String == orgDomainID && rec.OutboundProvider == outboundProvider
}

func (s *Store) ensureInboxForOrgLocked(ctx context.Context, orgID, address, orgDomainID, outboundProvider, externalRef string) (InboxRecord, bool, error) {
	if err := s.requireTx(); err != nil {
		return InboxRecord{}, false, err
	}
	if outboundProvider == "" {
		outboundProvider = "smtp"
	}

	rec := InboxRecord{
		ID:               uuid.NewString(),
		OrgID:            orgID,
		Address:          address,
		Status:           "active",
		ExternalRef:      sql.NullString{String: externalRef, Valid: true},
		InboundProvider:  "jmap",
		OutboundProvider: outboundProvider,
	}
	var domainRef any
	if orgDomainID != "" {
		domainRef = orgDomainID
		rec.OrgDomainID = sql.NullString{String: orgDomainID, Valid: true}
	}

	// Use the external-ref unique index as the serialization point so two
	// reconcilers cannot both pass a pre-read and make the loser surface a raw
	// unique-constraint error.
	err := s.q.QueryRowContext(ctx, `
		INSERT INTO inboxes
		  (id, org_id, org_domain_id, address, status, outbound_provider, external_ref)
		VALUES ($1, $2, $3, $4, 'active', $5, $6)
		ON CONFLICT (external_ref) WHERE external_ref IS NOT NULL DO NOTHING
		RETURNING created_at
	`, rec.ID, rec.OrgID, domainRef, rec.Address, outboundProvider, externalRef).Scan(&rec.CreatedAt)
	if err == nil {
		return rec, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		if isCanonicalInboxAddressConflict(err) {
			return InboxRecord{}, false, ErrResourceConflict
		}
		return InboxRecord{}, false, err
	}

	rec, err = s.GetInboxByExternalRef(ctx, externalRef)
	if err != nil {
		return InboxRecord{}, false, err
	}
	if !inboxReplayMatches(rec, orgID, address, orgDomainID, outboundProvider) {
		return InboxRecord{}, false, ErrIdempotencyConflict
	}
	return rec, false, nil
}

func isCanonicalInboxAddressConflict(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "uq_inboxes_canonical_address"
}

func (s *Store) GetInboxByExternalRef(ctx context.Context, externalRef string) (InboxRecord, error) {
	var rec InboxRecord
	row := s.q.QueryRowContext(ctx, `
		SELECT id, org_id, org_domain_id::text, address, status, created_at,
		       inbound_provider, outbound_provider,
		       inbound_provider_config_ref, outbound_provider_config_ref,
		       forward_to, external_ref
		FROM inboxes WHERE external_ref = $1
	`, strings.TrimSpace(externalRef))
	err := row.Scan(
		&rec.ID, &rec.OrgID, &rec.OrgDomainID, &rec.Address, &rec.Status, &rec.CreatedAt,
		&rec.InboundProvider, &rec.OutboundProvider,
		&rec.InboundProviderConfigRef, &rec.OutboundProviderConfigRef,
		&rec.ForwardTo, &rec.ExternalRef,
	)
	return rec, err
}

func (s *Store) ReactivateInboxForOrg(ctx context.Context, orgID, inboxID string) (bool, error) {
	var changed bool
	err := s.withTx(ctx, func(scoped *Store) error {
		identity, err := scoped.readInboxLifecycleIdentity(ctx, orgID, inboxID, false)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && identity.Status != "disabled") {
			return nil
		}
		if err != nil {
			return err
		}
		if err := scoped.lockInboxLifecycleIdentity(ctx, identity); err != nil {
			return err
		}
		// Inbox status is evidence the outbound policy reads. Canonical identity
		// is always locked first so every inbox writer has one lock order.
		if err := scoped.LockOrgPolicy(ctx, orgID); err != nil {
			return err
		}
		lockedIdentity, err := scoped.readInboxLifecycleIdentity(ctx, orgID, inboxID, true)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if lockedIdentity.Status != "disabled" {
			return nil
		}
		if !identity.sameCanonicalLockIdentity(lockedIdentity) {
			return ErrResourceConflict
		}
		if _, err := scoped.getInboxByCanonicalAddress(ctx, identity.CanonicalAddress, true); err == nil {
			return ErrResourceConflict
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		conflict, err := scoped.globalActiveInboxByCanonicalAddress(ctx, identity.CanonicalAddress)
		if err != nil {
			return err
		}
		if conflict {
			return ErrResourceConflict
		}
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE inboxes SET status = 'active'
			WHERE id = $1 AND org_id = $2 AND status = 'disabled'
		`, inboxID, orgID)
		if err != nil {
			if isCanonicalInboxAddressConflict(err) {
				return ErrResourceConflict
			}
			return err
		}
		n, err := result.RowsAffected()
		changed = n > 0
		return err
	})
	return changed, err
}

func (s *Store) UpdateInboxOutboundProvider(ctx context.Context, inboxID string, provider string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE inboxes SET outbound_provider = $2 WHERE id = $1
	`, inboxID, provider)
	return err
}

func (s *Store) UpdateInboxesOutboundProviderByDomain(ctx context.Context, orgDomainID string, provider string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE inboxes SET outbound_provider = $2 WHERE org_domain_id = $1
	`, orgDomainID, provider)
	return err
}

// MigrateInboxesOutboundToResend switches all inboxes still using smtp to resend.
// Called at startup when Resend is the configured provider.
func (s *Store) MigrateInboxesOutboundToResend(ctx context.Context) (int64, error) {
	result, err := s.q.ExecContext(ctx, `
		UPDATE inboxes
		SET outbound_provider = 'resend'
		WHERE outbound_provider = 'smtp'
	`)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// UpdateInboxForwardTo sets or clears the forwarding address on an inbox.
// Empty string clears forwarding.
func (s *Store) UpdateInboxForwardTo(ctx context.Context, orgID, inboxID, forwardTo string) error {
	var ft any
	if forwardTo == "" {
		ft = nil
	} else {
		ft = forwardTo
	}
	result, err := s.q.ExecContext(ctx, `
		UPDATE inboxes SET forward_to = $3 WHERE id = $1 AND org_id = $2
	`, inboxID, orgID, ft)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) DisableInboxForOrg(ctx context.Context, orgID string, inboxID string) (bool, error) {
	var changed bool
	err := s.withTx(ctx, func(scoped *Store) error {
		identity, err := scoped.readInboxLifecycleIdentity(ctx, orgID, inboxID, false)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := scoped.lockInboxLifecycleIdentity(ctx, identity); err != nil {
			return err
		}
		if err := scoped.LockOrgPolicy(ctx, orgID); err != nil {
			return err
		}
		lockedIdentity, err := scoped.readInboxLifecycleIdentity(ctx, orgID, inboxID, true)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if !identity.sameCanonicalLockIdentity(lockedIdentity) {
			return ErrResourceConflict
		}
		result, err := scoped.q.ExecContext(ctx, `
			UPDATE inboxes
			SET status = 'disabled'
			WHERE id = $1 AND org_id = $2
		`, inboxID, orgID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = rows > 0
		return nil
	})
	return changed, err
}

// DeleteInboxForOrg permanently removes a bootstrap-managed inbox and its
// cascading mailbox data. Tenant-facing deletion must continue to use
// DisableInboxForOrg so accidental user deletion remains recoverable.
func (s *Store) DeleteInboxForOrg(ctx context.Context, orgID string, inboxID string) (bool, error) {
	var changed bool
	err := s.withTx(ctx, func(scoped *Store) error {
		identity, err := scoped.readInboxLifecycleIdentity(ctx, orgID, inboxID, false)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if err := scoped.lockInboxLifecycleIdentity(ctx, identity); err != nil {
			return err
		}
		if err := scoped.LockOrgPolicy(ctx, orgID); err != nil {
			return err
		}
		lockedIdentity, err := scoped.readInboxLifecycleIdentity(ctx, orgID, inboxID, true)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if !identity.sameCanonicalLockIdentity(lockedIdentity) {
			return ErrResourceConflict
		}
		result, err := scoped.q.ExecContext(ctx, `
			DELETE FROM inboxes
			WHERE id = $1 AND org_id = $2
		`, inboxID, orgID)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = rows > 0
		return nil
	})
	return changed, err
}
