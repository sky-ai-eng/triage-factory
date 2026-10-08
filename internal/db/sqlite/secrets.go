package sqlite

import (
	"context"
	"crypto/subtle"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/auth"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// secretStore is the SQLite-mode impl of db.SecretStore. Local mode
// keeps long-lived credentials outside the DB — in the OS keychain, or
// an encrypted file when no keychain is available (internal/auth picks
// the backend) — so the store delegates each Put/Get/Delete and the
// per-user PutUser/GetUser/DeleteUser to internal/auth's keyed secret
// helpers. The per-user methods namespace their key
// ("user/<userID>/<key>") so they don't collide with per-org keys in
// the single local bag; local mode is N=1 (one user), so there's no
// cross-user RLS to enforce — that gate only exists in the Postgres impl.
//
// # Why route through SecretStore at all in local mode
//
// The Postgres impl is the canonical consumer of this interface
// (vault-backed, RLS-gated per org). The local impl exists so callers
// that don't want to branch on runmode — most importantly the per-org
// credential resolver — can issue one Get call and
// have it work in either mode. In local mode there's a single org
// (runmode.LocalDefaultOrgID) and a single keychain bag; orgID is
// asserted to match the sentinel and otherwise ignored.
//
// # orgID validation
//
// orgID MUST equal runmode.LocalDefaultOrgID in local mode. A mismatched
// orgID is a caller bug (probably forgot to extract the request orgID
// via the correct accessor), and silently writing to the wrong bag
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
	return deleteStoredSecret(key)
}

// deleteStoredSecret deletes key and reports whether an entry was there. The
// auth secret helpers (keychain or encrypted-file backend) don't report
// whether a row was actually removed, so it probes before deleting to give
// callers the (ok bool) contract the interface promises — ok=false when
// nothing was stored, matching the Postgres impl. An empty value reads as
// absent: nothing stores empty except to mean unset.
func deleteStoredSecret(key string) (bool, error) {
	cur, err := auth.GetSecret(key)
	if err != nil {
		return false, err
	}
	if cur == "" {
		return false, nil
	}
	if err := auth.DeleteSecret(key); err != nil {
		return false, err
	}
	return true, nil
}

// userKeychainKey namespaces a per-user secret inside the single local
// keychain bag. Multi mode gets a real RLS gate (vault_*_user_secret);
// local mode is N=1 (one user), so the namespace is purely cosmetic —
// it keeps per-user keys from colliding with per-org ones in the same
// bag and mirrors the multi-mode Vault name shape
// ("org/<org>/user/<user>/<key>" vs the local "user/<user>/<key>",
// orgID being the implicit LocalDefaultOrgID sentinel).
func userKeychainKey(userID, key string) string {
	return "user/" + userID + "/" + key
}

func (*secretStore) PutUser(_ context.Context, orgID, userID, key, value, _ string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	userSecretsMu.Lock()
	defer userSecretsMu.Unlock()
	return auth.PutSecret(userKeychainKey(userID, key), value)
}

// PutUserSystem == PutUser in local mode. The system/claims-checked split only
// exists in multi mode's Postgres impl; local is N=1 (one user, one keychain
// bag, no RLS), so both write the same namespaced keychain entry. assertLocalOrg
// still fires so a stray real UUID surfaces as a caller bug.
func (*secretStore) PutUserSystem(_ context.Context, orgID, userID, key, value, _ string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	userSecretsMu.Lock()
	defer userSecretsMu.Unlock()
	return auth.PutSecret(userKeychainKey(userID, key), value)
}

func (*secretStore) GetUser(_ context.Context, orgID, userID, key string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	return auth.GetSecret(userKeychainKey(userID, key))
}

// GetUserSystem == GetUser in local mode. There's a single user and a
// single keychain bag with no RLS and no claims, so the system path and
// the claims-checked path collapse onto the same keychain helper — the
// cross-user distinction only exists in multi mode's Postgres impl.
// assertLocalOrg still fires so a stray real UUID surfaces as a caller bug.
func (*secretStore) GetUserSystem(_ context.Context, orgID, userID, key string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	return auth.GetSecret(userKeychainKey(userID, key))
}

func (*secretStore) DeleteUser(_ context.Context, orgID, userID, key string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	userSecretsMu.Lock()
	defer userSecretsMu.Unlock()
	return deleteStoredSecret(userKeychainKey(userID, key))
}

// userSecretsMu makes DeleteUserSystemIfValue's read and delete one step
// against every other per-user write. The keychain has no compare-and-delete,
// and local mode is one process per state root, so a lock held by every
// per-user writer in it is enough.
var userSecretsMu sync.Mutex

func (*secretStore) DeleteUserSystemIfValue(_ context.Context, orgID, userID, key, value string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	uk := userKeychainKey(userID, key)
	userSecretsMu.Lock()
	defer userSecretsMu.Unlock()
	cur, err := auth.GetSecret(uk)
	if err != nil {
		return false, err
	}
	if cur == "" {
		return false, nil
	}
	if subtle.ConstantTimeCompare([]byte(cur), []byte(value)) != 1 {
		return false, nil
	}
	if err := auth.DeleteSecret(uk); err != nil {
		return false, err
	}
	return true, nil
}
