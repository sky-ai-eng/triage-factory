package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// BlueprintStoreFactory is what a per-backend test file hands to
// RunBlueprintStoreConformance. It returns:
//   - the wired BlueprintStore impl
//   - the orgID to pass to every method (SQLite returns
//     runmode.LocalDefaultOrg, Postgres a fresh org UUID)
//   - the teamID Create/SeedOrUpdate should attribute blueprints to (every
//     blueprint is team-scoped)
//   - a PromptSeeder hook the harness invokes to materialize step prompts. A
//     blueprint_steps row FKs the step prompt on (step_prompt_id, team_id), so
//     ReplaceSteps needs real same-team prompt rows; the backend owns that
//     INSERT against its own schema and returns the prompt id.
type BlueprintStoreFactory func(t *testing.T) (store db.BlueprintStore, orgID, teamID string, seedPrompt PromptSeederForBlueprints)

// PromptSeederForBlueprints seeds one user prompt owned by the conformance
// team and returns its id, so the harness can wire blueprint steps without
// knowing each backend's prompt-insert shape.
type PromptSeederForBlueprints func(t *testing.T, idHint string) string

// RunBlueprintStoreConformance runs the shared seed-idempotency + CRUD +
// step round-trip suite against any db.BlueprintStore impl. It mirrors the
// prompt SeedOrUpdate idempotency tests:
//
//   - SeedOrUpdate a system blueprint twice → same id, no duplicate row.
//   - List returns the seeded blueprint.
//   - GetBySystemSlug resolves it (and returns nil for an unknown slug).
//   - ReplaceSteps then ListSteps round-trips the ordered step list.
//   - Context cancellation fails fast rather than blocking.
func RunBlueprintStoreConformance(t *testing.T, factory BlueprintStoreFactory) {
	t.Helper()

	t.Run("SeedOrUpdate_IsIdempotent", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id1, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "system-bp", Name: "BP", Source: "system",
		})
		if err != nil {
			t.Fatalf("seed #1: %v", err)
		}
		id2, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "system-bp", Name: "BP", Source: "system",
		})
		if err != nil {
			t.Fatalf("seed #2: %v", err)
		}
		if id1 != id2 {
			t.Fatalf("re-seed minted a new id (%s) instead of resolving the existing copy (%s)", id2, id1)
		}
		// No duplicate row: List must show exactly one blueprint for this slug.
		list, err := store.List(ctx, orgID, teamID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var seen int
		for _, b := range list {
			if b.SystemSlug == "system-bp" {
				seen++
			}
		}
		if seen != 1 {
			t.Fatalf("List shows %d copies of system-bp; want exactly 1 (re-seed duplicated)", seen)
		}
	})

	t.Run("List_ReturnsSeeded", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "system-list-bp", Name: "Listed", Source: "system",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		list, err := store.List(ctx, orgID, teamID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		found := false
		for _, b := range list {
			if b.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("List did not return the seeded blueprint %s", id)
		}
	})

	t.Run("GetBySystemSlug_ResolvesSeededCopy", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "system-slug-bp", Name: "Slugged", Source: "system",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		got, err := store.GetBySystemSlug(ctx, orgID, teamID, "system-slug-bp")
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

	t.Run("ReplaceSteps_ListSteps_RoundTrip", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bpID, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "system-steps-bp", Name: "Steps", Source: "system",
		})
		if err != nil {
			t.Fatalf("seed blueprint: %v", err)
		}
		p1 := seedPrompt(t, "step-1")
		p2 := seedPrompt(t, "step-2")
		if err := store.ReplaceSteps(ctx, orgID, bpID, []string{p1, p2}, []string{"first", "second"}); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		steps, err := store.ListSteps(ctx, orgID, bpID)
		if err != nil {
			t.Fatalf("ListSteps: %v", err)
		}
		if len(steps) != 2 {
			t.Fatalf("len(steps)=%d, want 2", len(steps))
		}
		if steps[0].StepIndex != 0 || steps[0].StepPromptID != p1 || steps[0].Brief != "first" {
			t.Errorf("step 0 = %+v, want index=0 prompt=%s brief=first", steps[0], p1)
		}
		if steps[1].StepIndex != 1 || steps[1].StepPromptID != p2 || steps[1].Brief != "second" {
			t.Errorf("step 1 = %+v, want index=1 prompt=%s brief=second", steps[1], p2)
		}

		// Re-ReplaceSteps with a shorter list collapses, not appends.
		if err := store.ReplaceSteps(ctx, orgID, bpID, []string{p2}, nil); err != nil {
			t.Fatalf("ReplaceSteps shrink: %v", err)
		}
		steps2, err := store.ListSteps(ctx, orgID, bpID)
		if err != nil {
			t.Fatalf("ListSteps after shrink: %v", err)
		}
		if len(steps2) != 1 || steps2[0].StepPromptID != p2 || steps2[0].Brief != "" {
			t.Fatalf("after shrink: %+v, want one step prompt=%s empty brief", steps2, p2)
		}
	})

	t.Run("Update_RoundTripsNameAndDescription", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "system-update-bp", Name: "Before", Source: "system",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := store.Update(ctx, orgID, id, "After", "A longer description"); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got == nil {
			t.Fatalf("Get returned nil after Update")
		}
		if got.Name != "After" {
			t.Errorf("name = %q, want After", got.Name)
		}
		if got.Description != "A longer description" {
			t.Errorf("description = %q, want 'A longer description' (must round-trip)", got.Description)
		}
	})

	t.Run("Delete_RemovesBlueprint", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "system-delete-bp", Name: "Doomed", Source: "system",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := store.Delete(ctx, orgID, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil {
			t.Fatalf("Get after delete: %v", err)
		}
		if got != nil {
			t.Fatalf("Get after Delete returned %+v, want nil", got)
		}
		// And it must drop out of List.
		list, err := store.List(ctx, orgID, teamID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, b := range list {
			if b.ID == id {
				t.Fatalf("List still shows deleted blueprint %s", id)
			}
		}
	})

	t.Run("SeedOrUpdate_CtxCancellation_FailsFast", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "system-ctx-bp", Name: "Ctx", Source: "system",
		}); err == nil {
			t.Fatalf("SeedOrUpdate with cancelled ctx returned nil error")
		}
	})
}
