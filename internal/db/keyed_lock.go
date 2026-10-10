package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// unlockTimeout bounds the advisory unlock. A stalled database would otherwise
// hold the release, and the key's gate with it, so every caller queued on the
// key would wait on the stall; past it the session is discarded, which ends
// the lock just as surely.
const unlockTimeout = 5 * time.Second

// AcquireKeyedLock serializes a critical section keyed on key (an org id, ...)
// across the whole deployment, not just this process.
//
// Every caller first takes the key's gate in local, a map[key]chan struct{}
// holding one slot per key, so the callers in one process queue there. In
// local mode (SQLite has no advisory-lock primitive, and there is no second
// process to race at N=1) that gate is the whole lock and database is unused.
//
// In multi mode (Postgres) the gate's holder then takes a session-scoped
// pg_advisory_lock on a dedicated connection checked out from database, so
// two pods cannot interleave the same key's section either. The gate comes
// first so that a process has at most one connection per key blocked on the
// advisory lock: otherwise a queue of waiters on one key could hold every
// connection in the pool while the holder they wait on needs one more for
// its own work, and nothing would move until a ctx expired. The lock is
// session- not transaction-scoped because a guarded section can span several
// independent transactions (and work between them); a pg_advisory_xact_lock
// would release at the first one's commit and stop covering the rest.
//
// A connection whose lock or unlock call fails, or whose unlock outlasts
// unlockTimeout, is discarded rather than returned to the pool — forced closed via conn.Raw + driver.ErrBadConn,
// since (*sql.Conn).Close alone only pools it. A cancelled lock call may have
// been granted by the server before the cancellation reached it, and an
// unlock can fail on a healthy session (a server-side statement_timeout
// cancel, say); either way a pooled session that still holds the lock would
// block every other acquirer of that key deployment-wide until the pool
// happened to evict it (up to ConnMaxLifetime). Same discipline as
// migrations.go's migration lock.
//
// salt namespaces the keyspace in Postgres's advisory-lock table, which every
// caller in the database shares; internal/server/advisorylock.go keeps the
// registry of salts in use.
//
// Waiting honors ctx in both modes. Returns a release func the caller must
// call at least once (typically via defer). It is idempotent, so a caller may
// release early to narrow the held window and keep the deferred call as the
// safety net.
func AcquireKeyedLock(ctx context.Context, database *sql.DB, local *sync.Map, salt int64, key string) (release func(), err error) {
	v, _ := local.LoadOrStore(key, make(chan struct{}, 1))
	gate := v.(chan struct{})
	select {
	case gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	openGate := func() { <-gate }

	if runmode.Current() != runmode.ModeMulti {
		var once sync.Once
		return func() { once.Do(openGate) }, nil
	}

	conn, err := database.Conn(ctx)
	if err != nil {
		openGate()
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtextextended($1, $2))`, key, salt); err != nil {
		discardConn(conn)
		openGate()
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			uctx, cancel := context.WithTimeout(context.Background(), unlockTimeout)
			defer cancel()
			if _, uerr := conn.ExecContext(uctx, `SELECT pg_advisory_unlock(hashtextextended($1, $2))`, key, salt); uerr != nil {
				discardConn(conn)
			} else {
				_ = conn.Close()
			}
			openGate()
		})
	}, nil
}

// discardConn closes conn's backend session instead of returning it to the
// pool, so any advisory lock the session holds dies with it.
func discardConn(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
