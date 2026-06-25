package agentmeta

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"

	_ "modernc.org/sqlite"
)

// newTestDB spins up an in-memory SQLite with the full schema +
// events catalog seeded. Mirror of the helper in internal/db's own
// test files; redeclared here because newTestDB is unexported there.
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

// seedBlueprintRun mints a fresh blueprint + blueprint_run for taskID
// and returns its id. runs.blueprint_run_id is NOT NULL, so every
// seeded run needs a parent blueprint_run. SQLite blueprint_runs has no
// org_id/creator_user_id columns; org_id on blueprints takes its
// local-sentinel DEFAULT.
func seedBlueprintRun(t *testing.T, database *sql.DB, taskID string) string {
	t.Helper()
	bpID := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO blueprints (id, name, source, team_id, creator_user_id)
		VALUES (?, 'Test BP', 'user', ?, ?)
	`, bpID, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("seed blueprint: %v", err)
	}
	brID := uuid.New().String()
	if _, err := database.Exec(`
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, status, worktree_path, step_plan)
		VALUES (?, ?, ?, 'manual', 'running', '/tmp/wt', '[]')
	`, brID, bpID, taskID); err != nil {
		t.Fatalf("seed blueprint_run: %v", err)
	}
	return brID
}

// seedFooterRun installs the entity → event → task → prompt → run
// chain required for db.GetAgentRun to find a row, then patches the
// runs columns the footer actually reads (model, started_at,
// completed_at, duration_ms, total_cost_usd) via a direct UPDATE.
// Returns nothing — the test asserts via db.GetAgentRun and Build.
func seedFooterRun(t *testing.T, database *sql.DB, fix runFooterFixture) {
	t.Helper()
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#"+fix.ID, "pr", "T", "https://x/"+fix.ID)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	evt, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventGitHubPRCICheckFailed,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := sqlitestore.New(database).Tasks.FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID, domain.EventGitHubPRCICheckFailed, fix.ID, evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	prompts := sqlitestore.New(database).Prompts
	ctx := t.Context()
	if existing, _ := prompts.Get(ctx, runmode.LocalDefaultOrgID, "footer-test-prompt"); existing == nil {
		if err := prompts.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Prompt{ID: "footer-test-prompt", Name: "T", Body: "x", Source: "user"}); err != nil {
			t.Fatalf("prompt: %v", err)
		}
	}
	brID := fix.BlueprintRunID
	if brID == "" {
		brID = seedBlueprintRun(t, database, task.ID)
	}
	if err := sqlitestore.New(database).AgentRuns.Create(t.Context(), runmode.LocalDefaultOrgID, domain.AgentRun{
		ID: fix.ID, TaskID: task.ID, PromptID: "footer-test-prompt",
		Status: "running", Model: fix.Model, StartedAt: fix.StartedAt,
		BlueprintRunID: brID,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	// Patch the columns the footer reads. CreateAgentRun doesn't
	// expose total_cost_usd / duration_ms / completed_at, so we go
	// direct.
	if _, err := database.Exec(
		`UPDATE runs
		    SET completed_at = ?,
		        duration_ms = ?,
		        total_cost_usd = ?,
		        started_at = ?
		  WHERE id = ?`,
		fix.CompletedAt, fix.DurationMs, fix.TotalCostUSD, fix.StartedAt, fix.ID,
	); err != nil {
		t.Fatalf("patch run columns: %v", err)
	}
}

type runFooterFixture struct {
	ID           string
	Model        string
	StartedAt    time.Time
	CompletedAt  *time.Time
	DurationMs   *int
	TotalCostUSD *float64
	// BlueprintRunID, when set, attaches the run to an existing
	// blueprint_run instead of minting a fresh one — so two fixtures can
	// be seeded as sibling steps of the same blueprint for the
	// cost-aggregation test. Empty mints a new single-step blueprint_run.
	BlueprintRunID string
}

// TestBuild_NoRunID returns an empty string. A human running the CLI
// directly with no delegation context gets their body back
// unmodified rather than a "This was partially generated by AI"
// disclaimer that would be inaccurate.
func TestBuild_NoRunID(t *testing.T) {
	got := Build(nil, runmode.LocalDefaultOrgID, "", "review")
	if got != "" {
		t.Errorf("Build with empty runID = %q, want empty string (no AI disclosure when no run)", got)
	}
}

// TestBuild_KindNounRendersInDisclaimer pins the noun parameterization
// so a future "issue" or "comment" surface gets its own phrasing.
// Requires a real run because the disclaimer is suppressed for
// no-runID; we use the legacy-fallback fixture (TotalCostUSD nil) so
// the body of the test isn't sensitive to the metrics.
func TestBuild_KindNounRendersInDisclaimer(t *testing.T) {
	database := newTestDB(t)
	for i, kind := range []string{"review", "PR", "issue"} {
		runID := fmt.Sprintf("run-kind-%d", i)
		seedFooterRun(t, database, runFooterFixture{
			ID:        runID,
			Model:     "claude-haiku-4-5",
			StartedAt: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		})
		got := Build(sqlitestore.New(database).AgentRuns, runmode.LocalDefaultOrgID, runID, kind)
		want := "This " + kind + " was partially generated"
		if !strings.Contains(got, want) {
			t.Errorf("kind=%q: missing %q in %q", kind, want, got)
		}
	}
}

// TestBuild_HappyPath_UsesStoredCostAndDuration covers the canonical
// post-run case: TotalCostUSD and DurationMs were populated by
// CompleteAgentRun. The footer reads them directly with no "~" prefix.
func TestBuild_HappyPath_UsesStoredCostAndDuration(t *testing.T) {
	database := newTestDB(t)
	completedAt := time.Date(2026, 1, 1, 12, 1, 30, 0, time.UTC)
	durationMs := 90_000
	cost := 0.0123
	seedFooterRun(t, database, runFooterFixture{
		ID:           "r1",
		Model:        "claude-sonnet-4-6",
		StartedAt:    time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		CompletedAt:  &completedAt,
		DurationMs:   &durationMs,
		TotalCostUSD: &cost,
	})

	got := Build(sqlitestore.New(database).AgentRuns, runmode.LocalDefaultOrgID, "r1", "review")
	if !strings.Contains(got, "Time: 1m 30s") {
		t.Errorf("missing/wrong Time: %q", got)
	}
	if strings.Contains(got, "Model:") {
		t.Errorf("footer should no longer disclose a model name: %q", got)
	}
	if !strings.Contains(got, "Cost: $0.012") {
		t.Errorf("missing/wrong Cost (expected settled, no '~'): %q", got)
	}
	if strings.Contains(got, "Cost: ~$") {
		t.Errorf("settled-cost run should NOT show '~' prefix: %q", got)
	}
}

// TestBuild_LegacyFallback_FlagsApproximateCost covers the
// still-running CLI path: TotalCostUSD is nil. Footer falls back to
// RunTokenTotals (which returns zeros if there are no agent_messages
// rows) and prefixes Cost with "~" to flag the estimate.
func TestBuild_LegacyFallback_FlagsApproximateCost(t *testing.T) {
	database := newTestDB(t)
	durationMs := 5_000
	seedFooterRun(t, database, runFooterFixture{
		ID:         "r2",
		Model:      "claude-haiku-4-5",
		StartedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		DurationMs: &durationMs,
		// TotalCostUSD nil → forces the legacy path
	})

	got := Build(sqlitestore.New(database).AgentRuns, runmode.LocalDefaultOrgID, "r2", "review")
	if !strings.Contains(got, "Cost: ~$") {
		t.Errorf("legacy fallback should prefix Cost with '~': %q", got)
	}
	if strings.Contains(got, "Model:") {
		t.Errorf("footer should no longer disclose a model name: %q", got)
	}
}

// TestBuild_SumsCostAcrossBlueprintSteps covers the multi-step case:
// the footer for the step that authored the published review/PR sums
// the cost of every step in the blueprint, not just its own. Two
// sibling runs share one blueprint_run; the authoring run's footer
// reports their combined cost.
func TestBuild_SumsCostAcrossBlueprintSteps(t *testing.T) {
	database := newTestDB(t)

	// Seed step 1 to mint the shared blueprint_run, then read its id back
	// so step 2 (the authoring step) attaches to the same run.
	step1Cost := 0.01
	seedFooterRun(t, database, runFooterFixture{
		ID:           "bp-step-1",
		Model:        "claude-haiku-4-5",
		StartedAt:    time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		TotalCostUSD: &step1Cost,
	})
	var brID string
	if err := database.QueryRow(`SELECT blueprint_run_id FROM runs WHERE id = ?`, "bp-step-1").Scan(&brID); err != nil {
		t.Fatalf("read blueprint_run_id: %v", err)
	}

	completedAt := time.Date(2026, 1, 1, 12, 1, 30, 0, time.UTC)
	durationMs := 90_000
	step2Cost := 0.02
	seedFooterRun(t, database, runFooterFixture{
		ID:             "bp-step-2",
		Model:          "claude-opus-4-8",
		StartedAt:      time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC),
		CompletedAt:    &completedAt,
		DurationMs:     &durationMs,
		TotalCostUSD:   &step2Cost,
		BlueprintRunID: brID,
	})

	got := Build(sqlitestore.New(database).AgentRuns, runmode.LocalDefaultOrgID, "bp-step-2", "review")
	// 0.01 (step 1) + 0.02 (authoring step 2) = 0.030.
	if !strings.Contains(got, "Cost: $0.030") {
		t.Errorf("expected summed cost across blueprint steps (Cost: $0.030): %q", got)
	}
	if strings.Contains(got, "Cost: ~$") {
		t.Errorf("all steps settled — should not show '~' prefix: %q", got)
	}
	if strings.Contains(got, "Model:") {
		t.Errorf("footer should no longer disclose a model name: %q", got)
	}
}

// TestBuild_SumsTimeAcrossBlueprintSteps is the time analog of
// TestBuild_SumsCostAcrossBlueprintSteps: the footer for the authoring
// step reports the total time across every step of the blueprint, not
// just its own. Two sibling runs share one blueprint_run; the authoring
// run's footer reports their combined duration.
func TestBuild_SumsTimeAcrossBlueprintSteps(t *testing.T) {
	database := newTestDB(t)

	// Step 1 mints the shared blueprint_run; read its id back so step 2
	// (the authoring step) attaches to the same run.
	step1Dur := 30_000 // 30s
	seedFooterRun(t, database, runFooterFixture{
		ID:         "bpt-step-1",
		Model:      "claude-haiku-4-5",
		StartedAt:  time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		DurationMs: &step1Dur,
	})
	var brID string
	if err := database.QueryRow(`SELECT blueprint_run_id FROM runs WHERE id = ?`, "bpt-step-1").Scan(&brID); err != nil {
		t.Fatalf("read blueprint_run_id: %v", err)
	}

	step2Dur := 90_000 // 1m 30s
	seedFooterRun(t, database, runFooterFixture{
		ID:             "bpt-step-2",
		Model:          "claude-opus-4-8",
		StartedAt:      time.Date(2026, 1, 1, 12, 0, 30, 0, time.UTC),
		DurationMs:     &step2Dur,
		BlueprintRunID: brID,
	})

	got := Build(sqlitestore.New(database).AgentRuns, runmode.LocalDefaultOrgID, "bpt-step-2", "review")
	// 30s (step 1) + 1m 30s (authoring step 2) = 2m 0s.
	if !strings.Contains(got, "Time: 2m 0s") {
		t.Errorf("expected summed time across blueprint steps (Time: 2m 0s): %q", got)
	}
}

// TestPrettyDurationMs covers the format breakpoints (s / m s / h m).
func TestPrettyDurationMs(t *testing.T) {
	cases := map[int]string{
		500:       "0s",
		59_000:    "59s",
		60_000:    "1m 0s",
		90_000:    "1m 30s",
		3_600_000: "1h 0m",
		7_530_000: "2h 5m",
	}
	for ms, want := range cases {
		if got := prettyDurationMs(ms); got != want {
			t.Errorf("prettyDurationMs(%d) = %q, want %q", ms, got, want)
		}
	}
}

// footerFor builds a footer the same shape Build emits, for the StripFooter
// tests (no DB needed — only the separator + disclaimer marker matter).
func footerFor(kind, metrics string) string {
	return fmt.Sprintf("\n\n---\n*This %s was partially generated by AI using [Triage Factory](https://github.com/sky-ai-eng/triage-factory).*%s", kind, metrics)
}

func TestStripFooter(t *testing.T) {
	body := "Real PR body\n\nwith paragraphs"

	// No footer → unchanged.
	if got := StripFooter(body); got != body {
		t.Errorf("StripFooter(no footer) = %q, want unchanged", got)
	}

	// Single footer → stripped back to the body.
	if got := StripFooter(body + footerFor("PR", "\n\nTime: 1s | Cost: $0.001")); got != body {
		t.Errorf("StripFooter(one footer) = %q, want %q", got, body)
	}

	// Kind-independent: a review footer is recognized too.
	if got := StripFooter(body + footerFor("review", "")); got != body {
		t.Errorf("StripFooter(review footer) = %q, want %q", got, body)
	}

	// Already-doubled (a pre-fix stack) → all footers removed, so a fresh append
	// leaves exactly one.
	doubled := body + footerFor("PR", "") + footerFor("PR", "")
	if got := StripFooter(doubled); got != body {
		t.Errorf("StripFooter(doubled) = %q, want %q", got, body)
	}

	// Idempotency property: strip-then-append yields exactly one footer.
	once := StripFooter(body+footerFor("PR", "")) + footerFor("PR", "")
	if n := strings.Count(once, "partially generated by AI using [Triage Factory]"); n != 1 {
		t.Errorf("strip-then-append left %d footers, want 1", n)
	}

	// A literal "---" rule in user content (no disclaimer) is not mistaken for a footer.
	withRule := "intro\n\n---\nmore content, no footer here"
	if got := StripFooter(withRule); got != withRule {
		t.Errorf("StripFooter(plain --- rule) = %q, want unchanged", got)
	}
}
