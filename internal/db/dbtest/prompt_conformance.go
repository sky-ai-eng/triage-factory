package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PromptStoreFactory is what a per-backend test file hands to
// RunPromptStoreConformance. The factory returns:
//   - the wired PromptStore impl
//   - the orgID to pass to every method (sqlite returns
//     runmode.LocalDefaultOrgID, postgres returns a fresh org UUID)
//   - the teamID Create should attribute prompts to. Every prompt is
//     team-scoped, so the seeder threads it; SQLite pins the local sentinel,
//     Postgres binds the test team.
//   - a RunSeeder hook that lets the harness create runs rows the
//     Stats subtests need. The harness doesn't know how to create
//     runs directly (RunStore lands in wave 3b); the backend test
//     owns that wiring against its own connection. Each backend
//     translates a logical fixture (promptID + N runs at given
//     timestamps) into its own schema's INSERT shape.
type PromptStoreFactory func(t *testing.T) (store db.PromptStore, orgID, teamID string, seedRuns RunSeederForStats)

// RunSeederForStats is a callback the harness invokes to populate
// rows in the conversations table for Stats assertions. statusByOffset maps
// row index → status string ("completed" / "failed" / "running"
// etc.); the seeder generates one run per entry, with started_at
// staggered across days so the per-day grouping has signal. Returns
// the inserted run IDs in case the harness wants to clean them up
// (it doesn't today — the per-test DB reset handles it). promptID is
// the prompt's id the Stats subtests created via Create.
type RunSeederForStats func(t *testing.T, promptID string, statusByOffset []string) []string

