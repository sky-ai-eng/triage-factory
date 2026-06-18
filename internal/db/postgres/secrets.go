package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/aead"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// EnvSecretEncryptionKey names the .env var holding the 32-byte
// AES-256-GCM key this store encrypts org/user integration secrets with.
// Distinct from sessions.EnvSessionEncryptionKey so the two rotate
// independently — a session-key rotation must not invalidate stored
// integration secrets, and vice versa. Multi-mode boot loads it in
// internal/app/stores.go and injects it into New; local mode (keychain)
// ignores it.
const EnvSecretEncryptionKey = "TF_SECRET_ENCRYPTION_KEY"

// secretStore is the Postgres impl of db.SecretStore. It
// encrypts each secret app-side with an injected aead.Key and stores the
// opaque ciphertext + nonce in public.org_secrets — a normal RLS-gated
// table. This replaces the previous thin wrapper over the public.vault_*
// pgsodium functions, removing the container-filesystem root key whose
// loss-on-recreate made the old design unsafe to self-host.
//
// # Pool + door split (semantically identical to the old vault_* split)
//
// app is the claims-checked path: Put/Get/Delete and the per-user
// PutUser/GetUser/DeleteUser run on the app pool (tf_app). RLS on
// org_secrets enforces the org gate (and, for user rows, the user gate),
// so the caller must have set request.jwt.claims first — via
// (Store).WithTx (request handlers) or a claims-set tx (tests). A
// claims-less app-pool call sees zero rows (reads → ("", nil)) or fails
// WITH CHECK (writes), which is the intended failure mode.
//
// admin is the system/background path: GetSystem / GetUserSystem /
// PutUserSystem run on the supabase_admin pool, which bypasses RLS and
// trusts the explicit org_id / user_id args — no claims required. tf_app
// cannot reach those rows without claims, so the two doors stay legible
// (claims-checked for request handlers, explicit-trusted-args for system
// code). Inside WithTx the app half is the claims-set tx while the admin
// half stays the real admin pool; NewForTx, a test door, collapses both
// onto the tx, so its *System reads route through tf_app and see RLS —
// tests that exercise them use New(admin, app, key) directly.
//
// Cross-tenant reads return ("", nil) rather than raising (RLS filters
// the row), and cross-tenant writes raise 42501 (RLS WITH CHECK). The
// security property is identical to the vault gate; only a cross-tenant
// *read* shifts from "raise" to "empty", which actually matches the
// SecretStore "missing → ("", nil)" contract.
//
// The AES-GCM envelope is bound to its row identity via additional
// authenticated data: every Put/Get derives a canonical AAD
// from (org_id, user_id, key) — see secretAAD — and threads it through
// aead.Encrypt/Decrypt. A (ciphertext, nonce) blob therefore only decrypts
// in the exact row it was written to. A direct-DB-write attacker who
// bypasses the app layer (a SQL-injection write, a compromised admin
// connection, a malicious operator) cannot relocate a valid blob from one
// row into another — e.g. org A's PAT ciphertext into org B's row — and
// have it decrypt: the relocated blob fails its GCM auth tag. This is
// defense-in-depth beyond the primary threat model (the SecretStore API
// only ever encrypts a supplied plaintext, and RLS gates the normal paths).
type secretStore struct {
	app   queryer
	admin queryer
	key   aead.Key
}

func newSecretStore(app, admin queryer, key aead.Key) db.SecretStore {
	return &secretStore{app: app, admin: admin, key: key}
}

var _ db.SecretStore = (*secretStore)(nil)

// upsertSQL writes (or rotates) one row. $2 is the user_id: pass nil for
// org-scope (stored NULL), the user's id for per-user. The ON CONFLICT
// target names the exact expression of org_secrets_scope_key_uniq so
// rotations overwrite in place — including description, so passing ""
// on rotation clears it (parity with the prior vault behavior;
// description is informational only).
const upsertSQL = `
INSERT INTO public.org_secrets (org_id, user_id, key, ciphertext, nonce, description)
VALUES ($1::uuid, $2::uuid, $3::text, $4, $5, $6)
ON CONFLICT (org_id, COALESCE(user_id, '00000000-0000-0000-0000-000000000000'::uuid), key)
DO UPDATE SET ciphertext  = EXCLUDED.ciphertext,
              nonce       = EXCLUDED.nonce,
              description = EXCLUDED.description,
              updated_at  = now()`

