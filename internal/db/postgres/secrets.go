package postgres

import (
	"context"
	"database/sql"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// secretStore is the Postgres impl of db.SecretStore. Thin wrapper
// over the public.vault_* SECURITY DEFINER functions from D3 — those
// own the actual Vault interaction, the "org/<uuid>/<key>" naming
// convention, and the gate that p_org_id must match
// tf.current_org_id() on the caller's JWT claim.
//
// # Pool + ceremony
//
// All three methods run on the app pool (vault_* are GRANTed to
// tf_app only). The caller is expected to have set request.jwt.claims
// with the right org_id before calling — either via
// (Store).WithTx (request-handler path) or by being itself inside a
// claims-set tx (test path). If neither is true, the wrapper refuses
// with "Vault access denied: missing org context", which is the
// intended failure mode for a misconfigured caller.
//
// We don't auto-wrap each method in its own claims-set tx because
// that hides the contract — the secret subsystem belongs to D5, and
// D5 will wire it through WithTx for request-driven access plus the
// startup-time admin pool helpers we don't yet have. For now the
// surface is honest about the requirement.
//
// # GetSystem's separate door
//
// app is the claims-checked path (Put/Get/Delete → vault_*, GRANTed
// to tf_app, gated on p_org_id = current_org_id()). admin is the
// system/background path: GetSystem runs vault_get_org_secret_system
// on the supabase_admin pool, which trusts the explicit orgID and
// does NO claims check. tf_app has no EXECUTE on that function, so
// the two doors stay legible — claims-checked for request handlers,
// explicit-trusted-orgID for system code. Inside WithTx the app half
// is the claims-set tx while the admin half stays the real admin pool
// (GetSystem can't run on tf_app); NewForTx, a test door, collapses
// both onto the tx, so its GetSystem hits tf_app and is denied — tests
// that exercise GetSystem use New(admin, app) directly.
type secretStore struct {
	app   queryer
	admin queryer
}

func newSecretStore(app, admin queryer) db.SecretStore {
	return &secretStore{app: app, admin: admin}
}

var _ db.SecretStore = (*secretStore)(nil)

func (s *secretStore) Put(ctx context.Context, orgID, key, value, description string) error {
	// Description is optional at the SQL level (`DEFAULT NULL`,
	// COALESCEd to ''). Pass empty-string-as-nil so the wrapper's
	// behavior matches whether the caller supplied a description.
	var desc any
	if description != "" {
		desc = description
	}
	_, err := s.app.ExecContext(ctx,
		`SELECT public.vault_put_org_secret($1::uuid, $2::text, $3::text, $4)`,
		orgID, key, value, desc,
	)
	return err
}

func (s *secretStore) Get(ctx context.Context, orgID, key string) (string, error) {
	var got sql.NullString
	// vault_get_org_secret always returns exactly one row — NULL when
	// the secret doesn't exist, the value otherwise. Any error
	// (including sql.ErrNoRows) means the wrapper's shape regressed
	// or the connection failed; let it propagate so the caller sees
	// the anomaly instead of an "" that looks like "not configured".
	if err := s.app.QueryRowContext(ctx,
		`SELECT public.vault_get_org_secret($1::uuid, $2::text)`,
		orgID, key,
	).Scan(&got); err != nil {
		return "", err
	}
	if !got.Valid {
		return "", nil
	}
	return got.String, nil
}

// GetSystem reads an org-scoped secret on the supabase_admin pool via
// vault_get_org_secret_system — no request JWT, no current_org_id()
// check; the passed orgID is trusted. tf_app has no EXECUTE on the
// system function, so this path is reachable only from system code
// holding the admin pool. Mirrors Get's NULL → ("", nil) shape.
func (s *secretStore) GetSystem(ctx context.Context, orgID, key string) (string, error) {
	var got sql.NullString
	if err := s.admin.QueryRowContext(ctx,
		`SELECT public.vault_get_org_secret_system($1::uuid, $2::text)`,
		orgID, key,
	).Scan(&got); err != nil {
		return "", err
	}
	if !got.Valid {
		return "", nil
	}
	return got.String, nil
}

func (s *secretStore) Delete(ctx context.Context, orgID, key string) (bool, error) {
	var ok sql.NullBool
	err := s.app.QueryRowContext(ctx,
		`SELECT public.vault_delete_org_secret($1::uuid, $2::text)`,
		orgID, key,
	).Scan(&ok)
	if err != nil {
		return false, err
	}
	// vault_delete_org_secret returns BOOLEAN (true when a row was
	// removed). A NULL result is unexpected but harmless — treat it
	// as "nothing to delete" rather than synthesizing an error.
	return ok.Valid && ok.Bool, nil
}
