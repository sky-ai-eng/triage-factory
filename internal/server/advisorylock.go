package server

import (
	"context"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// acquireKeyedLock serializes a read-merge-write critical section keyed on
// key (a project id, an org id, ...) across the whole deployment, not just
// this process (TFAC-579).
//
// In multi mode (Postgres) it takes a session-scoped pg_advisory_lock on a
// dedicated connection checked out from s.db for the duration of the
// critical section — closing the gap projectMutexes/githubAppRegMu can't:
// two control pods, each with their own in-process sync.Map, can freely
// interleave the same project's or org's read-merge-write. The lock is
// session- not transaction-scoped because the guarded sections here span
// several independent s.tx.WithTx calls (and, for the knowledge-upload
// handler, file I/O in between) rather than one transaction — a
// pg_advisory_xact_lock would release at the first call's commit and stop
// covering the rest. Unlock best-effort precedes the connection Close so
// the pool gets a clean connection back; if the unlock itself fails the
// connection is already in a bad enough state that Postgres will drop the
// session (and every lock it held) once the backend disconnects, so the
// lock is never leaked past that.
//
// In local mode (SQLite has no advisory-lock primitive, and there's no
// second process to race at N=1 anyway) it falls back to the existing
// per-process keyed mutex — the same protection this RMW had before,
// unchanged.
//
// Returns a release func the caller must call exactly once (typically via
// defer) to end the critical section.
func (s *Server) acquireKeyedLock(ctx context.Context, mu *sync.Map, salt int64, key string) (release func(), err error) {
	if runmode.Current() != runmode.ModeMulti {
		v, _ := mu.LoadOrStore(key, &sync.Mutex{})
		m := v.(*sync.Mutex)
		m.Lock()
		return m.Unlock, nil
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
			_, _ = conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock(hashtextextended($1, $2))`, key, salt)
			_ = conn.Close()
		})
	}, nil
}

// Advisory-lock salts (TFAC-579). hashtextextended's salt is a global
// namespace shared by every pg_advisory_lock/pg_advisory_xact_lock caller in
// the database, not just this package — each guarded RMW domain across the
// whole codebase needs its own value so two unrelated keyspaces (a project
// id here, an entity id in internal/db/postgres/tasks.go, an org id in
// team_github_repos.go/auth_provision.go/ee/slack) can't accidentally
// collide. 7 and 8 are unclaimed as of this writing (0-2 are taken by
// auth_provision.go/team_github_repos.go/ee/slack, 5 by
// internal/db/postgres/tasks.go's entityTaskCreationLockSalt).
const (
	projectRMWLockSalt      int64 = 7
	githubAppRegRMWLockSalt int64 = 8
)
