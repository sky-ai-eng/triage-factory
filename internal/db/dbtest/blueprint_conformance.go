package dbtest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// BlueprintStoreFactory is what a per-backend test file hands to
// RunBlueprintStoreConformance. It returns:
//   - the wired BlueprintStore impl
//   - the orgID to pass to every method (SQLite returns
//     runmode.LocalDefaultOrgID, Postgres a fresh org UUID)
//   - the teamID Create should attribute blueprints to (every blueprint is
//     team-scoped)
//   - a PromptSeeder hook the harness invokes to materialize step prompts. A
//     blueprint_steps row FKs the step prompt on (step_prompt_id, team_id), so
//     ReplaceSteps needs real same-team prompt rows; the backend owns that
//     INSERT against its own schema and returns the prompt id.
type BlueprintStoreFactory func(t *testing.T) (store db.BlueprintStore, orgID, teamID string, seedPrompt PromptSeederForBlueprints)

// PromptSeederForBlueprints seeds one user prompt owned by the conformance
// team and returns its id, so the harness can wire blueprint steps without
// knowing each backend's prompt-insert shape.
type PromptSeederForBlueprints func(t *testing.T, idHint string) string

// BlueprintRunWriteFactory is what a per-backend test file hands to
// RunBlueprintRunWriteConformance. A blueprint_runs row FKs both a blueprint
// and a task, and a task hangs off an entity and an event, so the backend
// seeds that graph its own way and returns the two ids the run needs. Kept
// separate from BlueprintStoreFactory because the rest of the blueprint suite
// never mints a run and would pay for that fixture on every subtest.
type BlueprintRunWriteFactory func(t *testing.T) (store db.BlueprintStore, orgID, blueprintID, taskID string)