// secretAAD builds the canonical associated-data that binds a secret
// row's AES-GCM envelope to its (org_id, user_id, key) identity.
// It is fed to aead.Encrypt/Decrypt as additional authenticated data, so a
// (ciphertext, nonce) blob written under one row only decrypts when read
// back under that same row: a blob relocated to a different org, user, or
// key by a direct-DB writer fails its auth tag instead of yielding the
// original plaintext.
//
// Layout is orgUUID(16) || userUUID(16) || key. The two UUIDs are parsed
// to their canonical 16-byte form so the binding is independent of how the
// caller spelled them (case, dashes, braces), and the two fixed-width
// prefixes make the encoding unambiguous — no two distinct identities can
// produce the same AAD bytes. Org-scope rows use the all-zero UUID for
// user_id, matching org_secrets_scope_key_uniq's COALESCE sentinel, so
// they can never collide with a real per-user row (an empty userID means
// org scope).
func secretAAD(orgID, userID, key string) ([]byte, error) {
	org, err := uuid.Parse(orgID)
	if err != nil {
		return nil, fmt.Errorf("secret aad: invalid org_id %q: %w", orgID, err)
	}
	var usr uuid.UUID // zero value == the all-zero org-scope sentinel
	if userID != "" {
		usr, err = uuid.Parse(userID)
		if err != nil {
			return nil, fmt.Errorf("secret aad: invalid user_id %q: %w", userID, err)
		}
	}
	aad := make([]byte, 0, len(org)+len(usr)+len(key))
	aad = append(aad, org[:]...)
	aad = append(aad, usr[:]...)
	aad = append(aad, key...)
	return aad, nil
}

// nullableUUID maps the org-scope empty userID to a SQL NULL (stored as
// user_id IS NULL, the COALESCE sentinel in org_secrets_scope_key_uniq)
// and a per-user id to the id itself. Centralizes the one spot that has to
// translate the store's "" == org-scope convention into the column's NULL.
func nullableUUID(userID string) any {
	if userID == "" {
		return nil
	}
	return userID
}

// upsert encrypts value and writes it on q. userID is "" for org scope
// (stored NULL) or the user's id for a per-user row; the AAD binds the
// ciphertext to that exact (org, user, key) identity. The error is
// returned raw: a cross-tenant write on the app pool trips the org_secrets
// RLS WITH CHECK and raises 42501, which is a genuine authorization
// failure that must keep its own message — NOT be relabeled as the
// "missing tf_app role; use *System" app-pool hint. That hint is only
// sound on reads, where RLS filters to zero rows instead of raising (see
// getOne).
func (s *secretStore) upsert(ctx context.Context, q queryer, orgID, userID, key, value, description string) error {
	aad, err := secretAAD(orgID, userID, key)
	if err != nil {
		return err
	}
	ct, nonce, err := s.key.Encrypt([]byte(value), aad)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, upsertSQL, orgID, nullableUUID(userID), key, ct, nonce, description)
	return err
}

