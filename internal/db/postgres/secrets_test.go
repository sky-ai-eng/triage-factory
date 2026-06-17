package postgres_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/aead"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
)

// TestSecretStore_Postgres_RoundTrip exercises the full Put / Get /
// Delete cycle against public.org_secrets (TFAC-402). Runs inside a
// WithUser tx so the org_secrets_org RLS policy's org gate is satisfied.
// We construct the store with pgstore.NewForTx(tx, pgtest.SecretKey) so
// every call rides the same connection that has SET LOCAL ROLE tf_app +
// the JWT claim, and encrypts/decrypts with the shared test key.
func TestSecretStore_Postgres_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)

		if err := stores.Secrets.Put(ctx, orgID, "github_pat", "ghp_alice_secret_v1", "primary GitHub token"); err != nil {
			return fmt.Errorf("Put: %w", err)
		}

		got, err := stores.Secrets.Get(ctx, orgID, "github_pat")
		if err != nil {
			return fmt.Errorf("Get: %w", err)
		}
		if got != "ghp_alice_secret_v1" {
			t.Errorf("Get got=%q want ghp_alice_secret_v1", got)
		}

		// Rotation: Put on the same key overwrites in place.
		if err := stores.Secrets.Put(ctx, orgID, "github_pat", "ghp_alice_secret_v2", ""); err != nil {
			return fmt.Errorf("Put rotation: %w", err)
		}
		got, err = stores.Secrets.Get(ctx, orgID, "github_pat")
		if err != nil {
			return fmt.Errorf("Get after rotation: %w", err)
		}
		if got != "ghp_alice_secret_v2" {
			t.Errorf("after rotation got=%q want ghp_alice_secret_v2", got)
		}

		// Missing key: Get returns "" without an error.
		got, err = stores.Secrets.Get(ctx, orgID, "nonexistent_key")
		if err != nil {
			return fmt.Errorf("Get missing: %w", err)
		}
		if got != "" {
			t.Errorf("missing key got=%q want empty", got)
		}

		// Delete returns ok=true on a present key, ok=false when absent.
		ok, err := stores.Secrets.Delete(ctx, orgID, "github_pat")
		if err != nil {
			return fmt.Errorf("Delete: %w", err)
		}
		if !ok {
			t.Errorf("Delete ok=false for present key; want true")
		}
		ok, err = stores.Secrets.Delete(ctx, orgID, "github_pat")
		if err != nil {
			return fmt.Errorf("Delete idempotent: %w", err)
		}
		if ok {
			t.Errorf("Delete on already-absent key ok=true; want false")
		}

		got, err = stores.Secrets.Get(ctx, orgID, "github_pat")
		if err != nil {
			return fmt.Errorf("Get after delete: %w", err)
		}
		if got != "" {
			t.Errorf("after Delete got=%q want empty", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestSecretStore_Postgres_CiphertextAtRest pins the whole point of
// TFAC-402: the stored bytes are AES ciphertext, not plaintext. A DB dump
// (raw-select on the admin pool, RLS bypassed) yields only opaque bytes —
// the value never appears in the column.
func TestSecretStore_Postgres_CiphertextAtRest(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const plaintext = "ghp_super_secret_value_at_rest"
	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.Put(ctx, orgID, "github_pat", plaintext, "")
	}); err != nil {
		t.Fatalf("seed via Put: %v", err)
	}

	var ct, nonce []byte
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT ciphertext, nonce FROM public.org_secrets
		   WHERE org_id = $1::uuid AND user_id IS NULL AND key = $2::text`,
		orgID, "github_pat",
	).Scan(&ct, &nonce); err != nil {
		t.Fatalf("raw select ciphertext: %v", err)
	}
	if len(ct) == 0 {
		t.Fatal("ciphertext column empty")
	}
	if len(nonce) != 12 {
		t.Errorf("nonce length %d, want 12 (AES-GCM)", len(nonce))
	}
	if bytes.Contains(ct, []byte(plaintext)) {
		t.Errorf("ciphertext at rest contains the plaintext %q — encryption did nothing", plaintext)
	}
	if string(ct) == plaintext {
		t.Error("ciphertext equals plaintext")
	}
}

// TestSecretStore_Postgres_CrossOrgIsolation pins the org gate now that
// it's RLS rather than a vault claim-check. A session with claims for
// orgA cannot reach orgB's secret:
//   - cross-org READ filters to zero rows → ("", nil) (the value is not
//     leaked; this also satisfies the SecretStore "missing → empty"
//     contract), and
//   - cross-org WRITE violates the WITH CHECK → 42501.
func TestSecretStore_Postgres_CrossOrgIsolation(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA := seedPgOrgAndUserForSecrets(t, h)
	orgB, userB := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// orgB seeds a secret for itself.
	if err := h.WithUser(t, userB, orgB, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.Put(ctx, orgB, "github_pat", "orgB_secret", "")
	}); err != nil {
		t.Fatalf("seed orgB secret: %v", err)
	}

	// Alice (claims orgA) passing orgB as the arg: RLS overrides the
	// WHERE org_id=orgB, so the read returns empty rather than orgB's value.
	if err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.Get(ctx, orgB, "github_pat")
		if err != nil {
			return fmt.Errorf("cross-org Get: %w", err)
		}
		if got != "" {
			t.Errorf("cross-org Get got=%q want empty (orgB secret must not leak)", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser A read: %v", err)
	}

	// Cross-org WRITE: Alice (claims orgA) writing to orgB must be refused
	// by the WITH CHECK (org_id must equal current_org_id()). Uses a key
	// that doesn't exist in orgB so this is a pure INSERT — the WITH CHECK
	// fires deterministically as 42501 (no ON CONFLICT path to muddy it).
	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.Put(ctx, orgB, "alice_steal_attempt", "stolen", "")
	})
	if err == nil {
		t.Fatalf("cross-org Put succeeded; org WITH CHECK broken")
	}
	pgtest.AssertRLSViolation(t, err)

	// orgB's secret is intact after the failed cross-org attempts.
	got, err := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey).Secrets.GetSystem(ctx, orgB, "github_pat")
	if err != nil {
		t.Fatalf("GetSystem orgB readback: %v", err)
	}
	if got != "orgB_secret" {
		t.Errorf("orgB secret got=%q want orgB_secret (untouched)", got)
	}
}

// TestSecretStore_Postgres_NonTfAppRoleRefused pins the grant matrix on
// public.org_secrets: tf_app holds SELECT/INSERT/UPDATE/DELETE; the
// standard Supabase roles (anon/authenticated/service_role) hold NONE, so
// a secrets table can never be reached by a PostgREST default role. This
// replaces the dropped vault wrappers' EXECUTE-grant lockdown.
func TestSecretStore_Postgres_NonTfAppRoleRefused(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Catalog-level pin: tf_app HAS the privileges; the denied roles
	// have none.
	const tbl = "public.org_secrets"
	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE"} {
		var tfHas bool
		if err := h.AdminDB.QueryRow(`SELECT has_table_privilege('tf_app', $1, $2)`, tbl, priv).Scan(&tfHas); err != nil {
			t.Fatalf("has_table_privilege(tf_app, %s): %v", priv, err)
		}
		if !tfHas {
			t.Errorf("tf_app LACKS %s on %s — breaks the call path", priv, tbl)
		}
		for _, role := range []string{"anon", "authenticated", "service_role"} {
			var has bool
			if err := h.AdminDB.QueryRow(`SELECT has_table_privilege($1, $2, $3)`, role, tbl, priv).Scan(&has); err != nil {
				t.Fatalf("has_table_privilege(%s, %s): %v", role, priv, err)
			}
			if has {
				t.Errorf("%s HAS %s on %s — secrets table grant lockdown broken", role, priv, tbl)
			}
		}
	}

	// Behavior-level pin: calling as a denied role raises permission-
	// denied (42501) at the privilege layer, before RLS is even reached.
	for _, role := range []string{"anon", "authenticated", "service_role"} {
		role := role
		t.Run(role, func(t *testing.T) {
			tx, err := h.AppDB.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()

			if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE `+role); err != nil {
				t.Fatalf("set role %s: %v", role, err)
			}
			// Claims set so the denial can only come from the missing
			// table grant, not a NULL-org RLS filter.
			claims := fmt.Sprintf(`{"sub":"%s","org_id":"%s"}`, userID, orgID)
			if _, err := tx.ExecContext(ctx, `SELECT set_config('request.jwt.claims', $1, true)`, claims); err != nil {
				t.Fatalf("set claims: %v", err)
			}

			probe := func(label, query string) {
				t.Helper()
				if _, err := tx.ExecContext(ctx, `SAVEPOINT sp`); err != nil {
					t.Fatalf("%s: savepoint: %v", label, err)
				}
				_, err := tx.ExecContext(ctx, query, orgID)
				pgtest.AssertRLSViolation(t, err)
				if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT sp`); err != nil {
					t.Fatalf("%s: rollback to savepoint: %v", label, err)
				}
			}
			probe("select", `SELECT ciphertext FROM public.org_secrets WHERE org_id = $1::uuid`)
			probe("insert", `INSERT INTO public.org_secrets (org_id, key, ciphertext, nonce) VALUES ($1::uuid, 'k', '\x00', '\x00')`)
			probe("delete", `DELETE FROM public.org_secrets WHERE org_id = $1::uuid`)
		})
	}
}

// TestSecretStore_Postgres_GetSystem_NoClaimsRead pins the system
// accessor: a background/system caller with NO request.jwt.claims reads an
// org secret via GetSystem (admin pool, RLS bypassed), while a claims-less
// tf_app session sees zero rows. This is the "two distinct doors" proof —
// explicit-trusted-orgID for system code, claims-gated RLS for handlers.
func TestSecretStore_Postgres_GetSystem_NoClaimsRead(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Seed through the claims-checked write path.
	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.Put(ctx, orgID, "github_app_pem", "-----BEGIN PEM----- v1", "App private key")
	}); err != nil {
		t.Fatalf("seed secret via Put: %v", err)
	}

	// GetSystem routes to the admin pool. No WithUser wrap, no claims.
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	got, err := stores.Secrets.GetSystem(ctx, orgID, "github_app_pem")
	if err != nil {
		t.Fatalf("GetSystem (no claims): %v", err)
	}
	if got != "-----BEGIN PEM----- v1" {
		t.Errorf("GetSystem got=%q want the seeded PEM", got)
	}

	// Absent key: ("", nil), mirroring Get.
	got, err = stores.Secrets.GetSystem(ctx, orgID, "nonexistent")
	if err != nil {
		t.Fatalf("GetSystem missing: %v", err)
	}
	if got != "" {
		t.Errorf("GetSystem missing got=%q want empty", got)
	}

	// The other door: a claims-less tf_app session reads zero rows
	// (current_org_id() is NULL, so org_secrets_org's org gate never
	// matches). Proves GetSystem isn't just "Get without the gate" — the
	// app pool genuinely can't see the row without claims.
	tx, err := h.AppDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `SET LOCAL ROLE tf_app`); err != nil {
		t.Fatalf("set role tf_app: %v", err)
	}
	var sink []byte
	err = tx.QueryRowContext(ctx,
		`SELECT ciphertext FROM public.org_secrets WHERE org_id = $1::uuid AND user_id IS NULL AND key = $2::text`,
		orgID, "github_app_pem",
	).Scan(&sink)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claims-less tf_app read = %v, want sql.ErrNoRows (RLS should hide the row)", err)
	}
}

// TestSecretStore_Postgres_WrongKeyDecryptError pins the rotation /
// wrong-key failure mode: a value encrypted under one key, read back with
// a different key, yields a clean decrypt error — never garbage plaintext
// and never a silent ("", nil) that looks like "not configured". This is
// the AEAD auth-tag guarantee surfacing through the store.
func TestSecretStore_Postgres_WrongKeyDecryptError(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var key1, key2 aead.Key
	if _, err := rand.Read(key1[:]); err != nil {
		t.Fatalf("seed key1: %v", err)
	}
	if _, err := rand.Read(key2[:]); err != nil {
		t.Fatalf("seed key2: %v", err)
	}

	// Write under key1.
	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, key1).Secrets.Put(ctx, orgID, "github_pat", "the_value", "")
	}); err != nil {
		t.Fatalf("Put under key1: %v", err)
	}

	// Read under key2 (the "rotated the env key" scenario).
	got, err := pgstore.New(h.AdminDB, h.AppDB, key2).Secrets.GetSystem(ctx, orgID, "github_pat")
	if err == nil {
		t.Fatalf("GetSystem with wrong key returned no error (got=%q); decrypt must fail", got)
	}
	if got != "" {
		t.Errorf("GetSystem with wrong key returned non-empty %q alongside the error", got)
	}
}

// TestSecretStore_Postgres_PerUser_RoundTrip exercises PutUser / GetUser /
// DeleteUser under matching (org, user) claims — the org_secrets_user RLS
// policy's org+user gate — and asserts an org-scope secret and a per-user
// secret can share the same key as distinct rows.
func TestSecretStore_Postgres_PerUser_RoundTrip(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Consumer composes a host-scoped key; the store treats it opaquely.
	const key = "jira_token/jira.example.com"

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		stores := pgstore.NewForTx(tx, pgtest.SecretKey)

		if err := stores.Secrets.PutUser(ctx, orgID, userID, key, "pat_v1", "DC PAT"); err != nil {
			return fmt.Errorf("PutUser: %w", err)
		}

		got, err := stores.Secrets.GetUser(ctx, orgID, userID, key)
		if err != nil {
			return fmt.Errorf("GetUser: %w", err)
		}
		if got != "pat_v1" {
			t.Errorf("GetUser got=%q want pat_v1", got)
		}

		// Rotation overwrites the same row.
		if err := stores.Secrets.PutUser(ctx, orgID, userID, key, "pat_v2", ""); err != nil {
			return fmt.Errorf("PutUser rotation: %w", err)
		}
		got, err = stores.Secrets.GetUser(ctx, orgID, userID, key)
		if err != nil {
			return fmt.Errorf("GetUser after rotation: %w", err)
		}
		if got != "pat_v2" {
			t.Errorf("after rotation got=%q want pat_v2", got)
		}

		// An org-scope secret with the SAME key is a distinct row (user_id
		// NULL vs the user's id) — the unique index keys on the scope too.
		if err := stores.Secrets.Put(ctx, orgID, key, "org_scope_value", ""); err != nil {
			return fmt.Errorf("Put org-scope same key: %w", err)
		}
		gotUser, err := stores.Secrets.GetUser(ctx, orgID, userID, key)
		if err != nil {
			return fmt.Errorf("GetUser after org Put: %w", err)
		}
		if gotUser != "pat_v2" {
			t.Errorf("per-user value got=%q want pat_v2 (org-scope Put must not clobber it)", gotUser)
		}
		gotOrg, err := stores.Secrets.Get(ctx, orgID, key)
		if err != nil {
			return fmt.Errorf("Get org-scope: %w", err)
		}
		if gotOrg != "org_scope_value" {
			t.Errorf("org-scope value got=%q want org_scope_value", gotOrg)
		}

		// Missing key → ("", nil).
		got, err = stores.Secrets.GetUser(ctx, orgID, userID, "nonexistent_key")
		if err != nil {
			return fmt.Errorf("GetUser missing: %w", err)
		}
		if got != "" {
			t.Errorf("missing key got=%q want empty", got)
		}

		// DeleteUser → ok=true on present, ok=false on already-absent.
		ok, err := stores.Secrets.DeleteUser(ctx, orgID, userID, key)
		if err != nil {
			return fmt.Errorf("DeleteUser: %w", err)
		}
		if !ok {
			t.Errorf("DeleteUser ok=false for present key; want true")
		}
		ok, err = stores.Secrets.DeleteUser(ctx, orgID, userID, key)
		if err != nil {
			return fmt.Errorf("DeleteUser idempotent: %w", err)
		}
		if ok {
			t.Errorf("DeleteUser on already-absent key ok=true; want false")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser: %v", err)
	}
}

// TestSecretStore_Postgres_PerUser_CrossUserDenied is the load-bearing
// cross-user test: two users in the SAME org, and user B must not reach
// user A's secret. With RLS, B's reads/deletes of A's row filter to empty
// (the row is invisible), and B's write under A's id is refused. The
// complement leg — B reading its OWN absent key returns ("", nil) — proves
// the filter is the per-row user gate, not a blanket block on B.
func TestSecretStore_Postgres_PerUser_CrossUserDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userA := seedPgOrgAndUserForSecrets(t, h)
	userB := seedPgSecondUserInOrg(t, h, orgID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const key = "jira_token/jira.example.com"

	// User A seeds a secret for itself.
	if err := h.WithUser(t, userA, orgID, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.PutUser(ctx, orgID, userA, key, "alice_pat", "")
	}); err != nil {
		t.Fatalf("seed A's secret: %v", err)
	}

	// B reading A's secret: A's row is invisible to B → ("", nil), not a leak.
	if err := h.WithUser(t, userB, orgID, func(tx *sql.Tx) error {
		got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.GetUser(ctx, orgID, userA, key)
		if err != nil {
			return fmt.Errorf("GetUser as B: %w", err)
		}
		if got != "" {
			t.Errorf("B reading A's secret got=%q want empty (must not leak)", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser B read: %v", err)
	}

	// B deleting A's secret: A's row is invisible → 0 rows → ok=false, no error.
	if err := h.WithUser(t, userB, orgID, func(tx *sql.Tx) error {
		ok, err := pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.DeleteUser(ctx, orgID, userA, key)
		if err != nil {
			return fmt.Errorf("DeleteUser as B: %w", err)
		}
		if ok {
			t.Errorf("DeleteUser as B targeting A returned ok=true; A's row must be invisible to B")
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser B delete: %v", err)
	}

	// B writing under A's id: WITH CHECK refuses (the new row's user_id
	// must equal current_user_id()=B). The conflicting A row is invisible,
	// so the UPSERT cannot silently overwrite it either — it errors.
	err := h.WithUser(t, userB, orgID, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.PutUser(ctx, orgID, userA, key, "forged", "")
	})
	if err == nil {
		t.Fatalf("PutUser as B targeting A's id succeeded; cross-user write gate broken")
	}

	// Complement: B reading its OWN absent key is allowed and empty.
	if err := h.WithUser(t, userB, orgID, func(tx *sql.Tx) error {
		got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.GetUser(ctx, orgID, userB, key)
		if err != nil {
			return fmt.Errorf("GetUser for B's own key: %w", err)
		}
		if got != "" {
			t.Errorf("B's own absent key got=%q want empty", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser B own key: %v", err)
	}

	// A's secret is intact after B's failed attempts.
	if err := h.WithUser(t, userA, orgID, func(tx *sql.Tx) error {
		got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.GetUser(ctx, orgID, userA, key)
		if err != nil {
			return fmt.Errorf("GetUser as A: %w", err)
		}
		if got != "alice_pat" {
			t.Errorf("A's secret got=%q want alice_pat (untouched by B)", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithUser A readback: %v", err)
	}
}

// TestSecretStore_Postgres_PerUser_MismatchedOrgDenied pins the org leg of
// the per-user gate: a session with claims for orgA writing with orgB as
// the arg is refused by WITH CHECK even when p_user_id matches.
func TestSecretStore_Postgres_PerUser_MismatchedOrgDenied(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgA, userA := seedPgOrgAndUserForSecrets(t, h)
	orgB, _ := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := h.WithUser(t, userA, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.PutUser(ctx, orgB, userA, "k", "stolen", "")
	})
	if err == nil {
		t.Fatalf("PutUser with mismatched orgID succeeded; org WITH CHECK broken")
	}
	pgtest.AssertRLSViolation(t, err)
}

// TestSecretStore_Postgres_GetUserSystem_NoClaimsRead pins the per-user
// system accessor: a claims-less caller reads a user's secret via
// GetUserSystem (admin pool, RLS bypassed) with explicit args.
func TestSecretStore_Postgres_GetUserSystem_NoClaimsRead(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const key = "jira_token/jira.example.com"

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.PutUser(ctx, orgID, userID, key, "alice_pat_v1", "DC PAT")
	}); err != nil {
		t.Fatalf("seed secret via PutUser: %v", err)
	}

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	got, err := stores.Secrets.GetUserSystem(ctx, orgID, userID, key)
	if err != nil {
		t.Fatalf("GetUserSystem (no claims): %v", err)
	}
	if got != "alice_pat_v1" {
		t.Errorf("GetUserSystem got=%q want the seeded PAT", got)
	}

	got, err = stores.Secrets.GetUserSystem(ctx, orgID, userID, "nonexistent")
	if err != nil {
		t.Fatalf("GetUserSystem missing: %v", err)
	}
	if got != "" {
		t.Errorf("GetUserSystem missing got=%q want empty", got)
	}
}

// TestSecretStore_Postgres_PutUserSystem_NoClaimsWriteBack pins the OAuth
// rotation write-back door: a claims-less system caller rotates a user's
// secret via PutUserSystem (admin pool), and the new value reads back
// through both the system door and the claims-checked one (single row, not
// a separate bag).
func TestSecretStore_Postgres_PutUserSystem_NoClaimsWriteBack(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)
	orgID, userID := seedPgOrgAndUserForSecrets(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const key = "jira_token/acme.atlassian.net"

	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.PutUser(ctx, orgID, userID, key, "envelope_v1", "Jira user access token")
	}); err != nil {
		t.Fatalf("seed via PutUser: %v", err)
	}

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	if err := stores.Secrets.PutUserSystem(ctx, orgID, userID, key, "envelope_v2", "Jira user access token"); err != nil {
		t.Fatalf("PutUserSystem (no claims): %v", err)
	}

	if got, err := stores.Secrets.GetUserSystem(ctx, orgID, userID, key); err != nil || got != "envelope_v2" {
		t.Fatalf("GetUserSystem after rotation = (%q, %v), want envelope_v2", got, err)
	}
	if err := h.WithUser(t, userID, orgID, func(tx *sql.Tx) error {
		got, err := pgstore.NewForTx(tx, pgtest.SecretKey).Secrets.GetUser(ctx, orgID, userID, key)
		if err != nil {
			return err
		}
		if got != "envelope_v2" {
			t.Errorf("GetUser after PutUserSystem rotation = %q, want envelope_v2", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("GetUser after rotation: %v", err)
	}
}

func seedPgOrgAndUserForSecrets(t *testing.T, h *pgtest.Harness) (orgID, userID string) {
	t.Helper()
	orgID = uuid.New().String()
	userID = uuid.New().String()
	email := fmt.Sprintf("secret-conf-%s@test.local", userID[:8])
	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(`INSERT INTO users (id, display_name) VALUES ($1, $2)`, userID, "Secret Conformance User"); err != nil {
		t.Fatalf("seed users: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO orgs (id, name, slug, owner_user_id) VALUES ($1, $2, $3, $4)`,
		orgID, "Secret Conformance Org "+orgID[:8], "secret-"+orgID[:8], userID); err != nil {
		t.Fatalf("seed orgs: %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'owner')`,
		orgID, userID); err != nil {
		t.Fatalf("seed org_memberships: %v", err)
	}
	seedPgDefaultTeam(t, h, orgID, userID)
	return orgID, userID
}

// seedPgSecondUserInOrg seeds an additional (member-role) user into an
// existing org so cross-user tests have two distinct principals under one
// tenant. The RLS per-user gate keys on JWT claims, but a real users row +
// membership keeps the fixture honest (valid FK target, realistic shape).
func seedPgSecondUserInOrg(t *testing.T, h *pgtest.Harness, orgID string) (userID string) {
	t.Helper()
	userID = uuid.New().String()
	email := fmt.Sprintf("secret-conf-2-%s@test.local", userID[:8])
	h.SeedAuthUser(t, userID, email)
	if _, err := h.AdminDB.Exec(`INSERT INTO users (id, display_name) VALUES ($1, $2)`, userID, "Secret Conformance User 2"); err != nil {
		t.Fatalf("seed users (2nd): %v", err)
	}
	if _, err := h.AdminDB.Exec(`INSERT INTO org_memberships (org_id, user_id, role) VALUES ($1, $2, 'member')`,
		orgID, userID); err != nil {
		t.Fatalf("seed org_memberships (2nd): %v", err)
	}
	return userID
}