// RunPromptStoreConformance runs the shared assertion suite against
// any db.PromptStore impl. Each subtest gets a fresh store via
// factory() so test bodies don't have to coordinate state.
//
// Shipped-content seeding (system_slug, NULL creator) and the boot-time sync
// that keeps unmodified copies equal to shipped are covered by
// RunShippedSyncConformance; this suite uses plain Create because it only needs
// a prompt to exist. What this covers (and why):
//
//   - CRUD round-trips — minimal "the SQL parses + behaves" checks for
//     every method.
//   - Hidden filtering — List omits hidden rows, Get returns them.
//   - Soft-delete — request reads hide it, ...System still resolves it.
//   - Stats aggregation — totals + success rate + per-day grouping.
//   - Context cancellation — passing a cancelled ctx fails fast
//     rather than blocking.
func RunPromptStoreConformance(t *testing.T, factory PromptStoreFactory) {
	t.Helper()

	t.Run("CRUD_Roundtrip", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		// Create
		p := domain.Prompt{ID: "user-1", Name: "Mine", Body: "body", Source: "user", AllowedTools: "Read,Write"}
		if err := store.Create(ctx, orgID, teamID, p); err != nil {
			t.Fatalf("create: %v", err)
		}
		got, err := store.Get(ctx, orgID, "user-1")
		if err != nil || got == nil {
			t.Fatalf("get after create: err=%v got=%v", err, got)
		}
		if got.AllowedTools != "Read,Write" {
			t.Fatalf("allowed_tools=%q want Read,Write", got.AllowedTools)
		}
		// Update
		if err := store.Update(ctx, orgID, "user-1", "Mine v2", "body v2", "opus"); err != nil {
			t.Fatalf("update: %v", err)
		}
		got2, _ := store.Get(ctx, orgID, "user-1")
		if got2.Name != "Mine v2" || got2.Body != "body v2" {
			t.Fatalf("after update: got %+v", got2)
		}
		// IncrementUsage
		if err := store.IncrementUsage(ctx, orgID, "user-1"); err != nil {
			t.Fatalf("increment: %v", err)
		}
		if err := store.IncrementUsage(ctx, orgID, "user-1"); err != nil {
			t.Fatalf("increment2: %v", err)
		}
		got3, _ := store.Get(ctx, orgID, "user-1")
		if got3.UsageCount != 2 {
			t.Fatalf("usage_count=%d want 2", got3.UsageCount)
		}
		// Delete
		if err := store.Delete(ctx, orgID, "user-1"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		got4, _ := store.Get(ctx, orgID, "user-1")
		if got4 != nil {
			t.Fatalf("get after delete: want nil, got %+v", got4)
		}
	})

	t.Run("UpdateImported_RoundtripsContent", func(t *testing.T) {
		// UpdateImported is the file-importer's "re-import a changed
		// SKILL.md" write — it updates name/body/allowed_tools without
		// flipping user_modified (so a subsequent file change still
		// re-imports cleanly). The user_modified flag is internal and
		// not observable through the interface; the contract we CAN
		// pin from the outside is that the three fields round-trip.
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		if err := store.Create(ctx, orgID, teamID, domain.Prompt{
			ID: "imp-1", Name: "Imported", Body: "v1", Source: "imported", AllowedTools: "Read",
		}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := store.UpdateImported(ctx, orgID, "imp-1", "Renamed", "v2", "Read,Write"); err != nil {
			t.Fatalf("update imported: %v", err)
		}
		got, _ := store.Get(ctx, orgID, "imp-1")
		if got.Name != "Renamed" || got.Body != "v2" || got.AllowedTools != "Read,Write" {
			t.Fatalf("after UpdateImported: name=%q body=%q tools=%q", got.Name, got.Body, got.AllowedTools)
		}
	})

	t.Run("Hide_Unhide_FiltersList", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		if err := store.Create(ctx, orgID, teamID, domain.Prompt{ID: "u-visible", Name: "V", Body: "x", Source: "user"}); err != nil {
			t.Fatalf("create visible: %v", err)
		}
		if err := store.Create(ctx, orgID, teamID, domain.Prompt{ID: "u-hidden", Name: "H", Body: "x", Source: "user"}); err != nil {
			t.Fatalf("create hidden: %v", err)
		}
		if err := store.Hide(ctx, orgID, "u-hidden"); err != nil {
			t.Fatalf("hide: %v", err)
		}
		list, err := store.List(ctx, orgID, "")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if !containsPromptID(list, "u-visible") {
			t.Fatalf("visible row missing from List: %v", promptIDs(list))
		}
		if containsPromptID(list, "u-hidden") {
			t.Fatalf("hidden row leaked into List: %v", promptIDs(list))
		}
		// Get still returns the hidden row by ID (handler logic
		// decides what to do; the store doesn't filter by hidden on
		// Get).
		got, _ := store.Get(ctx, orgID, "u-hidden")
		if got == nil {
			t.Fatalf("Get should still return hidden rows by ID")
		}
		// Unhide brings it back
		if err := store.Unhide(ctx, orgID, "u-hidden"); err != nil {
			t.Fatalf("unhide: %v", err)
		}
		list2, _ := store.List(ctx, orgID, "")
		if !containsPromptID(list2, "u-hidden") {
			t.Fatalf("after Unhide, row still missing from List: %v", promptIDs(list2))
		}
	})

	t.Run("Stats_AggregatesRuns", func(t *testing.T) {
		store, orgID, teamID, seedRuns := factory(t)
		ctx := context.Background()
		// Set up: a prompt + 5 runs (3 completed, 1 failed, 1 running).
		id := "stats-p"
		if err := store.Create(ctx, orgID, teamID, domain.Prompt{ID: id, Name: "S", Body: "x", Source: "user"}); err != nil {
			t.Fatalf("create stats prompt: %v", err)
		}
		seedRuns(t, id, []string{"completed", "completed", "completed", "failed", "running"})
		stats, err := store.Stats(ctx, orgID, id)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if stats.TotalRuns != 5 {
			t.Fatalf("total_runs=%d want 5", stats.TotalRuns)
		}
		if stats.CompletedRuns != 3 {
			t.Fatalf("completed_runs=%d want 3", stats.CompletedRuns)
		}
		if stats.FailedRuns != 1 {
			t.Fatalf("failed_runs=%d want 1", stats.FailedRuns)
		}
		if stats.SuccessRate < 0.59 || stats.SuccessRate > 0.61 {
			t.Fatalf("success_rate=%f want ~0.60", stats.SuccessRate)
		}
		if len(stats.RunsPerDay) != 30 {
			t.Fatalf("runs_per_day len=%d want 30", len(stats.RunsPerDay))
		}
	})

	t.Run("Stats_NoRuns_ReturnsZeros", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id := "unused-p"
		if err := store.Create(ctx, orgID, teamID, domain.Prompt{ID: id, Name: "U", Body: "x", Source: "user"}); err != nil {
			t.Fatalf("create: %v", err)
		}
		stats, err := store.Stats(ctx, orgID, id)
		if err != nil {
			t.Fatalf("stats: %v", err)
		}
		if stats.TotalRuns != 0 {
			t.Fatalf("total_runs=%d want 0", stats.TotalRuns)
		}
		if stats.LastUsedAt != nil {
			t.Fatalf("last_used_at=%v want nil for never-used prompt", *stats.LastUsedAt)
		}
		if len(stats.RunsPerDay) != 30 {
			t.Fatalf("runs_per_day len=%d want 30 (skeleton)", len(stats.RunsPerDay))
		}
	})

	t.Run("SoftDelete_HidesFromListAndGetButSystemResolves", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		if err := store.Create(ctx, orgID, teamID, domain.Prompt{ID: "sd-1", Name: "SD", Body: "x", Source: "user"}); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := store.Delete(ctx, orgID, "sd-1"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		// Request-facing Get + List hide it.
		if got, err := store.Get(ctx, orgID, "sd-1"); err != nil || got != nil {
			t.Fatalf("Get after soft-delete = (%v, %v); want (nil, nil)", got, err)
		}
		if list, _ := store.List(ctx, orgID, ""); containsPromptID(list, "sd-1") {
			t.Fatalf("soft-deleted prompt leaked into List")
		}
		// ...System still resolves it so historical runs render the name/body.
		got, err := store.GetSystem(ctx, orgID, "sd-1")
		if err != nil || got == nil {
			t.Fatalf("GetSystem after soft-delete = (%v, %v); want a row", got, err)
		}
	})

	t.Run("Delete_WithRunHistory_Succeeds", func(t *testing.T) {
		// Regression: a user prompt with run history must be
		// deletable without hitting the conversations.prompt_id RESTRICT FK (a hard DELETE
		// would 500). Soft-delete sidesteps the FK and keeps the audit trail.
		store, orgID, teamID, seedRuns := factory(t)
		ctx := context.Background()
		if err := store.Create(ctx, orgID, teamID, domain.Prompt{ID: "rh-1", Name: "RH", Body: "x", Source: "user"}); err != nil {
			t.Fatalf("create: %v", err)
		}
		seedRuns(t, "rh-1", []string{"completed", "failed"})
		if err := store.Delete(ctx, orgID, "rh-1"); err != nil {
			t.Fatalf("delete prompt with run history failed (the FK-500 regression): %v", err)
		}
		if got, _ := store.Get(ctx, orgID, "rh-1"); got != nil {
			t.Fatalf("Get after delete = %+v; want nil", got)
		}
		if got, _ := store.GetSystem(ctx, orgID, "rh-1"); got == nil {
			t.Fatalf("GetSystem after delete = nil; the row + its runs must survive as the audit trail")
		}
	})

	t.Run("Get_Missing_ReturnsNilNoError", func(t *testing.T) {
		// Pre-D2 prompts.go convention: Get for a non-existent ID
		// returns (nil, nil), not an error. Handlers depend on it.
		store, orgID, _, _ := factory(t)
		got, err := store.Get(context.Background(), orgID, "does-not-exist")
		if err != nil {
			t.Fatalf("Get for missing returned err: %v", err)
		}
		if got != nil {
			t.Fatalf("Get for missing returned non-nil: %+v", got)
		}
	})

	t.Run("CtxCancellation_FailsFast", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := store.Create(ctx, orgID, teamID, domain.Prompt{ID: "ctxtest", Name: "C", Body: "x", Source: "user"}); err == nil {
			t.Fatalf("Create with cancelled ctx returned nil error")
		}
	})
}

func containsPromptID(list []domain.Prompt, id string) bool {
	for _, p := range list {
		if p.ID == id {
			return true
		}
	}
	return false
}

func promptIDs(list []domain.Prompt) []string {
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}