// getOne runs a (ciphertext, nonce) SELECT on q, decrypts under aad, and
// maps no-row → ("", nil) — the SecretStore "missing is not an error"
// contract. A decrypt failure (wrong/rotated key, tampered bytes, or a
// blob relocated from another row so the aad no longer matches) surfaces
// as an error rather than garbage. wrapErr is set only for the app-pool
// reads (Get/GetUser): on a SELECT, RLS filters to zero rows rather than
// raising, so a 42501 there is unambiguously the bare-authenticator /
// missing-tf_app-grant misuse the hint diagnoses. Admin-pool reads (and
// every write/delete) pass false / return raw.
func (s *secretStore) getOne(ctx context.Context, q queryer, wrapErr bool, callsite string, aad []byte, query string, args ...any) (string, error) {
	var ct, nonce []byte
	err := q.QueryRowContext(ctx, query, args...).Scan(&ct, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		if wrapErr {
			return "", wrapAppPoolPermErr(err, callsite)
		}
		return "", err
	}
	plain, err := s.key.Decrypt(ct, nonce, aad)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// getOrg / getUser are the two read shapes. Each derives the row-identity
// AAD and the matching WHERE clause from the SAME (org[, user], key) args
// in one place, so the binding can never drift from the row it reads — the
// AAD passed to Decrypt always describes exactly the row the SELECT
// targeted. Get/GetSystem differ only in pool + wrapErr; likewise
// GetUser/GetUserSystem.
func (s *secretStore) getOrg(ctx context.Context, q queryer, wrapErr bool, callsite, orgID, key string) (string, error) {
	aad, err := secretAAD(orgID, "", key)
	if err != nil {
		return "", err
	}
	return s.getOne(ctx, q, wrapErr, callsite, aad,
		`SELECT ciphertext, nonce FROM public.org_secrets
		   WHERE org_id = $1::uuid AND user_id IS NULL AND key = $2::text`,
		orgID, key)
}

func (s *secretStore) getUser(ctx context.Context, q queryer, wrapErr bool, callsite, orgID, userID, key string) (string, error) {
	aad, err := secretAAD(orgID, userID, key)
	if err != nil {
		return "", err
	}
	return s.getOne(ctx, q, wrapErr, callsite, aad,
		`SELECT ciphertext, nonce FROM public.org_secrets
		   WHERE org_id = $1::uuid AND user_id = $2::uuid AND key = $3::text`,
		orgID, userID, key)
}

// del runs a scoped DELETE on the app pool and reports whether a row was
// removed (ok=false on no match, matching the interface's "did the write
// land" contract). DELETE has no WITH CHECK and RLS USING filters rather
// than raises, so a cross-tenant delete simply removes zero rows and
// returns (false, nil); any error is returned raw, not relabeled.
func (s *secretStore) del(ctx context.Context, query string, args ...any) (bool, error) {
	res, err := s.app.ExecContext(ctx, query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// --- Org scope: Put/Get/GetSystem/Delete ---

func (s *secretStore) Put(ctx context.Context, orgID, key, value, description string) error {
	return s.upsert(ctx, s.app, orgID, "", key, value, description)
}

func (s *secretStore) Get(ctx context.Context, orgID, key string) (string, error) {
	return s.getOrg(ctx, s.app, true, "secrets.Get", orgID, key)
}

// GetSystem reads an org-scoped secret on the supabase_admin pool — RLS
// bypassed, the passed orgID trusted. The admin-pool route is the system
// door; request handlers use the claims-checked Get.
func (s *secretStore) GetSystem(ctx context.Context, orgID, key string) (string, error) {
	return s.getOrg(ctx, s.admin, false, "secrets.GetSystem", orgID, key)
}

func (s *secretStore) Delete(ctx context.Context, orgID, key string) (bool, error) {
	return s.del(ctx,
		`DELETE FROM public.org_secrets
		   WHERE org_id = $1::uuid AND user_id IS NULL AND key = $2::text`,
		orgID, key)
}

// --- Per-user scope: PutUser/GetUser/GetUserSystem/PutUserSystem/DeleteUser ---
//
// Same app-pool claims-checked path as the org trio, but org_secrets RLS
// additionally gates user rows on user_id = tf.current_user_id() so a
// session acting as user A can't touch user B's secret. The key is opaque
// (the consumer composes host-scoping, e.g. "jira_token/<host>").

func (s *secretStore) PutUser(ctx context.Context, orgID, userID, key, value, description string) error {
	return s.upsert(ctx, s.app, orgID, userID, key, value, description)
}

// PutUserSystem writes a per-user secret on the supabase_admin pool — RLS
// bypassed, args trusted. The write-side mirror of GetUserSystem, for
// system code acting as a user (the Cloud OAuth refresh-token rotation
// write-back, which runs claims-free).
func (s *secretStore) PutUserSystem(ctx context.Context, orgID, userID, key, value, description string) error {
	return s.upsert(ctx, s.admin, orgID, userID, key, value, description)
}

func (s *secretStore) GetUser(ctx context.Context, orgID, userID, key string) (string, error) {
	return s.getUser(ctx, s.app, true, "secrets.GetUser", orgID, userID, key)
}

// GetUserSystem reads a per-user secret on the supabase_admin pool — RLS
// bypassed, args trusted. System door for code acting as a user (the
// write-actor resolver building a JiraClientForUser on a background path).
func (s *secretStore) GetUserSystem(ctx context.Context, orgID, userID, key string) (string, error) {
	return s.getUser(ctx, s.admin, false, "secrets.GetUserSystem", orgID, userID, key)
}

func (s *secretStore) DeleteUser(ctx context.Context, orgID, userID, key string) (bool, error) {
	return s.del(ctx,
		`DELETE FROM public.org_secrets
		   WHERE org_id = $1::uuid AND user_id = $2::uuid AND key = $3::text`,
		orgID, userID, key)
}
