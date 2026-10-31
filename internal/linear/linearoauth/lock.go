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
// put back a value another writer has since replaced. Because the bind,
// install and unbind that write org_settings.linear_workspace_* hold it, a
// writer of a user's credential takes it too, briefly, to read the workspace
// that keys the credential and write under it; it does so after its
// UserCredentialLock, the only order the two are ever taken in. In multi mode
// it is a Postgres advisory lock, so it holds across pods; in local mode it
// is a per-org gate (db.AcquireKeyedLock).
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

// UserCredentialLockSalt namespaces UserCredentialLock in Postgres's
// advisory-lock keyspace; internal/server/advisorylock.go keeps the registry.
const UserCredentialLockSalt int64 = 12

// localUserCredentialLocks is UserCredentialLock's in-process gates, package
// state for the reason localCredentialLocks is.
var localUserCredentialLocks sync.Map

// UserCredentialLock is the per-(org, user) lock every writer of a user's
// Linear credential holds: the key paste and the Connect callback's store,
// the disconnect, a member's removal, and UserTokenCache's rotation
// write-back and its removal of a credential Linear refused. Each of those
// writes the envelope and the identity row as one step, so a credential and
// the identity derived from it never come from two different binds. It is
// held only around database work, never across a call to Linear: in multi
// mode it is a Postgres session advisory lock, which holds a pool connection
// for as long as it is held; in local mode a per-key gate. It is not
// reentrant: a holder must not call linear.Resolver.ForUser for the same
// user, whose refresh takes it. A holder that needs the org's workspace
// stable takes CredentialLock after this one, never before.
type UserCredentialLock struct {
	db *sql.DB
}

// NewUserCredentialLock builds the lock over database, which takes the
// advisory lock in multi mode and is unused in local mode.
func NewUserCredentialLock(database *sql.DB) *UserCredentialLock {
	return &UserCredentialLock{db: database}
}

// Lock takes (orgID, userID)'s lock, returning its idempotent release.
func (l *UserCredentialLock) Lock(ctx context.Context, orgID, userID string) (release func(), err error) {
	return db.AcquireKeyedLock(ctx, l.db, &localUserCredentialLocks, UserCredentialLockSalt, orgID+":"+userID)
}
