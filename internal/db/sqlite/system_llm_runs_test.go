package sqlite_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestSystemLLMRunStore_SQLite_RoundTrip pins that Record inserts a row
// whose every field reads back intact, that an empty id is server-
// generated, and that an empty metadata_json lands as SQL NULL.
func TestSystemLLMRunStore_SQLite_RoundTrip(t *testing.T) {
	conn := newSQLiteForSystemLLMTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	started := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	completed := time.Date(2026, 6, 24, 12, 0, 5, 0, time.UTC)
	row := domain.SystemLLMRun{
		OrgID:               runmode.LocalDefaultOrgID,
		Job:                 "scorer",
		Model:               "haiku",
		TotalCostUSD:        0.0123,
		InputTokens:         11,
		OutputTokens:        22,
		CacheReadTokens:     333,
		CacheCreationTokens: 44,
		DurationMs:          1500,
		NumTurns:            1,
		IsError:             false,
		MetadataJSON:        `{"batch_size":10}`,
		StartedAt:           started,
		CompletedAt:         completed,
	}
	if err := stores.SystemLLMRuns.Record(ctx, row); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var (
		gotID, gotJob, gotModel     string
		gotCost                     float64
		in, out, cr, cc, dur, turns int
		isErr                       bool
		gotMeta                     sql.NullString
		gotStarted                  time.Time
	)
	err := conn.QueryRow(`
		SELECT id, job, model, total_cost_usd, input_tokens, output_tokens,
		       cache_read_tokens, cache_creation_tokens, duration_ms, num_turns,
		       is_error, metadata_json, started_at
		FROM system_llm_runs WHERE org_id = ?
	`, runmode.LocalDefaultOrgID).Scan(
		&gotID, &gotJob, &gotModel, &gotCost, &in, &out, &cr, &cc, &dur, &turns,
		&isErr, &gotMeta, &gotStarted,
	)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}

	if gotID == "" {
		t.Error("expected a server-generated id, got empty string")
	}
	if gotJob != "scorer" || gotModel != "haiku" {
		t.Errorf("job/model = %q/%q, want scorer/haiku", gotJob, gotModel)
	}
	if gotCost != 0.0123 {
		t.Errorf("total_cost_usd = %v, want 0.0123", gotCost)
	}
	if in != 11 || out != 22 || cr != 333 || cc != 44 {
		t.Errorf("tokens = (%d,%d,%d,%d), want (11,22,333,44)", in, out, cr, cc)
	}
	if dur != 1500 || turns != 1 || isErr {
		t.Errorf("dur/turns/err = %d/%d/%v, want 1500/1/false", dur, turns, isErr)
	}
	if !gotMeta.Valid || gotMeta.String != `{"batch_size":10}` {
		t.Errorf("metadata_json = %v, want {\"batch_size\":10}", gotMeta)
	}
	if !gotStarted.Equal(started) {
		t.Errorf("started_at = %v, want %v", gotStarted, started)
	}
}

// TestSystemLLMRunStore_SQLite_EmptyMetadataIsNull confirms an empty
// MetadataJSON serializes to SQL NULL (not the empty string).
func TestSystemLLMRunStore_SQLite_EmptyMetadataIsNull(t *testing.T) {
	conn := newSQLiteForSystemLLMTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	if err := stores.SystemLLMRuns.Record(ctx, domain.SystemLLMRun{
		OrgID:     runmode.LocalDefaultOrgID,
		Job:       "classifier",
		Model:     "haiku",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var meta sql.NullString
	if err := conn.QueryRow(`SELECT metadata_json FROM system_llm_runs`).Scan(&meta); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if meta.Valid {
		t.Errorf("metadata_json = %q, want NULL", meta.String)
	}
}

// TestSystemLLMRunStore_SQLite_HonorsProvidedID pins that a caller-supplied
// row.ID is preserved (parity with Postgres) rather than overwritten.
func TestSystemLLMRunStore_SQLite_HonorsProvidedID(t *testing.T) {
	conn := newSQLiteForSystemLLMTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	const id = "11111111-1111-1111-1111-111111111111"
	if err := stores.SystemLLMRuns.Record(ctx, domain.SystemLLMRun{
		ID:        id,
		OrgID:     runmode.LocalDefaultOrgID,
		Job:       "scorer",
		Model:     "haiku",
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var got string
	if err := conn.QueryRow(`SELECT id FROM system_llm_runs`).Scan(&got); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got != id {
		t.Errorf("id = %q, want %q (caller-supplied id must be honored)", got, id)
	}
}

// TestSystemLLMRunStore_SQLite_DuplicateTraceIDIsNoOp mirrors the Postgres
// pin on this dialect (TFAC-579): a second Record with the same trace_id
// must be a clean no-op — the ON CONFLICT (trace_id) WHERE trace_id IS NOT
// NULL clause fires, no error, no second row, no double-counted spend.
func TestSystemLLMRunStore_SQLite_DuplicateTraceIDIsNoOp(t *testing.T) {
	conn := newSQLiteForSystemLLMTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()
	const traceID = "session-dedup-sqlite"

	if err := stores.SystemLLMRuns.Record(ctx, domain.SystemLLMRun{
		OrgID: runmode.LocalDefaultOrgID, Job: "scorer", Model: "haiku",
		StartedAt: time.Now().UTC(), TraceID: traceID,
	}); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if err := stores.SystemLLMRuns.Record(ctx, domain.SystemLLMRun{
		OrgID: runmode.LocalDefaultOrgID, Job: "scorer", Model: "haiku",
		StartedAt: time.Now().UTC(), TraceID: traceID,
		TotalCostUSD: 999, // distinguishable if it wrongly inserted
	}); err != nil {
		t.Fatalf("duplicate TraceID Record should be a no-op, not an error: %v", err)
	}

	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM system_llm_runs WHERE trace_id = ?`, traceID).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row for trace_id %q, got %d (duplicate double-counted spend)", traceID, count)
	}
	var cost float64
	if err := conn.QueryRow(`SELECT total_cost_usd FROM system_llm_runs WHERE trace_id = ?`, traceID).Scan(&cost); err != nil {
		t.Fatalf("read cost: %v", err)
	}
	if cost == 999 {
		t.Error("second Record's row landed (cost=999) — ON CONFLICT DO NOTHING didn't fire")
	}
}

// TestSystemLLMRunStore_SQLite_EmptyTraceIDNeverCollides mirrors the
// Postgres pin: an empty TraceID binds NULL (nullIfEmpty), the partial
// unique index excludes NULL, so two trace-less rows must both land.
func TestSystemLLMRunStore_SQLite_EmptyTraceIDNeverCollides(t *testing.T) {
	conn := newSQLiteForSystemLLMTest(t)
	stores := sqlitestore.New(conn)
	ctx := context.Background()

	for range 2 {
		if err := stores.SystemLLMRuns.Record(ctx, domain.SystemLLMRun{
			OrgID: runmode.LocalDefaultOrgID, Job: "scorer", Model: "haiku",
			StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	var count int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM system_llm_runs WHERE trace_id IS NULL`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("expected both NULL-trace_id rows to land, got %d", count)
	}
}

func newSQLiteForSystemLLMTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", db.TestDSNMemory)
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return conn
}
