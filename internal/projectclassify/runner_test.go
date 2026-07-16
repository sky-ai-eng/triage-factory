package projectclassify

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/systemllm"

	_ "modernc.org/sqlite"
)

// TestRunner_ProviderBackoff_LogsQuietly pins the systemllm circuit
// breaker's caller-side behavior (runner.go's allProviderBackoff check): a
// cycle where every vote fails with a breaker skip still leaves the entity
// unclassified for retry exactly like any other fully-failed cycle, but it
// must log at Info, never at the Warn level a genuine failure gets — a
// boot-time overload shouldn't read as a wall of alarms.
func TestRunner_ProviderBackoff_LogsQuietly(t *testing.T) {
	isolateHome(t)
	database := newTestDB(t)

	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p1", Name: "P1"}); err != nil {
		t.Fatal(err)
	}
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "T", "https://x/1")
	if err != nil {
		t.Fatal(err)
	}

	logs := &captureHandler{}
	prev := classifyLog
	classifyLog = slog.New(logs)
	t.Cleanup(func() { classifyLog = prev })

	r := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, runmode.LocalDefaultOrgID, nil, nil, nil, nil)
	// Force every Stage 1 vote to fail with a breaker skip (simulates the
	// upstream provider being in cooldown).
	r.stage1Fn = func(context.Context, string, haikuPrompt) (int, string, error) {
		return 0, "", &systemllm.ErrProviderBackoff{Provider: "anthropic-direct:default"}
	}
	r.run(context.Background()) // synchronous one cycle

	post, err := sqlitestore.New(database).Entities.ListUnclassified(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListUnclassifiedEntities: %v", err)
	}
	found := false
	for _, e := range post {
		if e.ID == entity.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("entity should still be unclassified after a fully-backed-off cycle; classified_at must remain NULL for retry")
	}

	const wantMsg = "all votes deferred; provider backing off, retrying next cycle"
	var sawWantMsg bool
	for _, rec := range logs.recorded() {
		if rec.Level >= slog.LevelWarn {
			t.Errorf("unexpected Warn/Error record for a provider-backoff skip: %v %q", rec.Level, rec.Message)
		}
		if rec.Message == wantMsg {
			sawWantMsg = true
			if rec.Level != slog.LevelInfo {
				t.Errorf("log level for %q = %v, want Info", wantMsg, rec.Level)
			}
		}
	}
	if !sawWantMsg {
		t.Errorf("expected a log record with message %q", wantMsg)
	}
}

// captureHandler is a minimal slog.Handler that records emitted entries so a
// test can assert exactly what — and how often — a code path logged. Mirrors
// internal/github/client_test.go's helper of the same name/shape.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) recorded() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]slog.Record(nil), h.records...)
}

// newTestDB opens an in-memory SQLite DB with the test schema bootstrapped.
// Shared by the runner tests that exercise the real per-org cycle against the
// store (the WaitFor tests use an in-memory fake instead).
func newTestDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })
	if err := db.BootstrapSchemaForTest(database); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return database
}

// TestRunner_StopIdempotent pins that Stop is safe to call more than once
// (guarded by stopOnce); a bare close(r.stop) would panic on the second call.
func TestRunner_StopIdempotent(t *testing.T) {
	r := NewRunner(nil, nil, runmode.LocalDefaultOrgID, nil, nil, nil, nil)
	r.Start()
	r.Stop()
	r.Stop() // must not panic
}

// TestRunner_AllErroredLeavesEntityForRetry guards against the bug
// where a fully-failed classification cycle (e.g., claude CLI
// unavailable) would still stamp classified_at, permanently freezing
// the entity at unassigned even after the failure clears. The runner
// should leave classified_at NULL when every per-project vote
// errored, so the entity resurfaces on the next trigger.
func TestRunner_AllErroredLeavesEntityForRetry(t *testing.T) {
	isolateHome(t)
	database := newTestDB(t)

	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p1", Name: "P1"}); err != nil {
		t.Fatal(err)
	}
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "T", "https://x/1")
	if err != nil {
		t.Fatal(err)
	}

	r := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, runmode.LocalDefaultOrgID, nil, nil, nil, nil)
	// Force every Stage 1 vote to error (simulates claude CLI down).
	r.stage1Fn = func(context.Context, string, haikuPrompt) (int, string, error) {
		return 0, "", errors.New("simulated CLI down")
	}
	r.run(context.Background()) // synchronous one cycle

	post, err := sqlitestore.New(database).Entities.ListUnclassified(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListUnclassifiedEntities: %v", err)
	}
	found := false
	for _, e := range post {
		if e.ID == entity.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("entity should still be unclassified after fully-failed cycle; classified_at must remain NULL for retry")
	}
}

// TestRunner_PartialErrorStillStamps verifies that a cycle where some
// votes succeed and some error DOES stamp classified_at — partial
// information is a real classification result, just with a known
// gap, and we don't want to keep retrying for the broken project.
func TestRunner_PartialErrorStillStamps(t *testing.T) {
	isolateHome(t)
	database := newTestDB(t)

	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p-good", Name: "Good"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p-flaky", Name: "Flaky"}); err != nil {
		t.Fatal(err)
	}
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#2", "pr", "T", "https://x/2")
	if err != nil {
		t.Fatal(err)
	}

	r := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, runmode.LocalDefaultOrgID, nil, nil, nil, nil)
	r.stage1Fn = func(_ context.Context, _ string, p haikuPrompt) (int, string, error) {
		if strings.Contains(p.Message, "<project_name>\nFlaky\n</project_name>") {
			return 0, "", errors.New("simulated CLI failure for Flaky")
		}
		return 30, "stub for Good", nil
	}
	r.run(context.Background())

	post, err := sqlitestore.New(database).Entities.ListUnclassified(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range post {
		if e.ID == entity.ID {
			t.Errorf("entity should be classified (partial-error path stamps classified_at)")
		}
	}
}

// TestRunner_ExactTieRationaleDoesNotQuoteOneCandidate guards against a
// misleading rationale: on an exact tie, bestRationale would otherwise pick
// one arbitrary tied vote's confident-sounding text and persist it verbatim,
// even though that project was NOT assigned — a human reading the rationale
// on an unassigned entity should see that it was tied, not a cherry-picked
// quote implying a near-miss on one specific project.
func TestRunner_ExactTieRationaleDoesNotQuoteOneCandidate(t *testing.T) {
	isolateHome(t)
	database := newTestDB(t)

	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p-alpha", Name: "Alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p-beta", Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#3", "pr", "T", "https://x/3")
	if err != nil {
		t.Fatal(err)
	}

	r := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, runmode.LocalDefaultOrgID, nil, nil, nil, nil)
	r.stage1Fn = func(_ context.Context, _ string, p haikuPrompt) (int, string, error) {
		if strings.Contains(p.Message, "<project_name>\nAlpha\n</project_name>") {
			return 75, "definitely belongs to Alpha", nil
		}
		return 75, "definitely belongs to Beta", nil
	}
	r.run(context.Background())

	got, err := sqlitestore.New(database).Entities.GetSystem(context.Background(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProjectID != nil {
		t.Errorf("entity should be unassigned on an exact tie, got project %v", *got.ProjectID)
	}
	if strings.Contains(got.ClassificationRationale, "definitely belongs to") {
		t.Errorf("rationale quotes one tied candidate's vote as if it won: %q", got.ClassificationRationale)
	}
	if !strings.Contains(got.ClassificationRationale, "tied") {
		t.Errorf("rationale should explain the tie, got: %q", got.ClassificationRationale)
	}
}
