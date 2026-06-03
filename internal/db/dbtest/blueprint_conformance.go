package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

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

	t.Run("CopyOnly_PromptInOneBlueprintOnly", func(t *testing.T) {
		// The durable copy-only invariant: a prompt is a step in at most one
		// blueprint. Adding it to a second blueprint must fail at the DB layer.
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp1, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "copyonly-bp1", Name: "BP1", Source: "system"})
		if err != nil {
			t.Fatalf("seed bp1: %v", err)
		}
		bp2, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "copyonly-bp2", Name: "BP2", Source: "system"})
		if err != nil {
			t.Fatalf("seed bp2: %v", err)
		}
		p := seedPrompt(t, "shared")
		if err := store.ReplaceSteps(ctx, orgID, bp1, []string{p}, nil); err != nil {
			t.Fatalf("ReplaceSteps bp1: %v", err)
		}
		if err := store.ReplaceSteps(ctx, orgID, bp2, []string{p}, nil); err == nil {
			t.Fatalf("ReplaceSteps bp2 with an already-owned prompt succeeded; want unique-constraint error")
		}
	})

	t.Run("StepPromptOwner_ResolvesOwningBlueprint", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "owner-bp", Name: "Owner", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		p := seedPrompt(t, "owned")
		if err := store.ReplaceSteps(ctx, orgID, bp, []string{p}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		owner, ok, err := store.StepPromptOwner(ctx, orgID, p)
		if err != nil {
			t.Fatalf("StepPromptOwner: %v", err)
		}
		if !ok || owner != bp {
			t.Fatalf("StepPromptOwner = (%q, %v), want (%q, true)", owner, ok, bp)
		}
		// A prompt that isn't a step in any blueprint resolves to ok=false.
		orphan := seedPrompt(t, "orphan")
		if _, ok, err := store.StepPromptOwner(ctx, orgID, orphan); err != nil || ok {
			t.Fatalf("StepPromptOwner(orphan) = (_, %v, %v), want (_, false, nil)", ok, err)
		}
	})

	t.Run("SoftDelete_HidesFromRequestReadsButSystemResolves", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "softdel-bp", Name: "Soft", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := store.Delete(ctx, orgID, bp); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// Request-facing Get + List hide it.
		if got, err := store.Get(ctx, orgID, bp); err != nil || got != nil {
			t.Fatalf("Get after soft-delete = (%v, %v); want (nil, nil)", got, err)
		}
		list, err := store.List(ctx, orgID, teamID)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, b := range list {
			if b.ID == bp {
				t.Fatalf("soft-deleted blueprint leaked into List")
			}
		}
		// ...System still resolves it (in-flight runs / past-run timelines).
		if got, err := store.GetSystem(ctx, orgID, bp); err != nil || got == nil {
			t.Fatalf("GetSystem after soft-delete = (%v, %v); want a row", got, err)
		}
	})

	t.Run("StepPromptOwner_FalseAfterBlueprintSoftDelete", func(t *testing.T) {
		// Regression: StepPromptOwner must return ok=false once the owning
		// blueprint is soft-deleted. The step row persists as an audit trail
		// (RESTRICT FK), so without the deleted_at IS NULL join, StepPromptOwner
		// incorrectly returns ok=true — blocking the prompt from being claimed by
		// a new blueprint with a phantom 422.
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "softdel-owner-bp", Name: "SoftDelOwner", Source: "system",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		p := seedPrompt(t, "paired-prompt")
		if err := store.ReplaceSteps(ctx, orgID, bp, []string{p}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		// Confirm the prompt is owned before deletion.
		owner, ok, err := store.StepPromptOwner(ctx, orgID, p)
		if err != nil || !ok || owner != bp {
			t.Fatalf("pre-delete: StepPromptOwner = (%q, %v, %v); want (%q, true, nil)", owner, ok, err, bp)
		}
		// Soft-delete the owning blueprint (simulates the delete-pairing path).
		if err := store.Delete(ctx, orgID, bp); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// After soft-delete, StepPromptOwner must return ok=false so the prompt
		// can be claimed by a new blueprint without a spurious 422.
		_, ok, err = store.StepPromptOwner(ctx, orgID, p)
		if err != nil || ok {
			t.Fatalf("post-delete: StepPromptOwner = (_, %v, %v); want (_, false, nil)", ok, err)
		}
	})

	t.Run("MergeInto_AppendsSourceStepsAndRetiresSource", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		host, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "merge-host", Name: "Host", Source: "system"})
		if err != nil {
			t.Fatalf("seed host: %v", err)
		}
		source, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "merge-source", Name: "Source", Source: "system"})
		if err != nil {
			t.Fatalf("seed source: %v", err)
		}
		h1, h2 := seedPrompt(t, "host-1"), seedPrompt(t, "host-2")
		s1, s2 := seedPrompt(t, "src-1"), seedPrompt(t, "src-2")
		if err := store.ReplaceSteps(ctx, orgID, host, []string{h1, h2}, []string{"h1", "h2"}); err != nil {
			t.Fatalf("ReplaceSteps host: %v", err)
		}
		if err := store.ReplaceSteps(ctx, orgID, source, []string{s1, s2}, []string{"s1", "s2"}); err != nil {
			t.Fatalf("ReplaceSteps source: %v", err)
		}
		if err := store.MergeInto(ctx, orgID, host, source); err != nil {
			t.Fatalf("MergeInto: %v", err)
		}
		// Host now resolves with N+M densely-indexed steps in order.
		steps, err := store.ListSteps(ctx, orgID, host)
		if err != nil {
			t.Fatalf("ListSteps host: %v", err)
		}
		wantPrompts := []string{h1, h2, s1, s2}
		wantBriefs := []string{"h1", "h2", "s1", "s2"}
		if len(steps) != 4 {
			t.Fatalf("merged host has %d steps, want 4: %+v", len(steps), steps)
		}
		for i, st := range steps {
			if st.StepIndex != i {
				t.Errorf("step %d has index %d, want dense %d", i, st.StepIndex, i)
			}
			if st.StepPromptID != wantPrompts[i] {
				t.Errorf("step %d prompt = %s, want %s", i, st.StepPromptID, wantPrompts[i])
			}
			if st.Brief != wantBriefs[i] {
				t.Errorf("step %d brief = %q, want %q", i, st.Brief, wantBriefs[i])
			}
		}
		// Source is retired: request-facing Get → nil, GetSystem still resolves.
		if got, err := store.Get(ctx, orgID, source); err != nil || got != nil {
			t.Fatalf("Get(source) after merge = (%v, %v), want (nil, nil)", got, err)
		}
		if got, err := store.GetSystem(ctx, orgID, source); err != nil || got == nil {
			t.Fatalf("GetSystem(source) after merge = (%v, %v), want a row", got, err)
		}
		// step_prompt_id uniqueness still holds: each moved prompt now resolves
		// to the host as its sole owner.
		for _, p := range []string{s1, s2} {
			owner, ok, err := store.StepPromptOwner(ctx, orgID, p)
			if err != nil || !ok || owner != host {
				t.Fatalf("StepPromptOwner(%s) = (%q, %v, %v), want (%q, true, nil)", p, owner, ok, err, host)
			}
		}
	})

	t.Run("SplitAt_PartitionsAndCreatesTriggerlessDownstream", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "split-bp", Name: "Split", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		p0, p1, p2, p3 := seedPrompt(t, "sp-0"), seedPrompt(t, "sp-1"), seedPrompt(t, "sp-2"), seedPrompt(t, "sp-3")
		if err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1, p2, p3}, []string{"b0", "b1", "b2", "b3"}); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		newID := uuid.New().String()
		got, err := store.SplitAt(ctx, orgID, bp, 2, newID, "Downstream")
		if err != nil {
			t.Fatalf("SplitAt: %v", err)
		}
		if got != newID {
			t.Fatalf("SplitAt returned %s, want %s", got, newID)
		}
		// Upstream keeps [0,2) at their original indices.
		up, err := store.ListSteps(ctx, orgID, bp)
		if err != nil {
			t.Fatalf("ListSteps upstream: %v", err)
		}
		if len(up) != 2 || up[0].StepPromptID != p0 || up[1].StepPromptID != p1 {
			t.Fatalf("upstream steps = %+v, want [p0,p1]", up)
		}
		if up[0].StepIndex != 0 || up[1].StepIndex != 1 {
			t.Errorf("upstream indices not dense: %+v", up)
		}
		// Downstream gets [2,N) re-densified 0-based with their briefs.
		down, err := store.ListSteps(ctx, orgID, newID)
		if err != nil {
			t.Fatalf("ListSteps downstream: %v", err)
		}
		if len(down) != 2 || down[0].StepPromptID != p2 || down[1].StepPromptID != p3 {
			t.Fatalf("downstream steps = %+v, want [p2,p3]", down)
		}
		if down[0].StepIndex != 0 || down[1].StepIndex != 1 {
			t.Errorf("downstream indices not re-densified 0-based: %+v", down)
		}
		if down[0].Brief != "b2" || down[1].Brief != "b3" {
			t.Errorf("downstream briefs = [%q,%q], want [b2,b3]", down[0].Brief, down[1].Brief)
		}
		// The downstream blueprint exists, is request-visible, and carries the
		// supplied name.
		dbp, err := store.Get(ctx, orgID, newID)
		if err != nil || dbp == nil {
			t.Fatalf("Get(downstream) = (%v, %v), want a row", dbp, err)
		}
		if dbp.Name != "Downstream" {
			t.Errorf("downstream name = %q, want Downstream", dbp.Name)
		}
		// at_step_index at the ends is the handler's job to reject, but the store
		// must keep the partition non-empty when asked to split at a real
		// boundary — covered above. Each prompt still has exactly one owner.
		ownerUp, _, _ := store.StepPromptOwner(ctx, orgID, p1)
		ownerDown, _, _ := store.StepPromptOwner(ctx, orgID, p2)
		if ownerUp != bp || ownerDown != newID {
			t.Fatalf("ownership after split: p1→%s (want %s), p2→%s (want %s)", ownerUp, bp, ownerDown, newID)
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
