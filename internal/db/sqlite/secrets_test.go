package sqlite_test

import (
	"context"
	"testing"

	"github.com/zalando/go-keyring"
	_ "modernc.org/sqlite"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSecretStore_SQLite_KeychainRoundTrip pins the local-mode
// SecretStore contract: Put → keychain entry, Get → same value,
// Delete → ok=true and subsequent Get returns "". Uses
// keyring.MockInit so the test exercises the production code path
// (auth.GetSecret / PutSecret / DeleteSecret) against an in-memory
// keychain — no OS keychain required in CI.
//
// Establishes the local-equals-multi-at-N=1 framing for the secrets
// layer: callers see the same Put/Get/Delete shape in either mode,
// SKY-322's credential resolver can lean on this without branching
// on runmode.
func TestSecretStore_SQLite_KeychainRoundTrip(t *testing.T) {
	keyring.MockInit()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	if err := stores.Secrets.Put(ctx, org, "anthropic_api_key", "sk-ant-test-v1", "local-mode test secret"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := stores.Secrets.Get(ctx, org, "anthropic_api_key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-ant-test-v1" {
		t.Errorf("Get got=%q want sk-ant-test-v1", got)
	}

	// Rotation: Put on the same key overwrites the stored value.
	if err := stores.Secrets.Put(ctx, org, "anthropic_api_key", "sk-ant-test-v2", ""); err != nil {
		t.Fatalf("Put rotation: %v", err)
	}
	got, err = stores.Secrets.Get(ctx, org, "anthropic_api_key")
	if err != nil {
		t.Fatalf("Get after rotation: %v", err)
	}
	if got != "sk-ant-test-v2" {
		t.Errorf("after rotation got=%q want sk-ant-test-v2", got)
	}

	// Missing key: Get returns "" without an error so callers can
	// distinguish "not configured" from "fetch failed."
	got, err = stores.Secrets.Get(ctx, org, "nonexistent")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if got != "" {
		t.Errorf("missing key got=%q want empty", got)
	}

	// Delete returns ok=true on a present key and ok=false on
	// already-absent keys — mirrors the Postgres impl's contract.
	ok, err := stores.Secrets.Delete(ctx, org, "anthropic_api_key")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !ok {
		t.Errorf("Delete ok=false for present key; want true")
	}
	ok, err = stores.Secrets.Delete(ctx, org, "anthropic_api_key")
	if err != nil {
		t.Fatalf("Delete (idempotent): %v", err)
	}
	if ok {
		t.Errorf("Delete on already-absent key ok=true; want false")
	}

	got, err = stores.Secrets.Get(ctx, org, "anthropic_api_key")
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if got != "" {
		t.Errorf("after Delete got=%q want empty", got)
	}
}

// TestSecretStore_SQLite_GetSystemEqualsGet pins the local-mode
// GetSystem contract: it forwards to the same keychain bag as Get and
// returns identical values. Local mode is single-org with no RLS and
// no claims, so the system path and the claims-checked path collapse
// onto the same keychain helper — there's nothing to differentiate.
// This is the "no regression in local mode" leg of SKY-364's
// acceptance.
func TestSecretStore_SQLite_GetSystemEqualsGet(t *testing.T) {
	keyring.MockInit()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	if err := stores.Secrets.Put(ctx, org, "github_app_pem", "-----BEGIN-----v1", ""); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// GetSystem returns the stored value, identically to Get.
	sysGot, err := stores.Secrets.GetSystem(ctx, org, "github_app_pem")
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	plainGot, err := stores.Secrets.Get(ctx, org, "github_app_pem")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sysGot != "-----BEGIN-----v1" || sysGot != plainGot {
		t.Errorf("GetSystem=%q Get=%q; want both %q", sysGot, plainGot, "-----BEGIN-----v1")
	}

	// Missing key: GetSystem returns "" without an error, mirroring Get.
	got, err := stores.Secrets.GetSystem(ctx, org, "nonexistent")
	if err != nil {
		t.Fatalf("GetSystem missing: %v", err)
	}
	if got != "" {
		t.Errorf("GetSystem missing got=%q want empty", got)
	}

	// A non-local orgID is a caller bug — assertLocalOrg fires here too.
	const realOrgUUID = "9b3c1f2d-0000-4000-8000-000000000001"
	if _, err := stores.Secrets.GetSystem(ctx, realOrgUUID, "github_app_pem"); err == nil {
		t.Errorf("GetSystem with non-local orgID succeeded; want error")
	}
}

// TestSecretStore_SQLite_DeleteWithOnlyEnvOverlay pins the Delete
// contract under the TRIAGE_FACTORY_* env overlay. auth.GetSecret
// returns the env value even when the keychain row doesn't exist, so
// probing with GetSecret would lie ("yes, I removed it") when in
// truth DeleteSecret can't touch env vars. The implementation uses
// auth.HasKeychainEntry to bypass the overlay — this test pins that
// behavior so a regression to the GetSecret probe surfaces.
func TestSecretStore_SQLite_DeleteWithOnlyEnvOverlay(t *testing.T) {
	keyring.MockInit()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID

	// Set the env overlay but write nothing to the keychain.
	t.Setenv("TRIAGE_FACTORY_GITHUB_PAT", "env-only-token")

	// Get reflects the env value (existing behavior — env wins).
	got, err := stores.Secrets.Get(ctx, org, "github_pat")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "env-only-token" {
		t.Fatalf("Get got=%q want env-only-token", got)
	}

	// Delete must return ok=false: there's no keychain row, and
	// reporting "removed" while the env var continues to surface the
	// value on the next Get would lie to the caller.
	ok, err := stores.Secrets.Delete(ctx, org, "github_pat")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ok {
		t.Errorf("Delete on env-only value ok=true; want false — env overlay can't be removed")
	}

	// Subsequent Get still returns the env value (unchanged by Delete).
	got, err = stores.Secrets.Get(ctx, org, "github_pat")
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if got != "env-only-token" {
		t.Errorf("Get after delete got=%q want env-only-token (env survives Delete)", got)
	}
}

// TestSecretStore_SQLite_RejectsNonLocalOrg pins the safety net for
// callers that forgot to extract the request orgID via the SKY-316
// accessor. Passing a real UUID into the local-mode store would
// otherwise silently write to a shared keychain bag and surface as a
// "missing secret" later — much harder to debug than an upfront
// rejection at the Put call.
func TestSecretStore_SQLite_RejectsNonLocalOrg(t *testing.T) {
	keyring.MockInit()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const realOrgUUID = "9b3c1f2d-0000-4000-8000-000000000001"
	if err := stores.Secrets.Put(ctx, realOrgUUID, "k", "v", ""); err == nil {
		t.Errorf("Put with non-local orgID succeeded; want error")
	}
	if _, err := stores.Secrets.Get(ctx, realOrgUUID, "k"); err == nil {
		t.Errorf("Get with non-local orgID succeeded; want error")
	}
	if _, err := stores.Secrets.Delete(ctx, realOrgUUID, "k"); err == nil {
		t.Errorf("Delete with non-local orgID succeeded; want error")
	}
}

// TestSecretStore_SQLite_PerUserKeychainRoundTrip pins the local-mode
// per-user SecretStore contract (SKY-442): PutUser → keychain entry,
// GetUser → same value, rotation overwrites, DeleteUser → ok=true then
// ok=false, and a missing key reads back "". Local mode is N=1 (one
// user), so there's no cross-user RLS to enforce here — that gate lives
// in the Postgres impl; this leg only proves the keychain delegation +
// user-namespaced key behave like the per-org methods.
func TestSecretStore_SQLite_PerUserKeychainRoundTrip(t *testing.T) {
	keyring.MockInit()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	const userID = "11111111-1111-1111-1111-111111111111"

	// Consumer composes a host-scoped key (the store treats it opaquely).
	const key = "jira_token/jira.example.com"

	if err := stores.Secrets.PutUser(ctx, org, userID, key, "pat-v1", "DC PAT"); err != nil {
		t.Fatalf("PutUser: %v", err)
	}

	got, err := stores.Secrets.GetUser(ctx, org, userID, key)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got != "pat-v1" {
		t.Errorf("GetUser got=%q want pat-v1", got)
	}

	// Rotation: PutUser on the same key overwrites the stored value.
	if err := stores.Secrets.PutUser(ctx, org, userID, key, "pat-v2", ""); err != nil {
		t.Fatalf("PutUser rotation: %v", err)
	}
	got, err = stores.Secrets.GetUser(ctx, org, userID, key)
	if err != nil {
		t.Fatalf("GetUser after rotation: %v", err)
	}
	if got != "pat-v2" {
		t.Errorf("after rotation got=%q want pat-v2", got)
	}

	// GetUserSystem returns the same value as GetUser — single bag, no RLS.
	sysGot, err := stores.Secrets.GetUserSystem(ctx, org, userID, key)
	if err != nil {
		t.Fatalf("GetUserSystem: %v", err)
	}
	if sysGot != "pat-v2" {
		t.Errorf("GetUserSystem got=%q want pat-v2 (== GetUser)", sysGot)
	}

	// PutUserSystem == PutUser locally (the OAuth rotation write-back door):
	// rotate via the system door, read back via the claims door.
	if err := stores.Secrets.PutUserSystem(ctx, org, userID, key, "pat-v3", ""); err != nil {
		t.Fatalf("PutUserSystem: %v", err)
	}
	got, err = stores.Secrets.GetUser(ctx, org, userID, key)
	if err != nil {
		t.Fatalf("GetUser after PutUserSystem: %v", err)
	}
	if got != "pat-v3" {
		t.Errorf("after PutUserSystem got=%q want pat-v3", got)
	}

	// Missing key: GetUser returns "" without an error.
	got, err = stores.Secrets.GetUser(ctx, org, userID, "nonexistent")
	if err != nil {
		t.Fatalf("GetUser missing: %v", err)
	}
	if got != "" {
		t.Errorf("missing key got=%q want empty", got)
	}

	// DeleteUser returns ok=true on a present key and ok=false on an
	// already-absent key — mirrors the per-org Delete contract.
	ok, err := stores.Secrets.DeleteUser(ctx, org, userID, key)
	if err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if !ok {
		t.Errorf("DeleteUser ok=false for present key; want true")
	}
	ok, err = stores.Secrets.DeleteUser(ctx, org, userID, key)
	if err != nil {
		t.Fatalf("DeleteUser (idempotent): %v", err)
	}
	if ok {
		t.Errorf("DeleteUser on already-absent key ok=true; want false")
	}

	got, err = stores.Secrets.GetUser(ctx, org, userID, key)
	if err != nil {
		t.Fatalf("GetUser after delete: %v", err)
	}
	if got != "" {
		t.Errorf("after DeleteUser got=%q want empty", got)
	}
}

// TestSecretStore_SQLite_PerUserKeyNamespacing pins that per-user keys
// don't collide with per-org keys (or other users' keys) in the single
// local keychain bag — the user-namespaced key ("user/<userID>/<key>")
// keeps the bags disjoint. A regression that dropped the namespace would
// let a per-org Put clobber a same-key per-user secret, or leak one
// user's value to another.
func TestSecretStore_SQLite_PerUserKeyNamespacing(t *testing.T) {
	keyring.MockInit()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	const (
		userA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		userB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
		key   = "jira_token"
	)

	// Same key written at org scope, user-A scope, and user-B scope must
	// all coexist as distinct values.
	if err := stores.Secrets.Put(ctx, org, key, "org-value", ""); err != nil {
		t.Fatalf("Put (org): %v", err)
	}
	if err := stores.Secrets.PutUser(ctx, org, userA, key, "user-a-value", ""); err != nil {
		t.Fatalf("PutUser (A): %v", err)
	}
	if err := stores.Secrets.PutUser(ctx, org, userB, key, "user-b-value", ""); err != nil {
		t.Fatalf("PutUser (B): %v", err)
	}

	orgGot, err := stores.Secrets.Get(ctx, org, key)
	if err != nil {
		t.Fatalf("Get (org): %v", err)
	}
	aGot, err := stores.Secrets.GetUser(ctx, org, userA, key)
	if err != nil {
		t.Fatalf("GetUser (A): %v", err)
	}
	bGot, err := stores.Secrets.GetUser(ctx, org, userB, key)
	if err != nil {
		t.Fatalf("GetUser (B): %v", err)
	}
	if orgGot != "org-value" || aGot != "user-a-value" || bGot != "user-b-value" {
		t.Errorf("namespacing collision: org=%q userA=%q userB=%q; want distinct org-value/user-a-value/user-b-value", orgGot, aGot, bGot)
	}

	// Deleting user A's key leaves the org key and user B's key intact.
	if _, err := stores.Secrets.DeleteUser(ctx, org, userA, key); err != nil {
		t.Fatalf("DeleteUser (A): %v", err)
	}
	if v, _ := stores.Secrets.Get(ctx, org, key); v != "org-value" {
		t.Errorf("org key disturbed by per-user delete: got=%q want org-value", v)
	}
	if v, _ := stores.Secrets.GetUser(ctx, org, userB, key); v != "user-b-value" {
		t.Errorf("user B key disturbed by user A delete: got=%q want user-b-value", v)
	}
}

// TestSecretStore_SQLite_PerUserRejectsNonLocalOrg pins the same
// safety net as the per-org variant: a real UUID passed into the
// local-mode store is a caller bug (forgot to extract the request
// orgID), and every per-user method rejects it upfront rather than
// silently writing to the shared keychain bag.
func TestSecretStore_SQLite_PerUserRejectsNonLocalOrg(t *testing.T) {
	keyring.MockInit()
	conn := openSQLiteForTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const (
		realOrgUUID = "9b3c1f2d-0000-4000-8000-000000000001"
		userID      = "11111111-1111-1111-1111-111111111111"
	)
	if err := stores.Secrets.PutUser(ctx, realOrgUUID, userID, "k", "v", ""); err == nil {
		t.Errorf("PutUser with non-local orgID succeeded; want error")
	}
	if _, err := stores.Secrets.GetUser(ctx, realOrgUUID, userID, "k"); err == nil {
		t.Errorf("GetUser with non-local orgID succeeded; want error")
	}
	if _, err := stores.Secrets.GetUserSystem(ctx, realOrgUUID, userID, "k"); err == nil {
		t.Errorf("GetUserSystem with non-local orgID succeeded; want error")
	}
	if err := stores.Secrets.PutUserSystem(ctx, realOrgUUID, userID, "k", "v", ""); err == nil {
		t.Errorf("PutUserSystem with non-local orgID succeeded; want error")
	}
	if _, err := stores.Secrets.DeleteUser(ctx, realOrgUUID, userID, "k"); err == nil {
		t.Errorf("DeleteUser with non-local orgID succeeded; want error")
	}
}
