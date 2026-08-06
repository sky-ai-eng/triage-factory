package db

import (
	"database/sql"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// PoolAdmin, PoolApp, and PoolLocal name the connection pools a TF process
// can hold, for the `db.pool` attribute on statement spans and pool-stats
// metrics. Multi mode opens two against the same server and they behave
// nothing alike — the admin pool serves JWT-less background jobs, the app
// pool serves RLS-bound request handlers — so saturation on one says
// something very different from saturation on the other, and without a
// discriminator the two series add together into a number that describes
// neither. Local mode has exactly one.
const (
	PoolAdmin = "admin"
	PoolApp   = "app"
	PoolLocal = "local"
)

// poolKey is the attribute distinguishing the pools above. Not a semconv
// key: nothing standard names "which of this process's pools", and
// db.namespace (the closest fit) is the database name, which is identical
// for both.
const poolKey = attribute.Key("db.pool")

// OpenTraced is sql.Open with OTel instrumentation installed at the
// *driver* level, and it is the only way a TF process should open a
// database handle.
//
// Driver-level is a requirement, not a preference. The alternative — a
// decorator around the small `queryer` interface the stores are written
// against — is the more obvious design and it breaks at runtime: several
// stores type-assert that interface back to a concrete *sql.DB or *sql.Tx
// (both dialects' transaction helpers do, among others), and a decorator
// fails those assertions in a way the compiler cannot see. Wrapping the
// driver instead leaves every type in the stack exactly what it was, and
// instruments all ~1,000 store query sites in both dialects without one
// call-site change.
//
// Returns an uninstrumented handle when tracing is off, in the only sense
// that matters: the global provider is a no-op, so the wrapped driver's
// per-call span is an interface call and a discarded struct.
func OpenTraced(driverName, dsn, pool string) (*sql.DB, error) {
	return otelsql.Open(driverName, dsn, tracedDriverOptions(driverName, pool)...)
}

// RegisterPoolMetrics publishes the handle's sql.DBStats as OTel gauges
// (open/idle/in-use connections, wait count, wait duration) tagged with
// which pool they describe. Pool exhaustion is otherwise invisible: a
// request blocked waiting for a connection looks exactly like a slow
// query, because the wait happens before any statement span starts.
//
// Best-effort by contract — the returned error is worth logging and never
// worth failing boot over, since the process runs fine without the gauges.
// The registration is deliberately not retained: these pools live for the
// process lifetime, so there is no point at which unregistering them would
// be the right thing to do.
func RegisterPoolMetrics(handle *sql.DB, driverName, pool string) error {
	_, err := otelsql.RegisterDBStatsMetrics(handle, tracedDriverOptions(driverName, pool)...)
	return err
}

// tracedDriverOptions is the one place the instrumentation is configured,
// shared by the span and metric paths so a pool's statement spans and its
// gauges can't end up labelled differently.
func tracedDriverOptions(driverName, pool string) []otelsql.Option {
	return []otelsql.Option{
		otelsql.WithAttributes(append(dbSystemAttrs(driverName), poolKey.String(pool))...),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			// Ping runs on every /readyz and on every pool health check.
			// A span per ping would out-number every other span in the
			// process and describe none of its work.
			Ping: false,
			// One event per row. A single list query would carry
			// hundreds; a full backfill, thousands. The query's own span
			// already covers the time they take to arrive.
			RowsNext: false,
			// driver.ErrSkip is how a driver says "I don't implement this
			// optional fast path, use the general one" — a routine
			// negotiation, not a failure. Recording it would paint
			// perfectly healthy spans with an error status and make
			// "traces with errors" useless as a filter.
			DisableErrSkip: true,
			// A connection reset is pool bookkeeping that happens between
			// two unrelated pieces of work, on whatever context the pool
			// hands it. It attaches to no caller's trace and tells no
			// caller anything.
			OmitConnResetSession: true,
		}),
	}
}

// dbSystemAttrs maps a registered driver name to its semconv
// db.system.name. Only the two drivers TF opens are named; an unrecognized
// one gets nothing rather than a guess, since a wrong db.system is worse
// than a missing one for anything reading these spans.
func dbSystemAttrs(driverName string) []attribute.KeyValue {
	switch driverName {
	case "pgx":
		return []attribute.KeyValue{semconv.DBSystemNamePostgreSQL}
	case "sqlite":
		return []attribute.KeyValue{semconv.DBSystemNameSQLite}
	}
	return nil
}