// RunBlueprintRunWriteConformance covers the returned-row standard on the
// blueprint_runs writes: CreateRun, SetRunWorktreePathSystem and
// SetRunCurrentStepSystem each hand back the row they persisted, and the two
// id-keyed stamps refuse an id no run answers to.
func RunBlueprintRunWriteConformance(t *testing.T, mk BlueprintRunWriteFactory) {
	t.Helper()

	t.Run("every_single_row_run_write_returns_the_stored_row", func(t *testing.T) {
		store, orgID, blueprintID, taskID := mk(t)
		ctx := context.Background()
		readRun := func(id string) func() (*domain.BlueprintRun, error) {
			return func() (*domain.BlueprintRun, error) { return store.GetRun(ctx, orgID, id) }
		}

		run, err := store.CreateRun(ctx, orgID, domain.BlueprintRun{
			BlueprintID: blueprintID, TaskID: taskID,
			TriggerType: domain.BlueprintTriggerManual,
		})
		if err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "CreateRun", run, readRun(run.ID))
		// The id and started_at are the statement's, not the caller's — which
		// is the whole reason a caller that supplied neither can still act on
		// the run it just created.
		if run.ID == "" || run.StartedAt.IsZero() || run.Status != domain.BlueprintRunStatusRunning {
			t.Errorf("CreateRun returned a row missing what only the row knows: %+v", run)
		}

		pathed, err := store.SetRunWorktreePathSystem(ctx, orgID, run.ID, "/tmp/ret-wt")
		if err != nil {
			t.Fatalf("SetRunWorktreePathSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetRunWorktreePathSystem", pathed, readRun(run.ID))
		if pathed.WorktreePath != "/tmp/ret-wt" {
			t.Errorf("SetRunWorktreePathSystem returned worktree_path %q, want the stamped one", pathed.WorktreePath)
		}

		stepped, err := store.SetRunCurrentStepSystem(ctx, orgID, run.ID, 2)
		if err != nil {
			t.Fatalf("SetRunCurrentStepSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "SetRunCurrentStepSystem", stepped, readRun(run.ID))
		if stepped.CurrentStepIndex != 2 || stepped.WorktreePath != "/tmp/ret-wt" {
			t.Errorf("SetRunCurrentStepSystem returned %+v, want step 2 with the earlier stamp intact", stepped)
		}

		missing := uuid.New().String()
		if _, err := store.SetRunWorktreePathSystem(ctx, orgID, missing, "/tmp/x"); !errors.Is(err, db.ErrNoSuchBlueprintRun) {
			t.Errorf("SetRunWorktreePathSystem on a missing id: got %v, want db.ErrNoSuchBlueprintRun", err)
		}
		if _, err := store.SetRunCurrentStepSystem(ctx, orgID, missing, 1); !errors.Is(err, db.ErrNoSuchBlueprintRun) {
			t.Errorf("SetRunCurrentStepSystem on a missing id: got %v, want db.ErrNoSuchBlueprintRun", err)
		}
	})
}

// RunBlueprintStoreConformance runs the shared CRUD + step round-trip +
// composition suite against any db.BlueprintStore impl:
//
//   - List returns a created blueprint.
//   - ReplaceSteps then ListSteps round-trips the ordered step list.
//   - Merge / split / delete-step composition and the user_modified stamping
//     contract.
//
// Shipped-content seeding (system_slug, NULL creator) and its boot-time sync
// live in RunShippedSyncConformance; these tests use plain Create because they
// only need a blueprint to exist, not the shipped shape.
func RunBlueprintStoreConformance(t *testing.T, factory BlueprintStoreFactory) {
	t.Helper()

	t.Run("every_single_row_write_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard applied to each converted method in turn.
		// The property is one line — what the write handed back is what a point
		// read finds — and AssertWriteReturnedStoredRow's doc covers what that
		// stands in for (RETURNING semantics, RLS visibility on the update arm,
		// column-list drift).
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		readBlueprint := func(id string) func() (*domain.Blueprint, error) {
			return func() (*domain.Blueprint, error) { return store.Get(ctx, orgID, id) }
		}

		id := uuid.New().String()
		created, err := store.Create(ctx, orgID, teamID, domain.Blueprint{
			ID: id, Name: "Returned", Source: "user",
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Create", created, readBlueprint(id))
		if created.UsageCount != 0 || created.TeamID == "" || created.CreatedAt.IsZero() {
			t.Errorf("Create returned a row missing what only the row knows: %+v", created)
		}

		renamed, err := store.Rename(ctx, orgID, id, "Returned v2")
		if err != nil {
			t.Fatalf("Rename: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Rename", renamed, readBlueprint(id))
		if renamed.Name != "Returned v2" || !renamed.UserModified {
			t.Errorf("Rename returned %+v, want the new name and user_modified stamped", renamed)
		}

		// ReplaceSteps writes a set and stamps the parent; the parent stamp is
		// the single row, and its user_modified is a fact only the statement
		// knows.
		stamped, err := store.ReplaceSteps(ctx, orgID, id, []string{seedPrompt(t, "ret-step")}, nil)
		if err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "ReplaceSteps", stamped, readBlueprint(id))
		if !stamped.UserModified {
			t.Error("ReplaceSteps returned a row without user_modified stamped")
		}

		if _, err := store.Rename(ctx, orgID, uuid.New().String(), "ghost"); !errors.Is(err, db.ErrNoSuchBlueprint) {
			t.Errorf("Rename on a missing id: got %v, want db.ErrNoSuchBlueprint", err)
		}
		if _, err := store.ReplaceSteps(ctx, orgID, uuid.New().String(), nil, nil); !errors.Is(err, db.ErrNoSuchBlueprint) {
			t.Errorf("ReplaceSteps on a missing id: got %v, want db.ErrNoSuchBlueprint", err)
		}
	})

	t.Run("List_ReturnsCreated", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id := seedBlueprint(t, store, orgID, teamID, "Listed")
		list, total, err := store.List(ctx, orgID, db.BlueprintListFilter{TeamID: teamID}, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if total != len(list) {
			t.Errorf("total = %d but the unwindowed page held %d rows", total, len(list))
		}
		found := false
		for _, b := range list {
			if b.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("List did not return the created blueprint %s", id)
		}
	})

	t.Run("ReplaceSteps_ListSteps_RoundTrip", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bpID := seedBlueprint(t, store, orgID, teamID, "Steps")
		p1 := seedPrompt(t, "step-1")
		p2 := seedPrompt(t, "step-2")
		if _, err := store.ReplaceSteps(ctx, orgID, bpID, []string{p1, p2}, []string{"first", "second"}); err != nil {
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
		if _, err := store.ReplaceSteps(ctx, orgID, bpID, []string{p2}, nil); err != nil {
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
		bp1 := seedBlueprint(t, store, orgID, teamID, "BP1")
		bp2 := seedBlueprint(t, store, orgID, teamID, "BP2")
		p := seedPrompt(t, "shared")
		if _, err := store.ReplaceSteps(ctx, orgID, bp1, []string{p}, nil); err != nil {
			t.Fatalf("ReplaceSteps bp1: %v", err)
		}
		if _, err := store.ReplaceSteps(ctx, orgID, bp2, []string{p}, nil); err == nil {
			t.Fatalf("ReplaceSteps bp2 with an already-owned prompt succeeded; want unique-constraint error")
		}
	})

	t.Run("StepPromptOwner_ResolvesOwningBlueprint", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp := seedBlueprint(t, store, orgID, teamID, "Owner")
		p := seedPrompt(t, "owned")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p}, nil); err != nil {
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
		bp := seedBlueprint(t, store, orgID, teamID, "Soft")
		if err := store.Delete(ctx, orgID, bp); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		// Request-facing Get + List hide it.
		if got, err := store.Get(ctx, orgID, bp); err != nil || got != nil {
			t.Fatalf("Get after soft-delete = (%v, %v); want (nil, nil)", got, err)
		}
		list, total, err := store.List(ctx, orgID, db.BlueprintListFilter{TeamID: teamID}, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, b := range list {
			if b.ID == bp {
				t.Fatalf("soft-deleted blueprint leaked into List")
			}
		}
		if total != len(list) {
			t.Errorf("total = %d but the page held %d rows; a hidden row must not be counted", total, len(list))
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
		bp := seedBlueprint(t, store, orgID, teamID, "SoftDelOwner")
		p := seedPrompt(t, "paired-prompt")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p}, nil); err != nil {
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
		id := seedBlueprint(t, store, orgID, teamID, "Before")
		if _, err := store.Rename(ctx, orgID, id, "After"); err != nil {
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
		bpA := seedBlueprint(t, store, orgID, teamID, "A")
		bpB := seedBlueprint(t, store, orgID, teamID, "B")
		bpDel := seedBlueprint(t, store, orgID, teamID, "Del")
		a1, a2 := seedPrompt(t, "all-a1"), seedPrompt(t, "all-a2")
		b1 := seedPrompt(t, "all-b1")
		d1 := seedPrompt(t, "all-d1")
		if _, err := store.ReplaceSteps(ctx, orgID, bpA, []string{a1, a2}, nil); err != nil {
			t.Fatalf("steps A: %v", err)
		}
		if _, err := store.ReplaceSteps(ctx, orgID, bpB, []string{b1}, nil); err != nil {
			t.Fatalf("steps B: %v", err)
		}
		if _, err := store.ReplaceSteps(ctx, orgID, bpDel, []string{d1}, nil); err != nil {
			t.Fatalf("steps Del: %v", err)
		}
		// Soft-deleted blueprints' steps must be excluded from the bulk read.
		if err := store.Delete(ctx, orgID, bpDel); err != nil {
			t.Fatalf("delete Del: %v", err)
		}

		all, total, err := store.ListAllSteps(ctx, orgID, db.BlueprintStepListFilter{TeamID: teamID}, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("ListAllSteps: %v", err)
		}
		if total != len(all) {
			t.Errorf("total = %d but the page held %d steps", total, len(all))
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

		// The same method narrowed to one blueprint IS the per-blueprint read
		// — that is why there is no second route for it.
		scoped, scopedTotal, err := store.ListAllSteps(ctx, orgID,
			db.BlueprintStepListFilter{TeamID: teamID, BlueprintIDs: []string{bpB}}, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("ListAllSteps(bpB): %v", err)
		}
		if len(scoped) != 1 || scopedTotal != 1 || scoped[0].StepPromptID != b1 {
			t.Errorf("ListAllSteps(bpB) = %+v (total %d), want just %s", scoped, scopedTotal, b1)
		}

		// And the pages partition the bulk read.
		firstStep, stepTotal, err := store.ListAllSteps(ctx, orgID,
			db.BlueprintStepListFilter{TeamID: teamID}, db.ListOpts{Limit: 1})
		if err != nil {
			t.Fatalf("ListAllSteps page 1: %v", err)
		}
		secondStep, _, err := store.ListAllSteps(ctx, orgID,
			db.BlueprintStepListFilter{TeamID: teamID}, db.ListOpts{Limit: 1, Offset: 1})
		if err != nil {
			t.Fatalf("ListAllSteps page 2: %v", err)
		}
		if len(firstStep) != 1 || len(secondStep) != 1 || stepTotal != total {
			t.Fatalf("paged steps = %d + %d (total %d), want 1 + 1 with total %d",
				len(firstStep), len(secondStep), stepTotal, total)
		}
		if firstStep[0] == secondStep[0] {
			t.Errorf("both step pages returned %+v; pages must partition the set", firstStep[0])
		}
	})

	t.Run("MergeInto_AppendsSourceStepsAndRetiresSource", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		host := seedBlueprint(t, store, orgID, teamID, "Host")
		source := seedBlueprint(t, store, orgID, teamID, "Source")
		h1, h2 := seedPrompt(t, "host-1"), seedPrompt(t, "host-2")
		s1, s2 := seedPrompt(t, "src-1"), seedPrompt(t, "src-2")
		if _, err := store.ReplaceSteps(ctx, orgID, host, []string{h1, h2}, []string{"h1", "h2"}); err != nil {
			t.Fatalf("ReplaceSteps host: %v", err)
		}
		if _, err := store.ReplaceSteps(ctx, orgID, source, []string{s1, s2}, []string{"s1", "s2"}); err != nil {
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
		bp := seedBlueprint(t, store, orgID, teamID, "Split")
		p0, p1, p2, p3 := seedPrompt(t, "sp-0"), seedPrompt(t, "sp-1"), seedPrompt(t, "sp-2"), seedPrompt(t, "sp-3")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1, p2, p3}, []string{"b0", "b1", "b2", "b3"}); err != nil {
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
		bp := seedBlueprint(t, store, orgID, teamID, "Tail")
		p0, p1, p2 := seedPrompt(t, "dt-0"), seedPrompt(t, "dt-1"), seedPrompt(t, "dt-2")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1, p2}, nil); err != nil {
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
		bp := seedBlueprint(t, store, orgID, teamID, "Head")
		p0, p1, p2 := seedPrompt(t, "dh-0"), seedPrompt(t, "dh-1"), seedPrompt(t, "dh-2")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1, p2}, nil); err != nil {
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
		bp := seedBlueprint(t, store, orgID, teamID, "Mid")
		p0, p1, p2, p3 := seedPrompt(t, "dm-0"), seedPrompt(t, "dm-1"), seedPrompt(t, "dm-2"), seedPrompt(t, "dm-3")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1, p2, p3}, nil); err != nil {
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
		bp := seedBlueprint(t, store, orgID, teamID, "MinHead")
		p0, p1 := seedPrompt(t, "dmh-0"), seedPrompt(t, "dmh-1")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1}, nil); err != nil {
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
		bp := seedBlueprint(t, store, orgID, teamID, "MinTail")
		p0, p1 := seedPrompt(t, "dmt-0"), seedPrompt(t, "dmt-1")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1}, nil); err != nil {
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
		bp := seedBlueprint(t, store, orgID, teamID, "Single")
		p := seedPrompt(t, "ds-0")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		// The sole-owner 1-step case is the handler's pair-delete path, not
		// DeleteStep — the store guards against being misused for it.
		if _, err := store.DeleteStep(ctx, orgID, bp, 0, ""); err == nil {
			t.Fatalf("DeleteStep on a 1-step blueprint returned nil error; want a guard error")
		}
	})

	t.Run("Create_CtxCancellation_FailsFast", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := store.Create(ctx, orgID, teamID, domain.Blueprint{
			ID: uuid.New().String(), Name: "Ctx", Source: "user",
		}); err == nil {
			t.Fatalf("Create with cancelled ctx returned nil error")
		}
	})

	// --- user_modified stamping ------------------------------------------
	//
	// The boot-time shipped-defaults sync treats user_modified as "this team
	// edited its copy, never overwrite it." These pin the contract documented on
	// db.BlueprintStore: every structural edit stamps true on the affected
	// surviving row(s); Create inserts false; usage bookkeeping never touches the
	// flag.

	t.Run("Create_LeavesUserModifiedFalse", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id := uuid.New().String()
		if _, err := store.Create(ctx, orgID, teamID, domain.Blueprint{ID: id, Name: "Created", Source: "user"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get after Create = (%v, %v), want a row", got, err)
		}
		if got.UserModified {
			t.Errorf("Created blueprint has user_modified=true, want false")
		}
	})

	t.Run("Rename_StampsUserModified", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id := seedBlueprint(t, store, orgID, teamID, "Before")
		if _, err := store.Rename(ctx, orgID, id, "After"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get after rename = (%v, %v)", got, err)
		}
		if !got.UserModified {
			t.Errorf("Rename did not stamp user_modified true")
		}
	})

	t.Run("ReplaceSteps_StampsUserModified", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		id := seedBlueprint(t, store, orgID, teamID, "StepsUM")
		p := seedPrompt(t, "um-steps-p")
		if _, err := store.ReplaceSteps(ctx, orgID, id, []string{p}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get after ReplaceSteps = (%v, %v)", got, err)
		}
		if !got.UserModified {
			t.Errorf("ReplaceSteps did not stamp user_modified true")
		}
	})

	t.Run("MergeInto_StampsHostNotSource", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		host := seedBlueprint(t, store, orgID, teamID, "Host")
		source := seedBlueprint(t, store, orgID, teamID, "Source")
		s1 := seedPrompt(t, "um-merge-s1")
		if _, err := store.ReplaceSteps(ctx, orgID, source, []string{s1}, nil); err != nil {
			t.Fatalf("ReplaceSteps source: %v", err)
		}
		// Host is freshly seeded with no steps of its own — user_modified starts
		// false, so a flip after MergeInto is attributable to MergeInto itself
		// (not to some prior ReplaceSteps on the host).
		before, err := store.Get(ctx, orgID, host)
		if err != nil || before == nil || before.UserModified {
			t.Fatalf("pre-merge host = (%+v, %v), want a fresh unmodified row", before, err)
		}
		if err := store.MergeInto(ctx, orgID, host, source); err != nil {
			t.Fatalf("MergeInto: %v", err)
		}
		gotHost, err := store.Get(ctx, orgID, host)
		if err != nil || gotHost == nil {
			t.Fatalf("Get(host) after merge = (%v, %v)", gotHost, err)
		}
		if !gotHost.UserModified {
			t.Errorf("MergeInto did not stamp host user_modified true")
		}
	})

	t.Run("SplitAt_DownstreamBornModifiedWithNoSystemSlug", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp := seedBlueprint(t, store, orgID, teamID, "SplitUM")
		p0, p1 := seedPrompt(t, "um-split-0"), seedPrompt(t, "um-split-1")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		newID := uuid.New().String()
		if _, err := store.SplitAt(ctx, orgID, bp, 1, newID, "Downstream"); err != nil {
			t.Fatalf("SplitAt: %v", err)
		}
		up, err := store.Get(ctx, orgID, bp)
		if err != nil || up == nil || !up.UserModified {
			t.Fatalf("upstream after split = (%+v, %v), want user_modified=true", up, err)
		}
		down, err := store.Get(ctx, orgID, newID)
		if err != nil || down == nil {
			t.Fatalf("Get(downstream) = (%v, %v), want a row", down, err)
		}
		if !down.UserModified {
			t.Errorf("downstream blueprint user_modified = false, want true (minted already-diverged)")
		}
		if down.SystemSlug != "" {
			t.Errorf("downstream blueprint system_slug = %q, want empty (a split product is never a shipped row)", down.SystemSlug)
		}
	})

	t.Run("DeleteStep_StampsUserModified_Head", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp := seedBlueprint(t, store, orgID, teamID, "HeadUM")
		p0, p1 := seedPrompt(t, "um-dh-0"), seedPrompt(t, "um-dh-1")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		down, err := store.DeleteStep(ctx, orgID, bp, 0, "Downstream")
		if err != nil {
			t.Fatalf("DeleteStep: %v", err)
		}
		// The retired original is soft-deleted — GetSystem still resolves it.
		orig, err := store.GetSystem(ctx, orgID, bp)
		if err != nil || orig == nil {
			t.Fatalf("GetSystem(original) = (%v, %v), want a row", orig, err)
		}
		if !orig.UserModified {
			t.Errorf("retired original user_modified = false, want true")
		}
		downBP, err := store.Get(ctx, orgID, down)
		if err != nil || downBP == nil {
			t.Fatalf("Get(downstream) = (%v, %v), want a row", downBP, err)
		}
		if !downBP.UserModified {
			t.Errorf("downstream user_modified = false, want true")
		}
		if downBP.SystemSlug != "" {
			t.Errorf("downstream system_slug = %q, want empty", downBP.SystemSlug)
		}
	})

	t.Run("DeleteStep_StampsUserModified_Tail", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp := seedBlueprint(t, store, orgID, teamID, "TailUM")
		p0, p1 := seedPrompt(t, "um-dt-0"), seedPrompt(t, "um-dt-1")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		down, err := store.DeleteStep(ctx, orgID, bp, 1, "")
		if err != nil {
			t.Fatalf("DeleteStep: %v", err)
		}
		if down != "" {
			t.Fatalf("tail delete minted a downstream blueprint %q; want none", down)
		}
		orig, err := store.Get(ctx, orgID, bp)
		if err != nil || orig == nil {
			t.Fatalf("Get(original) = (%v, %v), want a row", orig, err)
		}
		if !orig.UserModified {
			t.Errorf("original user_modified = false, want true")
		}
	})

	t.Run("DeleteStep_StampsUserModified_Mid", func(t *testing.T) {
		store, orgID, teamID, seedPrompt := factory(t)
		ctx := context.Background()
		bp := seedBlueprint(t, store, orgID, teamID, "MidUM")
		p0, p1, p2 := seedPrompt(t, "um-dm-0"), seedPrompt(t, "um-dm-1"), seedPrompt(t, "um-dm-2")
		if _, err := store.ReplaceSteps(ctx, orgID, bp, []string{p0, p1, p2}, nil); err != nil {
			t.Fatalf("ReplaceSteps: %v", err)
		}
		down, err := store.DeleteStep(ctx, orgID, bp, 1, "Downstream")
		if err != nil {
			t.Fatalf("DeleteStep: %v", err)
		}
		if down == "" {
			t.Fatalf("mid delete minted no downstream blueprint; want one")
		}
		orig, err := store.Get(ctx, orgID, bp)
		if err != nil || orig == nil || !orig.UserModified {
			t.Fatalf("original after mid delete = (%+v, %v), want user_modified=true", orig, err)
		}
		downBP, err := store.Get(ctx, orgID, down)
		if err != nil || downBP == nil {
			t.Fatalf("Get(downstream) = (%v, %v), want a row", downBP, err)
		}
		if !downBP.UserModified {
			t.Errorf("downstream user_modified = false, want true")
		}
		if downBP.SystemSlug != "" {
			t.Errorf("downstream system_slug = %q, want empty", downBP.SystemSlug)
		}
	})

	t.Run("IncrementUsage_LeavesUserModifiedUntouched", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		id := seedBlueprint(t, store, orgID, teamID, "UsageUM")
		if err := store.IncrementUsage(ctx, orgID, id); err != nil {
			t.Fatalf("IncrementUsage: %v", err)
		}
		got, err := store.Get(ctx, orgID, id)
		if err != nil || got == nil {
			t.Fatalf("Get = (%v, %v)", got, err)
		}
		if got.UserModified {
			t.Errorf("IncrementUsage flipped user_modified true; want untouched (false)")
		}
		if got.UsageCount != 1 {
			t.Errorf("UsageCount = %d, want 1", got.UsageCount)
		}
	})
}

