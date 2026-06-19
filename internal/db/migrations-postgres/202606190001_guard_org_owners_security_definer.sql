-- +goose Up
-- guard_org_owners is the global invariant "every org must retain ≥1 owner",
-- fired by AFTER UPDATE/DELETE statement triggers on org_memberships. The
-- baseline created it SECURITY INVOKER (the default), so its owner-existence
-- check ran under the mutating caller's RLS context — fine for an admin
-- removing/demoting someone (the admin still satisfies org_memberships_select
-- and sees every row), but BROKEN for a self-leave: the DELETE removes the
-- caller's own membership row, and org_memberships_select gates peer rows on
-- tf.user_has_org_access(), which now returns false for the just-departed
-- caller. The check then sees zero rows, concludes the org has no owner, and
-- raises a false 23514 — so a non-owner could never leave.
--
-- A global-invariant guard must evaluate the TRUE org state, not the caller's
-- RLS-filtered view. Make it SECURITY DEFINER (matching every other tf.*
-- helper) so the owner count is read past RLS. The body is otherwise the
-- baseline's verbatim; search_path stays pinned, so the definer rights are
-- safe. Behavior for admin-context mutations is unchanged (they already saw
-- the full set); this only repairs the self-delete case.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION tf.guard_org_owners() RETURNS trigger
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM affected ao
    WHERE NOT EXISTS (
      SELECT 1 FROM org_memberships
       WHERE org_id = ao.org_id AND role = 'owner'
    )
  ) THEN
    RAISE EXCEPTION 'each org must retain at least one owner role'
      USING ERRCODE = '23514';
  END IF;
  RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 'down not supported';
