package db

import (
	"database/sql"

	"github.com/XSAM/otelsql"
	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
)

// PoolAdmin, PoolApp, and PoolLocal name the connection pools a TF process
// can hold. Multi mode's two behave nothing alike — admin serves JWT-less
// background jobs, app serves RLS-bound handlers — so without a
// discriminator their saturation series add into a number describing
// neither. Local mode has exactly one.
const (
	PoolAdmin = "admin"
	PoolApp   = "app"
	PoolLocal = "local"
)

// poolKey distinguishes the pools above. Not a semconv key: nothing
// standard names "which of this process's pools", and db.namespace (the
// closest fit) is the database name, identical for both.
const poolKey = attribute.Key("db.pool")

// OpenTraced is sql.Open with OTel instrumentation at the *driver* level,
// and it is the only way a TF process should open a database handle.
//
// Driver-level is a requirement, not a preference. The obvious alternative
// — a decorator around the small `queryer` interface the stores are written
// against — breaks at runtime: several stores type-assert that interface
// back to a concrete *sql.DB or *sql.Tx, and a decorator fails those
// assertions in a way the compiler cannot see. Wrapping the driver leaves
// every type in the stack what it was and instruments all ~1,000 store
// query sites in both dialects with no call-site change.
func OpenTraced(driverName, dsn, pool string) (*sql.DB, error) {
	return otelsql.Open(driverName, dsn, tracedDriverOptions(driverName, pool)...)
}

// RegisterPoolMetrics publishes the handle's sql.DBStats as OTel gauges
// tagged with which pool they describe. Pool exhaustion is otherwise
// invisible: a request blocked waiting for a connection looks exactly like
// a slow query, because the wait happens before any statement span starts.
//
// Best-effort by contract — the error is worth logging, never worth failing
// boot over. The registration isn't retained: these pools live for the
// process lifetime, so there is no point at which unregistering them would
// be right.
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
		// The four below are all "this would drown out the real spans, or
		// mislead": a span per /readyz ping, an event per result row, an
		// error status for driver.ErrSkip (a routine "I don't implement
		// that fast path" negotiation), and pool bookkeeping that attaches
		// to no caller's trace.
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			Ping:                 false,
			RowsNext:             false,
			DisableErrSkip:       true,
			OmitConnResetSession: true,
		}),
	}
}

// dbSystemAttrs maps a driver name to its semconv db.system.name. An
// unrecognized driver gets nothing rather than a guess — a wrong db.system
// is worse than a missing one.
func dbSystemAttrs(driverName string) []attribute.KeyValue {
	switch driverName {
	case "pgx":
		return []attribute.KeyValue{semconv.DBSystemNamePostgreSQL}
	case "sqlite":
		return []attribute.KeyValue{semconv.DBSystemNameSQLite}
	}
	return nil
}
