-- +goose Up

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.enforce_inbox_domain_access() RETURNS trigger AS $$
DECLARE
  cloud_mode text;
  grant_active boolean;
BEGIN
  IF NEW.status <> 'active' THEN
    RETURN NEW;
  END IF;
  IF NEW.org_domain_id IS NULL THEN
    RETURN NEW;
  END IF;
  PERFORM 1 FROM public.org_domains d
  WHERE d.id = NEW.org_domain_id
    AND d.status = 'active'
    AND d.org_id = NEW.org_id;
  IF FOUND THEN
    RETURN NEW;
  END IF;

  -- New revokers take this pair lock before their row lock. Keep the row lock
  -- as well so a migration-first rollout also serializes with old revokers.
  PERFORM pg_advisory_xact_lock(hashtextextended(
    'org-domain-grant:' || NEW.org_domain_id::text || ':' || NEW.org_id::text,
    0
  ));

  -- A grantee can SELECT its grant through RLS, but PostgreSQL applies the
  -- owner-only mutation policy to SELECT ... FOR KEY SHARE. Temporarily use
  -- the same narrowly scoped bypass as owner revocation, retain explicit
  -- domain/grantee predicates, and restore the transaction setting before
  -- inbox RLS evaluates the pending row.
  cloud_mode := coalesce(current_setting('app.cloud_mode', true), '');
  IF lower(cloud_mode) = 'true' THEN
    PERFORM set_config('app.cloud_mode', 'false', true);
  END IF;
  PERFORM 1
  FROM public.org_domain_grants g
  JOIN public.org_domains d ON d.id = g.org_domain_id
  WHERE d.id = NEW.org_domain_id
    AND d.status = 'active'
    AND g.grantee_org_id = NEW.org_id
    AND g.status = 'active'
  FOR KEY SHARE OF g;
  grant_active := FOUND;
  IF lower(cloud_mode) = 'true' THEN
    PERFORM set_config('app.cloud_mode', cloud_mode, true);
  END IF;
  IF grant_active THEN
    RETURN NEW;
  END IF;

  RAISE EXCEPTION 'inbox org has no active access to domain' USING ERRCODE = '23514';
END;
$$ LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp;
-- +goose StatementEnd

-- +goose Down

-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM public.org_domain_grants) THEN
    RAISE EXCEPTION 'cannot roll back core migration 0027: domain grants exist';
  END IF;
END $$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION public.enforce_inbox_domain_access() RETURNS trigger AS $$
BEGIN
  IF NEW.status <> 'active' THEN
    RETURN NEW;
  END IF;
  IF NEW.org_domain_id IS NULL THEN
    RETURN NEW;
  END IF;
  PERFORM 1 FROM public.org_domains d
  WHERE d.id = NEW.org_domain_id
    AND d.status = 'active'
    AND d.org_id = NEW.org_id;
  IF FOUND THEN
    RETURN NEW;
  END IF;

  PERFORM 1
  FROM public.org_domain_grants g
  JOIN public.org_domains d ON d.id = g.org_domain_id
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
