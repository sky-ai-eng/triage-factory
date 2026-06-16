-- +goose Up
-- vault_put_user_secret_system — the write-side mirror of
-- vault_get_user_secret_system: persist a per-user secret WITHOUT a request JWT,
-- for system/background code acting as a user. The motivating caller is the
-- Cloud OAuth access-token minter — Atlassian rotates the refresh token on every
-- refresh, so the minter must write the new refresh token back into the user's
-- credential envelope while running on the claims-free admin pool (the
-- write-actor resolver holds no JWT). The claims-checked vault_put_user_secret
-- is unreachable there, exactly as the read path needs the _system door.
--
-- Same name convention as the rest of the per-user vault wrappers:
-- 'org/<org_id>/user/<user_id>/<key>'. No current_org_id()/current_user_id()
-- gate — p_org_id + p_user_id are trusted because the EXECUTE grant restricts
-- this to the admin/system pool (tf_app has none). A NULL org/user is a caller
-- bug, refused explicitly rather than building a malformed name.

-- +goose StatementBegin
CREATE FUNCTION public.vault_put_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text DEFAULT NULL::text) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path TO 'pg_catalog', 'public'
    AS $$
DECLARE
  v_full_name TEXT := 'org/' || p_org_id::text || '/user/' || p_user_id::text || '/' || p_key;
  v_existing  UUID;
  v_desc      TEXT := COALESCE(p_description, '');
BEGIN
  IF p_org_id IS NULL OR p_user_id IS NULL THEN
    RAISE EXCEPTION 'system Vault access denied: p_org_id or p_user_id is NULL'
      USING ERRCODE = '22004';
  END IF;
  SELECT id INTO v_existing FROM vault.decrypted_secrets WHERE name = v_full_name;
  IF v_existing IS NOT NULL THEN
    PERFORM vault.update_secret(v_existing, p_secret, v_full_name, v_desc);
    RETURN v_existing;
  END IF;
  RETURN vault.create_secret(p_secret, v_full_name, v_desc);
END;
$$;
-- +goose StatementEnd

-- System/admin pool ONLY. Deliberately NOT granted to tf_app: the app pool must
-- stay on the claims-checked vault_put_user_secret. Mirrors the
-- vault_get_user_secret_system grant matrix exactly.
REVOKE ALL ON FUNCTION public.vault_put_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) FROM PUBLIC;
REVOKE ALL ON FUNCTION public.vault_put_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) FROM anon, authenticated, service_role;
GRANT ALL ON FUNCTION public.vault_put_user_secret_system(p_org_id uuid, p_user_id uuid, p_key text, p_secret text, p_description text) TO postgres;

-- +goose Down
SELECT 'down not supported';
