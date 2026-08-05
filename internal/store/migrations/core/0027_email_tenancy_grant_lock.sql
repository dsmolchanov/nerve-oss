-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION enforce_inbox_domain_access() RETURNS trigger AS $$
BEGIN
  IF NEW.status <> 'active' THEN
    RETURN NEW;
  END IF;
  IF NEW.org_domain_id IS NULL THEN
    RETURN NEW;
  END IF;
  PERFORM 1 FROM org_domains d
  WHERE d.id = NEW.org_domain_id
    AND d.status = 'active'
    AND d.org_id = NEW.org_id;
  IF FOUND THEN
    RETURN NEW;
  END IF;

  -- Serialize activation with RevokeOrgDomainGrant without taking a row lock.
  -- A grantee can SELECT its grant through RLS, but PostgreSQL applies the
  -- owner-only mutation policy to SELECT ... FOR KEY SHARE.
  PERFORM pg_advisory_xact_lock(hashtextextended(
    'org-domain-grant:' || NEW.org_domain_id::text || ':' || NEW.org_id::text,
    0
  ));
  PERFORM 1
  FROM org_domain_grants g
  JOIN org_domains d ON d.id = g.org_domain_id
  WHERE d.id = NEW.org_domain_id
    AND d.status = 'active'
    AND g.grantee_org_id = NEW.org_id
    AND g.status = 'active';
  IF FOUND THEN
    RETURN NEW;
  END IF;

  RAISE EXCEPTION 'inbox org has no active access to domain' USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM org_domain_grants) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0027: domain grants exist';
  END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION enforce_inbox_domain_access() RETURNS trigger AS $$
BEGIN
  IF NEW.status <> 'active' THEN
    RETURN NEW;
  END IF;
  IF NEW.org_domain_id IS NULL THEN
    RETURN NEW;
  END IF;
  PERFORM 1 FROM org_domains d
  WHERE d.id = NEW.org_domain_id
    AND d.status = 'active'
    AND d.org_id = NEW.org_id;
  IF FOUND THEN
    RETURN NEW;
  END IF;

  PERFORM 1
  FROM org_domain_grants g
  JOIN org_domains d ON d.id = g.org_domain_id
  WHERE d.id = NEW.org_domain_id
    AND d.status = 'active'
    AND g.grantee_org_id = NEW.org_id
    AND g.status = 'active'
  FOR KEY SHARE OF g;
  IF FOUND THEN
    RETURN NEW;
  END IF;

  RAISE EXCEPTION 'inbox org has no active access to domain' USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