// seedBlueprint creates a user-source blueprint owned by the conformance team
// and returns its id. It replaces the removed BlueprintStore.SeedOrUpdate as
// the fixture path: the CRUD / step / composition tests below only need a
// blueprint to exist, not the shipped system-slug/NULL-creator shape (that
// path is covered by RunShippedSyncConformance).
func seedBlueprint(t *testing.T, store db.BlueprintStore, orgID, teamID, name string) string {
	t.Helper()
	id := uuid.New().String()
	if _, err := store.Create(context.Background(), orgID, teamID, domain.Blueprint{
		ID: id, Name: name, Source: "user",
	}); err != nil {
		t.Fatalf("seed blueprint %q: %v", name, err)
	}
	return id
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

	// seedConversation creates a source blueprint with the given prompts as ordered steps
	// (briefs[i] paired positionally) and returns the blueprint id + prompt ids.
	seedConversation := func(t *testing.T, store db.BlueprintStore, orgID, teamID, slug string, prompts []domain.Prompt, seed DuplicationPromptSeeder, briefs []string) (string, []string) {
		t.Helper()
		ctx := context.Background()
		bpID := seedBlueprint(t, store, orgID, teamID, slug)
		ids := make([]string, len(prompts))
		for i, p := range prompts {
			ids[i] = seed(t, p)
		}
		if _, err := store.ReplaceSteps(ctx, orgID, bpID, ids, briefs); err != nil {
			t.Fatalf("ReplaceSteps %s: %v", slug, err)
		}
		return bpID, ids
	}

	t.Run("DuplicateFullBlueprint", func(t *testing.T) {
		store, orgID, teamID, seed, getPrompt := factory(t)
		ctx := context.Background()
		srcID, srcPrompts := seedConversation(t, store, orgID, teamID, "dup-full",
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
		if bp.UserModified {
			t.Errorf("duplicated blueprint user_modified = true, want false (a fresh user-source copy hasn't diverged from anything)")
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
		// seedConversation's ReplaceSteps already stamped the source user_modified=true;
		// DuplicatePrompts must leave that alone (it never touches the source).
		src, err := store.Get(ctx, orgID, srcID)
		if err != nil || src == nil || !src.UserModified {
			t.Fatalf("source after duplicate = (%+v, %v), want user_modified=true (untouched)", src, err)
		}
	})

	t.Run("NonContiguousSelectionYieldsSeparateBlueprints", func(t *testing.T) {
		store, orgID, teamID, seed, _ := factory(t)
		ctx := context.Background()
		_, p := seedConversation(t, store, orgID, teamID, "dup-gap",
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
		_, p := seedConversation(t, store, orgID, teamID, "dup-subrun",
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
		_, p := seedConversation(t, store, orgID, teamID, "dup-sys",
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
