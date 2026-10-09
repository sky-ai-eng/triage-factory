package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// AcquireKeyedLock serializes a critical section keyed on key (an org id, ...)
// across the whole deployment, not just this process.
//
// In multi mode (Postgres) it takes a session-scoped pg_advisory_lock on a
// dedicated connection checked out from database for the duration of the
// critical section, so two pods cannot interleave the same key's section. The
// lock is session- not transaction-scoped because a guarded section can span
// several independent transactions (and work between them); a
// pg_advisory_xact_lock would release at the first one's commit and stop
// covering the rest. Release runs the unlock and returns the connection to
// the pool on success; on unlock FAILURE it forces a physical close via
// conn.Raw + driver.ErrBadConn instead — (*sql.Conn).Close alone only pools
// the connection, and a pooled session that still holds the lock blocks
// every other acquirer of that key deployment-wide until the pool happens to
// evict it (up to ConnMaxLifetime). A failed unlock with a healthy session is
// real (e.g. a server-side statement_timeout cancel), so "unlock failed ⇒
// session is dying anyway" is not a safe assumption — same discipline as
// migrations.go's migration lock.
//
// salt namespaces the keyspace in Postgres's advisory-lock table, which every
// caller in the database shares; internal/server/advisorylock.go keeps the
// registry of salts in use.
//
// In local mode (SQLite has no advisory-lock primitive, and there is no second
// process to race at N=1) it takes the key's mutex in local, a
// map[key]*sync.Mutex, and database is unused.
//
// Returns a release func the caller must call at least once (typically via
// defer). It is idempotent in both modes, so a caller may release early to
// narrow the held window and keep the deferred call as the safety net.
func AcquireKeyedLock(ctx context.Context, database *sql.DB, local *sync.Map, salt int64, key string) (release func(), err error) {
	if runmode.Current() != runmode.ModeMulti {
		v, _ := local.LoadOrStore(key, &sync.Mutex{})
		m := v.(*sync.Mutex)
		m.Lock()
		var once sync.Once
		return func() { once.Do(m.Unlock) }, nil
	}

	conn, err := database.Conn(ctx)
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
