package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PromptStoreFactory is what a per-backend test file hands to
// RunPromptStoreConformance. The factory returns:
//   - the wired PromptStore impl
//   - the orgID to pass to every method (sqlite returns
//     runmode.LocalDefaultOrg, postgres returns a fresh org UUID)
//   - the teamID Create/SeedOrUpdate should attribute prompts to. Every
//     prompt is team-scoped post-SKY-380, so the seeder threads it; SQLite
//     pins the local sentinel, Postgres binds the test team.
//   - a RunSeeder hook that lets the harness create runs rows the
//     Stats subtests need. The harness doesn't know how to create
//     runs directly (RunStore lands in wave 3b); the backend test
//     owns that wiring against its own connection. Each backend
//     translates a logical fixture (promptID + N runs at given
//     timestamps) into its own schema's INSERT shape.
type PromptStoreFactory func(t *testing.T) (store db.PromptStore, orgID, teamID string, seedRuns RunSeederForStats)

// RunSeederForStats is a callback the harness invokes to populate
// rows in the runs table for Stats assertions. statusByOffset maps
// row index → status string ("completed" / "failed" / "running"
// etc.); the seeder generates one run per entry, with started_at
// staggered across days so the per-day grouping has signal. Returns
// the inserted run IDs in case the harness wants to clean them up
// (it doesn't today — the per-test DB reset handles it). promptID is
// the prompt's id (a random UUID post-SKY-380 — the harness passes the
// id SeedOrUpdate returned, not the slug).
type RunSeederForStats func(t *testing.T, promptID string, statusByOffset []string) []string

