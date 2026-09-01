package server

import (
	"context"
	"database/sql/driver"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// acquireKeyedLock serializes a read-merge-write critical section keyed on
// key (an org id, ...) across the whole deployment, not just this process
// (TFAC-579).
//
// In multi mode (Postgres) it takes a session-scoped pg_advisory_lock on a
// dedicated connection checked out from s.db for the duration of the
// critical section — closing the gap githubAppRegMu can't: two control
// pods, each with their own in-process sync.Map, can freely interleave the
// same org's read-merge-write. The lock is
// session- not transaction-scoped because the guarded sections here span
// several independent s.tx.WithTx calls (and, for the knowledge-upload
// handler, file I/O in between) rather than one transaction — a
// pg_advisory_xact_lock would release at the first call's commit and stop
// covering the rest. Release runs the unlock and returns the connection to
// the pool on success; on unlock FAILURE it forces a physical close via
// conn.Raw + driver.ErrBadConn instead — (*sql.Conn).Close alone only
// pools the connection, and a pooled session that still holds the lock
// blocks every other acquirer of that key deployment-wide until the pool
// happens to evict it (up to ConnMaxLifetime). A failed unlock with a
// healthy session is real (e.g. a server-side statement_timeout cancel),
// so "unlock failed ⇒ session is dying anyway" is not a safe assumption —
// same discipline as db/migrations.go's migration lock.
//
// In local mode (SQLite has no advisory-lock primitive, and there's no
// second process to race at N=1 anyway) it falls back to the existing
// per-process keyed mutex — the same protection this RMW had before,
// unchanged.
//
// Returns a release func the caller must call at least once (typically via
// defer) to end the critical section — it's safe to call earlier and let a
// deferred call fire again as a no-op (e.g. to narrow the held window: release
// as soon as the guarded work is done, keep the defer as the safety net for
// every other early-return path), since the returned func is idempotent in
// both modes.
func (s *Server) acquireKeyedLock(ctx context.Context, mu *sync.Map, salt int64, key string) (release func(), err error) {
	if runmode.Current() != runmode.ModeMulti {
		v, _ := mu.LoadOrStore(key, &sync.Mutex{})
		m := v.(*sync.Mutex)
		m.Lock()
		var once sync.Once
		return func() { once.Do(m.Unlock) }, nil
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1, $2))`, key, salt); err != nil {
		_ = conn.Close()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if _, uerr := conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, $2))`, key, salt); uerr != nil {
				// See the doc comment: a pooled connection still holding
				// the lock wedges this key for every pod; force the
				// backend session closed so the lock dies with it.
				_ = conn.Raw(func(driverConn any) error { return driver.ErrBadConn })
			}
			_ = conn.Close()
		})
	}, nil
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
