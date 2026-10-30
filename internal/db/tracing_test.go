package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// TestOpenTracedNestsStatementSpansUnderCaller is the load-bearing test
// for the driver-level wrap: a query that knows nothing about tracing must
// land as a child of whatever span the caller had open. That nesting is
// the whole reason the instrumentation sits at the driver, and the type
// system can't check it.
func TestOpenTracedNestsStatementSpansUnderCaller(t *testing.T) {
	recorder, handle := openTracedForTest(t)

	ctx, parent := otel.Tracer("db/test").Start(context.Background(), "caller")
	if _, err := handle.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("exec: %v", err)
	}
	parent.End()

	var stmt sdktrace.ReadOnlySpan
	for _, s := range recorder.Ended() {
		if s.SpanContext().SpanID() == parent.SpanContext().SpanID() {
			continue
		}
		if s.Parent().SpanID() == parent.SpanContext().SpanID() {
			stmt = s
			break
		}
	}
	if stmt == nil {
		t.Fatal("no statement span parented to the caller's span; the driver wrap is not reaching store queries")
	}

	want := map[attribute.Key]string{
		"db.system.name": "sqlite",
		"db.pool":        PoolLocal,
	}
	got := map[attribute.Key]string{}
	for _, kv := range stmt.Attributes() {
		got[kv.Key] = kv.Value.AsString()
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("statement span %s = %q, want %q", k, got[k], v)
		}
	}
}

// TestOpenTracedSuppressesPingSpans covers the option that keeps /readyz
// from drowning the backend.
func TestOpenTracedSuppressesPingSpans(t *testing.T) {
	recorder, handle := openTracedForTest(t)

	ctx, parent := otel.Tracer("db/test").Start(context.Background(), "caller")
	if err := handle.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	parent.End()

	for _, s := range recorder.Ended() {
		if s.Name() == "sql.conn.ping" {
			t.Error("a ping produced a span; /readyz pings would out-number every other span in the process")
		}
	}
}

// TestOpenTracedRecordsNothingOutsideATrace covers the rule that a
// statement span exists only inside a trace something else opened. Every
// scan-loop tick runs on a ctx with no span, and without the filter each
// of its statements exports as a one-span trace of its own.
func TestOpenTracedRecordsNothingOutsideATrace(t *testing.T) {
	recorder, handle := openTracedForTest(t)

	exerciseTracedHandle(t, context.Background(), handle)

	if started := recorder.Started(); len(started) != 0 {
		var names []string
		for _, s := range started {
			names = append(names, s.Name())
		}
		t.Fatalf("parentless statements started %d spans %v, want 0; each would export as its own root trace", len(started), names)
	}
}

// TestOpenTracedRecordsEveryStatementInsideATrace is the other half of
// the filter: the same calls under a caller's span keep every span they
// had, parented to it. The commit is the case worth naming — otelsql
// tests it against the ctx BeginTx was given, so a filter that read the
// wrong ctx would drop it silently.
func TestOpenTracedRecordsEveryStatementInsideATrace(t *testing.T) {
	recorder, handle := openTracedForTest(t)

	ctx, parent := otel.Tracer("db/test").Start(context.Background(), "caller")
	exerciseTracedHandle(t, ctx, handle)
	parent.End()

	parented := map[string]bool{}
	for _, s := range recorder.Ended() {
		if s.Parent().SpanID() == parent.SpanContext().SpanID() {
			parented[s.Name()] = true
		}
	}
	for _, want := range []string{
		"sql.conn.exec",
		"sql.conn.query",
		"sql.rows",
		"sql.conn.begin_tx",
		"sql.tx.commit",
	} {
		if !parented[want] {
			t.Errorf("no %s span parented to the caller's span", want)
		}
	}
}

func openTracedForTest(t *testing.T) (*tracetest.SpanRecorder, *sql.DB) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	handle, err := OpenTraced("sqlite", filepath.Join(t.TempDir(), "trace.db"), PoolLocal)
	if err != nil {
		t.Fatalf("OpenTraced: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	return recorder, handle
}

// exerciseTracedHandle runs one of each statement shape a store issues: a
// bare exec, a query whose rows are iterated, and a committed transaction.
func exerciseTracedHandle(t *testing.T, ctx context.Context, handle *sql.DB) {
	t.Helper()
	if _, err := handle.ExecContext(ctx, `CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("exec: %v", err)
	}

	rows, err := handle.QueryContext(ctx, `SELECT id FROM t`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	rows.Close()

	tx, err := handle.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("tx exec: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
