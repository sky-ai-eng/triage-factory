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

	t.Run("Rename_UpdatesName", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: "rename-bp", Name: "Before", Source: "system",
		})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := store.Rename(ctx, orgID, id, "After"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get after rename = (%v, %v)", got, err)
		}
		if got.Name != "After" {
			t.Errorf("renamed blueprint name = %q, want %q", got.Name, "After")
		}
	})

	t.Run("ListAllSteps_GroupsAndExcludesDeleted", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bpA, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "all-a", Name: "A", Source: "system"})
		if err != nil {
			t.Fatalf("seed A: %v", err)
		}
		bpB, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "all-b", Name: "B", Source: "system"})
		if err != nil {
			t.Fatalf("seed B: %v", err)
		}
		bpDel, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "all-del", Name: "Del", Source: "system"})
		if err != nil {
			t.Fatalf("seed Del: %v", err)
		}
		a1, a2 := seedPrompt(t, "all-a1"), seedPrompt(t, "all-a2")
		b1 := seedPrompt(t, "all-b1")
		d1 := seedPrompt(t, "all-d1")
		if err := store.ReplaceSteps(ctx, orgID, bpA, []string{a1, a2}, nil); err != nil {
			t.Fatalf("steps A: %v", err)
		}
		if err := store.ReplaceSteps(ctx, orgID, bpB, []string{b1}, nil); err != nil {
			t.Fatalf("steps B: %v", err)
		}
		if err := store.ReplaceSteps(ctx, orgID, bpDel, []string{d1}, nil); err != nil {
			t.Fatalf("steps Del: %v", err)
		}
		// Soft-deleted blueprints' steps must be excluded from the bulk read.
		if err := store.Delete(ctx, orgID, bpDel); err != nil {
			t.Fatalf("delete Del: %v", err)
		}

		all, err := store.ListAllSteps(ctx, orgID, teamID)
		if err != nil {
			t.Fatalf("ListAllSteps: %v", err)
		}
		got := map[string][]string{}
		for _, st := range all {
			got[st.BlueprintID] = append(got[st.BlueprintID], st.StepPromptID)
		}
		// Within a blueprint the order is step_index-ascending.
		if len(got[bpA]) != 2 || got[bpA][0] != a1 || got[bpA][1] != a2 {
			t.Errorf("bpA steps = %v, want [%s %s]", got[bpA], a1, a2)
		}
		if len(got[bpB]) != 1 || got[bpB][0] != b1 {
			t.Errorf("bpB steps = %v, want [%s]", got[bpB], b1)
		}
		if _, present := got[bpDel]; present {
			t.Errorf("soft-deleted blueprint's steps leaked into ListAllSteps: %v", got[bpDel])
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

	// DeleteStep fragments a multi-step blueprint per the split rule. The
	// trigger side of each case is the EventHandler store's concern (handler
	// tests cover it); here we assert the structural partition: which component
	// keeps the original id, which becomes a new trigger-less downstream, that
	// the deleted step is isolated onto a soft-deleted wrapper, and that
	// step_index stays dense.
	t.Run("DeleteStep_TailDropsLastKeepsId", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "del-tail", Name: "Tail", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		p0, p1, p2 := seedPrompt(t, "dt-0"), seedPrompt(t, "dt-1"), seedPrompt(t, "dt-2")
		if err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1, p2}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		down, err := store.DeleteStep(ctx, orgID, bp, 2, "")
		if err != nil {
			t.Fatalf("DeleteStep: %v", err)
		}
		if down != "" {
			t.Fatalf("tail delete minted a downstream blueprint %q; want none", down)
		}
		// Original keeps [p0,p1] densely, request-visible.
		assertDenseSteps(t, store, orgID, bp, []string{p0, p1})
		if got, err := store.Get(ctx, orgID, bp); err != nil || got == nil {
			t.Fatalf("Get(original) after tail delete = (%v, %v), want a row", got, err)
		}
		// Deleted prompt is isolated onto a soft-deleted wrapper: no live owner.
		if _, ok, err := store.StepPromptOwner(ctx, orgID, p2); err != nil || ok {
			t.Fatalf("StepPromptOwner(deleted) = (_, %v, %v), want (_, false, nil)", ok, err)
		}
	})

	t.Run("DeleteStep_HeadDetachesEntryRetiresId", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "del-head", Name: "Head", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		p0, p1, p2 := seedPrompt(t, "dh-0"), seedPrompt(t, "dh-1"), seedPrompt(t, "dh-2")
		if err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1, p2}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		down, err := store.DeleteStep(ctx, orgID, bp, 0, "Downstream")
		if err != nil {
			t.Fatalf("DeleteStep: %v", err)
		}
		if down == "" {
			t.Fatalf("head delete minted no downstream blueprint; want one")
		}
		// The original blueprint_id retires with the entry prompt (soft-deleted).
		if got, err := store.Get(ctx, orgID, bp); err != nil || got != nil {
			t.Fatalf("Get(original) after head delete = (%v, %v), want (nil, nil)", got, err)
		}
		if got, err := store.GetSystem(ctx, orgID, bp); err != nil || got == nil {
			t.Fatalf("GetSystem(original) after head delete = (%v, %v), want a row", got, err)
		}
		// Remainder [p1,p2] is the new trigger-less downstream, re-densified.
		assertDenseSteps(t, store, orgID, down, []string{p1, p2})
		if _, ok, err := store.StepPromptOwner(ctx, orgID, p0); err != nil || ok {
			t.Fatalf("StepPromptOwner(deleted entry) = (_, %v, %v), want (_, false, nil)", ok, err)
		}
	})

	t.Run("DeleteStep_MidSplitsKeepsUpstreamId", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "del-mid", Name: "Mid", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		p0, p1, p2, p3 := seedPrompt(t, "dm-0"), seedPrompt(t, "dm-1"), seedPrompt(t, "dm-2"), seedPrompt(t, "dm-3")
		if err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1, p2, p3}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		// Delete the middle step (index 1): upstream keeps [p0], downstream gets
		// [p2,p3], the deleted p1 is isolated.
		down, err := store.DeleteStep(ctx, orgID, bp, 1, "Downstream")
		if err != nil {
			t.Fatalf("DeleteStep: %v", err)
		}
		if down == "" {
			t.Fatalf("mid delete minted no downstream blueprint; want one")
		}
		if down == bp {
			t.Fatalf("mid delete returned the original id as downstream")
		}
		assertDenseSteps(t, store, orgID, bp, []string{p0})
		assertDenseSteps(t, store, orgID, down, []string{p2, p3})
		// Upstream still request-visible (keeps trigger + id); deleted isolated.
		if got, err := store.Get(ctx, orgID, bp); err != nil || got == nil {
			t.Fatalf("Get(upstream) after mid delete = (%v, %v), want a row", got, err)
		}
		if _, ok, err := store.StepPromptOwner(ctx, orgID, p1); err != nil || ok {
			t.Fatalf("StepPromptOwner(deleted mid) = (_, %v, %v), want (_, false, nil)", ok, err)
		}
		// Surviving prompts resolve to their new owners.
		if owner, _, _ := store.StepPromptOwner(ctx, orgID, p0); owner != bp {
			t.Errorf("p0 owner = %s, want upstream %s", owner, bp)
		}
		if owner, _, _ := store.StepPromptOwner(ctx, orgID, p2); owner != down {
			t.Errorf("p2 owner = %s, want downstream %s", owner, down)
		}
	})

	// n=2 is the smallest multi-step blueprint — head and tail are the only
	// positions (no middle), and each surviving side is a 1-step blueprint, so
	// the index shifts have no buffer. Worth pinning as the boundary regression.
	t.Run("DeleteStep_MinSizeHead", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "del-min-head", Name: "MinHead", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		p0, p1 := seedPrompt(t, "dmh-0"), seedPrompt(t, "dmh-1")
		if err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		down, err := store.DeleteStep(ctx, orgID, bp, 0, "Downstream")
		if err != nil {
			t.Fatalf("DeleteStep: %v", err)
		}
		if down == "" {
			t.Fatalf("2-step head delete minted no downstream blueprint; want one")
		}
		// Original retires; the lone remaining step is the new downstream at index 0.
		if got, err := store.Get(ctx, orgID, bp); err != nil || got != nil {
			t.Fatalf("Get(original) after 2-step head delete = (%v, %v), want (nil, nil)", got, err)
		}
		assertDenseSteps(t, store, orgID, down, []string{p1})
		if _, ok, err := store.StepPromptOwner(ctx, orgID, p0); err != nil || ok {
			t.Fatalf("StepPromptOwner(deleted entry) = (_, %v, %v), want (_, false, nil)", ok, err)
		}
	})

	t.Run("DeleteStep_MinSizeTail", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "del-min-tail", Name: "MinTail", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		p0, p1 := seedPrompt(t, "dmt-0"), seedPrompt(t, "dmt-1")
		if err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		down, err := store.DeleteStep(ctx, orgID, bp, 1, "")
		if err != nil {
			t.Fatalf("DeleteStep: %v", err)
		}
		if down != "" {
			t.Fatalf("2-step tail delete minted a downstream blueprint %q; want none", down)
		}
		// Original keeps its lone surviving step at index 0, request-visible.
		assertDenseSteps(t, store, orgID, bp, []string{p0})
		if got, err := store.Get(ctx, orgID, bp); err != nil || got == nil {
			t.Fatalf("Get(original) after 2-step tail delete = (%v, %v), want a row", got, err)
		}
		if _, ok, err := store.StepPromptOwner(ctx, orgID, p1); err != nil || ok {
			t.Fatalf("StepPromptOwner(deleted) = (_, %v, %v), want (_, false, nil)", ok, err)
		}
	})

	t.Run("DeleteStep_RejectsSingleStep", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: "del-single", Name: "Single", Source: "system"})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		p := seedPrompt(t, "ds-0")
		if err := store.ReplaceSteps(ctx, orgID, bp, []string{p}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		// The sole-owner 1-step case is the handler's pair-delete path, not
		// DeleteStep — the store guards against being misused for it.
		if _, err := store.DeleteStep(ctx, orgID, bp, 0, ""); err == nil {
			t.Fatalf("DeleteStep on a 1-step blueprint returned nil error; want a guard error")
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

// assertDenseSteps reads a blueprint's steps and asserts they are exactly
// wantPrompts in order at densely-packed 0-based indices.
func assertDenseSteps(t *testing.T, store db.BlueprintStore, orgID, blueprintID string, wantPrompts []string) {
	t.Helper()
	steps, err := store.ListSteps(context.Background(), orgID, blueprintID)
	if err != nil {
		t.Fatalf("ListSteps(%s): %v", blueprintID, err)
	}
	if len(steps) != len(wantPrompts) {
		t.Fatalf("blueprint %s has %d steps, want %d: %+v", blueprintID, len(steps), len(wantPrompts), steps)
	}
	for i, st := range steps {
		if st.StepIndex != i {
			t.Errorf("blueprint %s step %d has index %d, want dense %d", blueprintID, i, st.StepIndex, i)
		}
		if st.StepPromptID != wantPrompts[i] {
			t.Errorf("blueprint %s step %d prompt = %s, want %s", blueprintID, i, st.StepPromptID, wantPrompts[i])
		}
	}
}

// --- Duplication conformance ---------------------------------------------
//
// DuplicatePrompts deep-copies prompt rows, so its conformance needs to read
// prompts back (body fidelity, source flip) — beyond the BlueprintStore-only
// surface RunBlueprintStoreConformance carries. It gets its own richer factory
// + entry point rather than widening the shared one and churning every call
// site.

// DuplicationPromptSeeder seeds one prompt owned by the conformance team and
// returns its id. The backend owns the INSERT against its own schema; p.ID may
// be empty (the seeder mints one). Source "system" rows must satisfy each
// dialect's "system has no creator" CHECK — the seeder handles that.
type DuplicationPromptSeeder func(t *testing.T, p domain.Prompt) string

// DuplicationPromptGetter reads a prompt by id so the suite can assert deep-copy
// fidelity (copied body, source flipped to user). It returns the row's
// name / body / model / allowed_tools / source.
type DuplicationPromptGetter func(t *testing.T, id string) domain.Prompt

// BlueprintDuplicationFactory wires the duplication suite against one backend.
type BlueprintDuplicationFactory func(t *testing.T) (store db.BlueprintStore, orgID, teamID string, seedPrompt DuplicationPromptSeeder, getPrompt DuplicationPromptGetter)

// RunBlueprintDuplicationConformance exercises BlueprintStore.DuplicatePrompts
// against any backend: full-blueprint clone, non-contiguous split, contiguous
// sub-run re-densify, and the system→user source flip — asserting originals are
// untouched and step_prompt_id uniqueness holds for every new step.
func RunBlueprintDuplicationConformance(t *testing.T, factory BlueprintDuplicationFactory) {
	t.Helper()

	// seedRun creates a source blueprint with the given prompts as ordered steps
	// (briefs[i] paired positionally) and returns the blueprint id + prompt ids.
	seedRun := func(t *testing.T, store db.BlueprintStore, orgID, teamID, slug string, prompts []domain.Prompt, seed DuplicationPromptSeeder, briefs []string) (string, []string) {
		t.Helper()
		ctx := context.Background()
		bpID, err := store.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{SystemSlug: slug, Name: slug, Source: "system"})
		if err != nil {
			t.Fatalf("seed blueprint %s: %v", slug, err)
		}
		ids := make([]string, len(prompts))
		for i, p := range prompts {
			ids[i] = seed(t, p)
		}
		if err := store.ReplaceSteps(ctx, orgID, bpID, ids, briefs); err != nil {
			t.Fatalf("ReplaceSteps %s: %v", slug, err)
		}
		return bpID, ids
	}

	t.Run("DuplicateFullBlueprint", func(t *testing.T) {
		store, orgID, teamID, seed, getPrompt := factory(t)
		ctx := context.Background()
		srcID, srcPrompts := seedRun(t, store, orgID, teamID, "dup-full",
			[]domain.Prompt{
				{Name: "Map", Body: "map the surface", Model: "opus", Source: "user"},
				{Name: "Write", Body: "write the review", Source: "user"},
			}, seed, []string{"brief-a", "brief-b"})

		newIDs, err := store.DuplicatePrompts(ctx, orgID, teamID, srcPrompts)
		if err != nil {
			t.Fatalf("DuplicatePrompts: %v", err)
		}
		if len(newIDs) != 1 {
			t.Fatalf("full-blueprint duplicate produced %d blueprints, want 1", len(newIDs))
		}
		bp, err := store.Get(ctx, orgID, newIDs[0])
		if err != nil || bp == nil {
			t.Fatalf("Get(new) = (%v, %v), want a row", bp, err)
		}
		if bp.Name != "dup-full (copy)" {
			t.Errorf("full clone name = %q, want %q", bp.Name, "dup-full (copy)")
		}
		steps, err := store.ListSteps(ctx, orgID, newIDs[0])
		if err != nil {
			t.Fatalf("ListSteps(new): %v", err)
		}
		if len(steps) != 2 {
			t.Fatalf("new blueprint has %d steps, want 2", len(steps))
		}
		wantBody := []string{"map the surface", "write the review"}
		wantBrief := []string{"brief-a", "brief-b"}
		wantModel := []string{"opus", ""}
		for i, st := range steps {
			if st.StepIndex != i {
				t.Errorf("step %d index = %d, want %d", i, st.StepIndex, i)
			}
			if st.StepPromptID == srcPrompts[i] {
				t.Errorf("step %d reuses source prompt id %s; want a fresh copy", i, st.StepPromptID)
			}
			if st.Brief != wantBrief[i] {
				t.Errorf("step %d brief = %q, want %q", i, st.Brief, wantBrief[i])
			}
			cp := getPrompt(t, st.StepPromptID)
			if cp.Body != wantBody[i] {
				t.Errorf("copied prompt %d body = %q, want %q", i, cp.Body, wantBody[i])
			}
			if cp.Model != wantModel[i] {
				t.Errorf("copied prompt %d model = %q, want %q", i, cp.Model, wantModel[i])
			}
			if cp.Source != "user" {
				t.Errorf("copied prompt %d source = %q, want user", i, cp.Source)
			}
			// step_prompt_id uniqueness: each new prompt resolves to the new blueprint.
			owner, ok, err := store.StepPromptOwner(ctx, orgID, st.StepPromptID)
			if err != nil || !ok || owner != newIDs[0] {
				t.Fatalf("StepPromptOwner(%s) = (%q, %v, %v), want (%s, true, nil)", st.StepPromptID, owner, ok, err, newIDs[0])
			}
		}
		// Originals untouched: source blueprint still has its two original steps.
		srcSteps, err := store.ListSteps(ctx, orgID, srcID)
		if err != nil {
			t.Fatalf("ListSteps(src): %v", err)
		}
		if len(srcSteps) != 2 || srcSteps[0].StepPromptID != srcPrompts[0] || srcSteps[1].StepPromptID != srcPrompts[1] {
			t.Fatalf("source steps mutated: %+v", srcSteps)
		}
	})

	t.Run("NonContiguousSelectionYieldsSeparateBlueprints", func(t *testing.T) {
		store, orgID, teamID, seed, _ := factory(t)
		ctx := context.Background()
		_, p := seedRun(t, store, orgID, teamID, "dup-gap",
			[]domain.Prompt{
				{Name: "P0", Body: "b0", Source: "user"},
				{Name: "P1", Body: "b1", Source: "user"},
				{Name: "P2", Body: "b2", Source: "user"},
			}, seed, nil)

		// Select index 0 and 2 (gap at 1) → two separate 1-step blueprints, no edge.
		newIDs, err := store.DuplicatePrompts(ctx, orgID, teamID, []string{p[0], p[2]})
		if err != nil {
			t.Fatalf("DuplicatePrompts: %v", err)
		}
		if len(newIDs) != 2 {
			t.Fatalf("non-contiguous selection produced %d blueprints, want 2", len(newIDs))
		}
		for _, id := range newIDs {
			steps, err := store.ListSteps(ctx, orgID, id)
			if err != nil {
				t.Fatalf("ListSteps(%s): %v", id, err)
			}
			if len(steps) != 1 || steps[0].StepIndex != 0 {
				t.Fatalf("blueprint %s steps = %+v, want one 0-indexed step (no spurious adjacency)", id, steps)
			}
		}
	})

	t.Run("ContiguousSubRunReDensifies", func(t *testing.T) {
		store, orgID, teamID, seed, getPrompt := factory(t)
		ctx := context.Background()
		_, p := seedRun(t, store, orgID, teamID, "dup-subrun",
			[]domain.Prompt{
				{Name: "P0", Body: "b0", Source: "user"},
				{Name: "P1", Body: "b1", Source: "user"},
				{Name: "P2", Body: "b2", Source: "user"},
				{Name: "P3", Body: "b3", Source: "user"},
			}, seed, []string{"br0", "br1", "br2", "br3"})

		// Sub-run [1..2] → one blueprint re-densified 0-based, partial name = P1.
		newIDs, err := store.DuplicatePrompts(ctx, orgID, teamID, []string{p[1], p[2]})
		if err != nil {
			t.Fatalf("DuplicatePrompts: %v", err)
		}
		if len(newIDs) != 1 {
			t.Fatalf("sub-run produced %d blueprints, want 1", len(newIDs))
		}
		bp, _ := store.Get(ctx, orgID, newIDs[0])
		if bp == nil || bp.Name != "P1" {
			t.Fatalf("partial-run name = %v, want P1 (first copied prompt)", bp)
		}
		steps, err := store.ListSteps(ctx, orgID, newIDs[0])
		if err != nil {
			t.Fatalf("ListSteps: %v", err)
		}
		if len(steps) != 2 || steps[0].StepIndex != 0 || steps[1].StepIndex != 1 {
			t.Fatalf("sub-run steps = %+v, want re-densified [0,1]", steps)
		}
		if steps[0].Brief != "br1" || steps[1].Brief != "br2" {
			t.Errorf("sub-run briefs = [%q,%q], want [br1,br2]", steps[0].Brief, steps[1].Brief)
		}
		if b := getPrompt(t, steps[0].StepPromptID).Body; b != "b1" {
			t.Errorf("sub-run step 0 body = %q, want b1", b)
		}
		if b := getPrompt(t, steps[1].StepPromptID).Body; b != "b2" {
			t.Errorf("sub-run step 1 body = %q, want b2", b)
		}
	})

	t.Run("SystemPromptCopiedAsUser", func(t *testing.T) {
		store, orgID, teamID, seed, getPrompt := factory(t)
		ctx := context.Background()
		_, p := seedRun(t, store, orgID, teamID, "dup-sys",
			[]domain.Prompt{
				{Name: "Shipped", Body: "shipped body", Source: "system", SystemSlug: "system-dup-step"},
			}, seed, nil)

		newIDs, err := store.DuplicatePrompts(ctx, orgID, teamID, []string{p[0]})
		if err != nil {
			t.Fatalf("DuplicatePrompts: %v", err)
		}
		if len(newIDs) != 1 {
			t.Fatalf("produced %d blueprints, want 1", len(newIDs))
		}
		steps, err := store.ListSteps(ctx, orgID, newIDs[0])
		if err != nil || len(steps) != 1 {
			t.Fatalf("ListSteps = (%+v, %v), want one step", steps, err)
		}
		cp := getPrompt(t, steps[0].StepPromptID)
		if cp.Source != "user" {
			t.Errorf("copied prompt source = %q, want user (decoupled from system)", cp.Source)
		}
		if cp.SystemSlug != "" {
			t.Errorf("copied prompt system_slug = %q, want empty", cp.SystemSlug)
		}
		if cp.Body != "shipped body" {
			t.Errorf("copied prompt body = %q, want 'shipped body'", cp.Body)
		}
		// The source prompt is untouched: still a system prompt.
		src := getPrompt(t, p[0])
		if src.Source != "system" {
			t.Errorf("source prompt source = %q, want system (unchanged)", src.Source)
		}
	})

	t.Run("EmptySetRejected", func(t *testing.T) {
		store, orgID, teamID, _, _ := factory(t)
		if _, err := store.DuplicatePrompts(context.Background(), orgID, teamID, nil); err == nil {
			t.Fatal("DuplicatePrompts(nil) returned nil error, want ErrDuplicateNoPrompts")
		}
	})
}
