package linearoauth

import (
	"context"
	"database/sql"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// CredentialLockSalt namespaces CredentialLock in Postgres's advisory-lock
// keyspace; internal/server/advisorylock.go keeps the registry of salts.
// Exported for a test that holds the lock from another session, the way
// another pod would, which no CredentialLock in its process can: they share
// the process's gates.
const CredentialLockSalt int64 = 10

// localCredentialLocks holds CredentialLock's in-process gates
// (db.AcquireKeyedLock), one per org. It is package state because every
// CredentialLock in the process has to share them: the server's handlers and
// the poller's token cache each hold one of their own, and in local mode the
// gates are the whole lock over the process's keychain.
var localCredentialLocks sync.Map

// CredentialLock is the per-org lock every writer of an org's Linear
// credential holds: the bind, unbind and install handlers, the OAuth app's
// store and delete, and the token cache's rotation write-back and revoke. A
// check one of them makes — is an install live, which app minted it, is the
// stored refresh token still the one Linear refused — therefore still holds
// when its write lands, and local mode's restore of a failed write cannot
// put back a value another writer has since replaced. In multi mode it is a
// Postgres advisory lock, so it holds across pods; in local mode it is a
// per-org gate (db.AcquireKeyedLock).
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
	return db.AcquireKeyedLock(ctx, l.db, &localCredentialLocks, CredentialLockSalt, orgID)
}
