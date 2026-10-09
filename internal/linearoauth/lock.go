package linearoauth

import (
	"context"
	"database/sql"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// credentialLockSalt namespaces CredentialLock in Postgres's advisory-lock
// keyspace; internal/server/advisorylock.go keeps the registry of salts.
const credentialLockSalt int64 = 10

// localCredentialLocks is CredentialLock's local-mode keyspace,
// map[orgID]*sync.Mutex. It is package state because local mode's credential
// is the process's keychain, which the server's handlers and the poller's
// token cache each reach through a CredentialLock of their own.
var localCredentialLocks sync.Map

// CredentialLock is the per-org lock every writer of an org's Linear
// credential holds: the bind, unbind and install handlers, the OAuth app's
// store and delete, and the token cache's rotation write-back and revoke. A
// check one of them makes — is an install live, which app minted it, is the
// stored refresh token still the one Linear refused — therefore still holds
// when its write lands, and local mode's restore of a failed write cannot
// put back a value another writer has since replaced. In multi mode it is a
// Postgres advisory lock, so it holds across pods; in local mode it is a
// keyed mutex.
type CredentialLock struct {
	db *sql.DB
}

// NewCredentialLock builds the lock over database, which takes the advisory
// lock in multi mode and is unused in local mode.
func NewCredentialLock(database *sql.DB) *CredentialLock {
	return &CredentialLock{db: database}
}

// Lock takes orgID's lock, returning its idempotent release.
func (l *CredentialLock) Lock(ctx context.Context, orgID string) (release func(), err error) {
	return db.AcquireKeyedLock(ctx, l.db, &localCredentialLocks, credentialLockSalt, orgID)
}
