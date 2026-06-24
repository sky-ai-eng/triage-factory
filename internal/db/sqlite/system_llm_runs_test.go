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
// generated, and that an empty metadata_json lands as SQL NULL. Mirrors
// the curator store round-trip test. TFAC-451.
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

func newSQLiteForSystemLLMTest(t *testing.T) *sql.DB {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
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
