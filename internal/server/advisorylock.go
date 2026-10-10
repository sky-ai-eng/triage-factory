package server

import (
	"context"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// acquireKeyedLock serializes a read-merge-write critical section keyed on
// key (an org id, ...) across the whole deployment, not just this process
// (TFAC-579): a Postgres advisory lock on s.db in multi mode, so two control
// pods cannot interleave the same org's read-merge-write, behind the key's
// in-process gate in mu, which is the whole lock in local mode. See
// db.AcquireKeyedLock.
func (s *Server) acquireKeyedLock(ctx context.Context, mu *sync.Map, salt int64, key string) (release func(), err error) {
	return db.AcquireKeyedLock(ctx, s.db, mu, salt, key)
}

// Advisory-lock salts (TFAC-579). hashtextextended's salt is a global
// namespace shared by every pg_advisory_lock/pg_advisory_xact_lock caller
// in the database (session and xact locks share one keyspace), not just
// this package — each guarded domain across the whole codebase needs its
// own value so two unrelated keyspaces can't accidentally collide.
//
// Registry of every hashtextextended salt in the codebase (keep this list
// current when claiming a new one):
//
//	0 — internal/db/postgres/team_github_repos.go (org id, xact)
//	    internal/server/auth_handlers.go          (email, xact)
//	    internal/db/migrations.go                 (fixed goose-migrate
//	    literal, session; disjoint key domains make sharing 0 safe)
//	1 — internal/server/auth_handlers.go          (auth user id, xact)
//	2 — ee/slack/store/pg                         (Slack api_app_id, xact)
//	5 — internal/db/postgres/tasks.go             (entity id, xact;
//	    entityTaskCreationLockSalt)
//	8 — this file                                 (org id, session)
//	9 — this file                                 (github host + installation
//	    id, session; githubInstallationBindLockSalt)
//	10 — internal/linearoauth                     (org id, session;
//	    CredentialLockSalt)
//	0x43484944 ("CHID") — ee/slack/store/pg                   (org id, xact;
//	    exclusive to move, shared to settle; channelMoveLockSalt)
//	0x53454154 ("SEAT") — internal/db/postgres/auth_events.go (seat period, xact)
//	0x544f4b4e ("TOKN") — internal/apitokens                  (user:org, xact)
//
// internal/auth/auth_provision.go's user lock hashes with FNV-1a in Go —
// a separate un-salted keyspace, listed here only so the next auditor
// doesn't go hunting for its salt.
const (
	githubAppRegRMWLockSalt int64 = 8

	// githubInstallationBindLockSalt namespaces the managed bind's
	// installation-identity lock. It is a SECOND keyspace rather than a second
	// key in the org one because the two answer different questions — "one
	// credential transition per workspace at a time" and "one workspace may
	// claim this installation" — and because an org id and a host+installation
	// string sharing a keyspace would be two unrelated domains colliding by
	// accident.
	//
	// Lock ORDER, where both are held: the org lock first, then this one. That
	// order is total because nothing takes this lock without already holding
	// the org lock, and nothing waits on an org lock while holding this one, so
	// no cycle can form.
	githubInstallationBindLockSalt int64 = 9
)

// lockLinearCredential takes orgID's Linear credential lock (see
// linearoauth.CredentialLock). Callers hold it across the reads their write
// depends on and the write itself, and take it before guardLocalLinearWrite,
// which relies on it.
func (s *Server) lockLinearCredential(ctx context.Context, orgID string) (release func(), err error) {
	return s.linearCredentialLock.Lock(ctx, orgID)
}