// RunPromptStoreConformance runs the shared assertion suite against
// any db.PromptStore impl. Each subtest gets a fresh store via
// factory() so test bodies don't have to coordinate state.
//
// SKY-380 note: prompts are team-scoped and identified by a random UUID;
// the shipped slug lives in system_slug. SeedOrUpdate dedupes on
// (org_id, team_id, system_slug) and returns the team copy's id, so the
// suite seeds by SystemSlug and reads back by the returned id. The
// per-team-divergence case (two teams, same slug, one user-modified)
// needs a real second team, so it lives in the Postgres package
// (TestPromptStore_Postgres_PerTeamDivergence) — SQLite is N=1 and the
// divergence is vacuous there.
//
// What this covers (and why):
//
//   - Seed/identical-reseed/metadata-change/user-modified-guard paths —
//     these are the load-bearing invariants the pre-D2 prompts_test.go
//     validated, ported here so both backends prove them.
//   - CRUD round-trips — minimal "the SQL parses + behaves" checks
//     for every method that isn't covered by the Seed paths.
//   - Hidden filtering — List omits hidden rows, Get returns them.
//   - Stats aggregation — totals + success rate + per-day grouping.
//   - Context cancellation — passing a cancelled ctx fails fast
//     rather than blocking. Pre-empts the review-bot finding from
//     the ScoreStore pilot.
func RunPromptStoreConformance(t *testing.T, factory PromptStoreFactory) {
	t.Helper()

	t.Run("SeedOrUpdate_FreshInsert", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-x", Name: "X", Body: "v1", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got == nil || got.Body != "v1" || got.Name != "X" {
			t.Fatalf("after fresh seed: got=%+v want body=v1 name=X", got)
		}
		if got.SystemSlug != "system-x" {
			t.Fatalf("system_slug=%q want system-x", got.SystemSlug)
		}
		if got.TeamID != teamID {
			t.Fatalf("team_id=%q want %q", got.TeamID, teamID)
		}
	})

	t.Run("SeedOrUpdate_UpdatesUntouchedPrompt", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-x", Name: "X", Body: "v1", Source: "system"})
		if err != nil {
			t.Fatalf("seed v1: %v", err)
		}
		// Re-seed the same (team, slug) — must resolve the same row id.
		id2, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-x", Name: "X2", Body: "v2", Source: "system"})
		if err != nil {
			t.Fatalf("seed v2: %v", err)
		}
		if id2 != id {
			t.Fatalf("re-seed minted a new id (%s) instead of resolving the existing copy (%s)", id2, id)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Body != "v2" || got.Name != "X2" {
			t.Fatalf("after re-seed: body=%q name=%q want v2 / X2", got.Body, got.Name)
		}
	})

	t.Run("SeedOrUpdate_PreservesUserModified", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-y", Name: "Y", Body: "v1", Source: "system"})
		if err != nil {
			t.Fatalf("seed v1: %v", err)
		}
		if err := store.Update(ctx, orgID, id, "Custom", "custom body", "leaf", ""); err != nil {
			t.Fatalf("user update: %v", err)
		}
		// Re-seed with new shipped content — must NOT overwrite the
		// user's edit.
		if _, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-y", Name: "Y", Body: "v2", Source: "system"}); err != nil {
			t.Fatalf("seed v2: %v", err)
		}
		got, _ := store.Get(ctx, orgID, id)
		if got.Body != "custom body" || got.Name != "Custom" {
			t.Fatalf("user-modified prompt was overwritten: got name=%q body=%q", got.Name, got.Body)
		}
	})

	t.Run("SeedOrUpdate_NoChurnOnIdenticalReseed", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-q", Name: "Q", Body: "v1", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		first, _ := store.Get(ctx, orgID, id)
		updatedBefore := first.UpdatedAt
		// Sleep so any churn would produce a strictly-greater timestamp.
		time.Sleep(15 * time.Millisecond)
		if _, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-q", Name: "Q", Body: "v1", Source: "system"}); err != nil {
			t.Fatalf("reseed: %v", err)
		}
		second, _ := store.Get(ctx, orgID, id)
		if !second.UpdatedAt.Equal(updatedBefore) {
			t.Fatalf("prompts.updated_at churned on identical reseed: before=%s after=%s", updatedBefore, second.UpdatedAt)
		}
	})

	t.Run("SeedOrUpdate_UpdatesOnMetadataChange", func(t *testing.T) {
		// The hash covers (name, body, source) so renaming alone must
		// trip an update — even with body unchanged. Ensures
		// shipped-rename ships.
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-m", Name: "Old Name", Body: "same body", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-m", Name: "New Name", Body: "same body", Source: "system"}); err != nil {
			t.Fatalf("seed rename: %v", err)
		}
		got, _ := store.Get(ctx, orgID, id)
		if got.Name != "New Name" {
			t.Fatalf("name=%q want New Name; metadata-only change should apply", got.Name)
		}
	})

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
		if err := store.Update(ctx, orgID, "user-1", "Mine v2", "body v2", "leaf", "opus"); err != nil {
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

	t.Run("SeedOrUpdate_RejectsNonSystemSource", func(t *testing.T) {
		// SeedOrUpdate is the shipped-content seeder; non-"system"
		// sources are rejected so a misuse can't accidentally land
		// version-sidecar rows on user/imported prompts (where a
		// later re-seed could silently overwrite them).
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		for _, src := range []string{"user", "imported", "garbage"} {
			_, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{
				SystemSlug: "bad-" + src, Name: "X", Body: "x", Source: src,
			})
			if err == nil {
				t.Fatalf("SeedOrUpdate accepted Source=%q; should reject", src)
			}
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
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "stats-p", Name: "S", Body: "x", Source: "system"})
		if err != nil {
			t.Fatalf("seed stats prompt: %v", err)
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
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "unused-p", Name: "U", Body: "x", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
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

	t.Run("GetBySystemSlug_ResolvesSeededCopy", func(t *testing.T) {
		// The shipped-prompt id is a random UUID per team copy (SKY-380);
		// callers resolve by slug. GetBySystemSlug must return the same row
		// SeedOrUpdate created, and (nil, nil) for an unknown slug.
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "system-slug-lookup", Name: "L", Body: "x", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := store.GetBySystemSlug(ctx, orgID, teamID, "system-slug-lookup")
		if err != nil {
			t.Fatalf("GetBySystemSlug: %v", err)
		}
		if got == nil || got.ID != id {
			t.Fatalf("GetBySystemSlug returned %+v; want id=%s", got, id)
		}
		missing, err := store.GetBySystemSlug(ctx, orgID, teamID, "no-such-slug")
		if err != nil {
			t.Fatalf("GetBySystemSlug(missing): %v", err)
		}
		if missing != nil {
			t.Fatalf("GetBySystemSlug(missing) = %+v; want nil", missing)
		}
	})

	t.Run("CtxCancellation_FailsFast", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Prompt{SystemSlug: "ctxtest", Name: "C", Body: "x", Source: "system"}); err == nil {
			t.Fatalf("SeedOrUpdate with cancelled ctx returned nil error")
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
