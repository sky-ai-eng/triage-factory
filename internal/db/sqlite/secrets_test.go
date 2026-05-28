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
	org := runmode.LocalDefaultOrg

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
	org := runmode.LocalDefaultOrg

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
	org := runmode.LocalDefaultOrg

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
