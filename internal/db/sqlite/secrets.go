package sqlite

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// secretStore is the SQLite-mode impl of db.SecretStore. Local mode
// keeps long-lived credentials in the OS keychain (no DB row), so the
// store delegates each Put/Get/Delete to internal/auth's keyed
// keychain helpers.
//
// # Why route through SecretStore at all in local mode
//
// The Postgres impl is the canonical consumer of this interface
// (vault-backed, RLS-gated per org). The local impl exists so callers
// that don't want to branch on runmode — most importantly the per-org
// credential resolver SKY-322 will add — can issue one Get call and
// have it work in either mode. In local mode there's a single org
// (runmode.LocalDefaultOrg) and a single keychain bag; orgID is
// asserted to match the sentinel and otherwise ignored.
//
// # orgID validation
//
// orgID MUST equal runmode.LocalDefaultOrg in local mode. A mismatched
// orgID is a caller bug (probably forgot to extract the request orgID
// via the SKY-316 accessor), and silently writing to the wrong bag
// would be a footgun. The error surfaces the caller mistake instead.
type secretStore struct{}

func newSecretStore() db.SecretStore { return &secretStore{} }

var _ db.SecretStore = (*secretStore)(nil)

func (*secretStore) Put(_ context.Context, orgID, key, value, _ string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return auth.PutSecret(key, value)
}

func (*secretStore) Get(_ context.Context, orgID, key string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	return auth.GetSecret(key)
}

// GetSystem == Get in local mode. There's a single org and a single
// keychain bag with no RLS and no claims, so there's nothing to
// differentiate the system path from the claims-checked one — the
// distinction only exists in multi mode's Postgres impl. assertLocalOrg
// still fires so a stray real UUID surfaces as a caller bug.
func (*secretStore) GetSystem(_ context.Context, orgID, key string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	return auth.GetSecret(key)
}

func (*secretStore) Delete(_ context.Context, orgID, key string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// The keychain helper doesn't report whether a row was actually
	// removed, so probe before deleting to give callers the (ok bool)
	// contract the interface promises. Use HasKeychainEntry rather
	// than GetSecret so the probe bypasses the TRIAGE_FACTORY_* env
	// overlay — DeleteSecret can't remove env vars, so reporting
	// ok=true based on an env-supplied value would lie to the caller
	// (subsequent Get would still return the env value). Matches the
	// Postgres impl's behavior of returning ok=false when no row was
	// removed.
	if !auth.HasKeychainEntry(key) {
		return false, nil
	}
	if err := auth.DeleteSecret(key); err != nil {
		return false, err
	}
	return true, nil
}
