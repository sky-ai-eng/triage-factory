package db_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestOrgTemplate_UpdateHandler_MatchedSemantics pins the conditional-update
// contract that closes the PATCH-vs-promote race (SKY-381): UpdateHandler's
// WHERE pins the row's kind, so it reports matched=false — rather than
// silently no-op'ing — when the row was deleted or promoted (rule→trigger)
// since the caller read it. The handler relies on this to 404/409 instead of
// returning a misleading 200. Correct under READ COMMITTED on both dialects
// (a single conditional UPDATE re-checks its WHERE under a row lock).
func TestOrgTemplate_UpdateHandler_MatchedSemantics(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	org := runmode.LocalDefaultOrg

	if err := db.BootstrapNewOrg(ctx, stores, org, runmode.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	rules, err := stores.OrgTemplate.ListHandlers(ctx, org, domain.EventHandlerKindRule)
	if err != nil || len(rules) == 0 {
		t.Fatalf("ListHandlers(rule): err=%v n=%d", err, len(rules))
	}
	rule := rules[0]

	// A matching update reports matched=true.
	matched, err := stores.OrgTemplate.UpdateHandler(ctx, org, rule)
	if err != nil {
		t.Fatalf("UpdateHandler (matching rule): %v", err)
	}
	if !matched {
		t.Error("UpdateHandler on an existing rule reported matched=false; want true")
	}

	// A non-existent id reports matched=false, not an error.
	ghost := rule
	ghost.ID = "00000000-0000-0000-0000-0000000000ff"
	matched, err = stores.OrgTemplate.UpdateHandler(ctx, org, ghost)
	if err != nil {
		t.Fatalf("UpdateHandler (ghost id): %v", err)
	}
	if matched {
		t.Error("UpdateHandler on a non-existent id reported matched=true; want false")
	}

	// TOCTTOU: promote the rule to a trigger, then a stale rule-shaped update
	// must report matched=false (its kind-pinned WHERE no longer matches)
	// rather than silently clobbering the now-trigger row.
	//
	// Promote onto a fresh, trigger-less template blueprint: the shipped
	// blueprints are already 1:1 with shipped triggers, and a template blueprint
	// is fired by exactly one event (org_template_handlers_one_trigger_per_blueprint).
	if err := stores.OrgTemplate.CreateBlueprint(ctx, org, domain.Blueprint{
		ID: "promote-target-bp", SystemSlug: "promote-target-slug", Name: "Promote Target", Source: "user",
	}); err != nil {
		t.Fatalf("CreateBlueprint (promote target): %v", err)
	}
	bt := 3
	minA := 0.0
	if err := stores.OrgTemplate.PromoteHandler(ctx, org, rule.ID, domain.EventHandler{
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "promote-target-bp",
		BreakerThreshold:       &bt,
		MinAutonomySuitability: &minA,
	}); err != nil {
		t.Fatalf("PromoteHandler: %v", err)
	}
	matched, err = stores.OrgTemplate.UpdateHandler(ctx, org, rule) // stale rule shape
	if err != nil {
		t.Fatalf("UpdateHandler (stale rule after promote): %v", err)
	}
	if matched {
		t.Error("stale rule-shaped UpdateHandler after a concurrent promote reported matched=true; want false")
	}
}

// TestOrgTemplate_MaterializesMultiStepPRReview pins that the shipped PR-review
// blueprint — the one multi-step shipped blueprint — materializes
// into a team as three ordered steps (security → correctness → aggregate), with
// the aggregator carrying its per-step haiku model override and the two reviewer
// steps inheriting (empty model). It is the first shipped blueprint with N>1
// steps, so it's the first to exercise the org-template multi-step copy path
// (SeedFromShipped → MaterializeIntoTeam) end to end; a regression that collapsed
// it back to one step would otherwise ship silently.
func TestOrgTemplate_MaterializesMultiStepPRReview(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	org := runmode.LocalDefaultOrg
	team := runmode.LocalDefaultTeamID

	if err := db.BootstrapNewOrg(ctx, stores, org, team, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	bp, err := stores.Blueprints.GetBySystemSlug(ctx, org, team, "system-pr-review")
	if err != nil || bp == nil {
		t.Fatalf("GetBySystemSlug(system-pr-review): bp=%v err=%v", bp, err)
	}
	steps, err := stores.Blueprints.ListSteps(ctx, org, bp.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}

	want := []struct{ slug, model string }{
		{"system-pr-review-security", ""},
		{"system-pr-review-correctness", ""},
		{"system-pr-review-aggregate", "haiku"},
	}
	if len(steps) != len(want) {
		t.Fatalf("materialized PR-review blueprint has %d steps; want %d (security, correctness, aggregate)", len(steps), len(want))
	}
	for i, w := range want {
		if steps[i].StepIndex != i {
			t.Errorf("step %d has step_index %d; want %d (steps must be densely 0..N)", i, steps[i].StepIndex, i)
		}
		p, err := stores.Prompts.Get(ctx, org, steps[i].StepPromptID)
		if err != nil || p == nil {
			t.Fatalf("Get step %d prompt: p=%v err=%v", i, p, err)
		}
		if p.SystemSlug != w.slug {
			t.Errorf("step %d prompt slug = %q; want %q (wrong order or wrong prompt)", i, p.SystemSlug, w.slug)
		}
		if p.Model != w.model {
			t.Errorf("step %d (%s) model = %q; want %q", i, w.slug, p.Model, w.model)
		}
	}
}

// TestOrgTemplate_CopyOnly_OwnerAndUniqueConstraint pins the template mirror of
// the copy-only invariant: a template prompt is a step in at most one template
// blueprint (the unique index refuses a second), and BlueprintStepPromptOwner
// resolves the single owner (ok=false for an un-stepped prompt). This is the
// store-level backing for the template steps-put 422 + prompt-delete pairing.
func TestOrgTemplate_CopyOnly_OwnerAndUniqueConstraint(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	org := runmode.LocalDefaultOrg
	if err := db.BootstrapNewOrg(ctx, stores, org, runmode.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}
	if err := stores.OrgTemplate.CreatePrompt(ctx, org, domain.Prompt{ID: "co-p", SystemSlug: "co-step", Name: "P", Body: "b", Source: "user"}); err != nil {
		t.Fatalf("CreatePrompt: %v", err)
	}
	if err := stores.OrgTemplate.CreateBlueprint(ctx, org, domain.Blueprint{ID: "co-bp1", SystemSlug: "co-bp1-slug", Name: "BP1", Source: "user"}); err != nil {
		t.Fatalf("CreateBlueprint bp1: %v", err)
	}
	if err := stores.OrgTemplate.CreateBlueprint(ctx, org, domain.Blueprint{ID: "co-bp2", SystemSlug: "co-bp2-slug", Name: "BP2", Source: "user"}); err != nil {
		t.Fatalf("CreateBlueprint bp2: %v", err)
	}
	if err := stores.OrgTemplate.ReplaceBlueprintSteps(ctx, org, "co-bp1", []string{"co-p"}, nil); err != nil {
		t.Fatalf("ReplaceBlueprintSteps bp1: %v", err)
	}
	owner, ok, err := stores.OrgTemplate.BlueprintStepPromptOwner(ctx, org, "co-p")
	if err != nil || !ok || owner != "co-bp1" {
		t.Fatalf("BlueprintStepPromptOwner = (%q, %v, %v); want (co-bp1, true, nil)", owner, ok, err)
	}
	if err := stores.OrgTemplate.ReplaceBlueprintSteps(ctx, org, "co-bp2", []string{"co-p"}, nil); err == nil {
		t.Fatal("ReplaceBlueprintSteps reused a template prompt in a 2nd blueprint; want unique-constraint error")
	}
	if err := stores.OrgTemplate.CreatePrompt(ctx, org, domain.Prompt{ID: "co-orphan", SystemSlug: "co-orphan-step", Name: "O", Body: "b", Source: "user"}); err != nil {
		t.Fatalf("CreatePrompt orphan: %v", err)
	}
	if _, ok, err := stores.OrgTemplate.BlueprintStepPromptOwner(ctx, org, "co-orphan"); err != nil || ok {
		t.Fatalf("BlueprintStepPromptOwner(orphan) = (_, %v, %v); want (_, false, nil)", ok, err)
	}
}

// TestOrgTemplate_MultiStepBlueprint_DeepCopy is the multi-step authoring
// acceptance pin: an org admin authors a multi-step template blueprint, and a
// team materialized from that template gets a REAL multi-step team blueprint
// (deep-copied, steps re-pointed at the team's prompt copies) — not a 1-step
// synthesized wrapper.
// It also pins that the materialized team trigger fires the team's blueprint
// copy (clean blueprint_id, no prompt_id-in-blueprint_id muddiness) and that
// the shipped 1-step blueprints still deep-copy with a single step.
func TestOrgTemplate_MultiStepBlueprint_DeepCopy(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	org := runmode.LocalDefaultOrg

	// Org-create seeds the template (prompts + blueprints + handlers) and
	// materializes the founder's team from it.
	if err := db.BootstrapNewOrg(ctx, stores, org, runmode.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	// --- Author a 2-step template blueprint (the new authoring capability) ---
	if err := stores.OrgTemplate.CreatePrompt(ctx, org, domain.Prompt{
		ID: "tmpl-prompt-a", SystemSlug: "tmpl-step-a", Name: "Step A", Body: "map the surface", Source: "user",
	}); err != nil {
		t.Fatalf("CreatePrompt A: %v", err)
	}
	if err := stores.OrgTemplate.CreatePrompt(ctx, org, domain.Prompt{
		ID: "tmpl-prompt-b", SystemSlug: "tmpl-step-b", Name: "Step B", Body: "write the review", Source: "user",
	}); err != nil {
		t.Fatalf("CreatePrompt B: %v", err)
	}
	const multiSlug = "tmpl-multi"
	if err := stores.OrgTemplate.CreateBlueprint(ctx, org, domain.Blueprint{
		ID: "tmpl-multi-id", SystemSlug: multiSlug, Name: "Two-step review", Source: "user",
	}); err != nil {
		t.Fatalf("CreateBlueprint: %v", err)
	}
	if err := stores.OrgTemplate.ReplaceBlueprintSteps(ctx, org, "tmpl-multi-id",
		[]string{"tmpl-prompt-a", "tmpl-prompt-b"}, []string{"do a", "do b"}); err != nil {
		t.Fatalf("ReplaceBlueprintSteps: %v", err)
	}

	// The store counts blueprint-step references so the prompt-delete handler
	// can 409 instead of letting the RESTRICT FK surface as a 500; a prompt
	// still used as a step can't be deleted.
	if refs, err := stores.OrgTemplate.CountBlueprintStepReferences(ctx, org, "tmpl-prompt-a"); err != nil || refs != 1 {
		t.Fatalf("CountBlueprintStepReferences(tmpl-prompt-a) = %d, err=%v; want 1", refs, err)
	}
	if err := stores.OrgTemplate.DeletePrompt(ctx, org, "tmpl-prompt-a"); err == nil {
		t.Error("DeletePrompt on a prompt used as a blueprint step succeeded; want a RESTRICT FK error")
	}

	// Wire a template trigger at the multi-step blueprint by promoting a rule.
	rules, err := stores.OrgTemplate.ListHandlers(ctx, org, domain.EventHandlerKindRule)
	if err != nil || len(rules) == 0 {
		t.Fatalf("ListHandlers(rule): err=%v n=%d", err, len(rules))
	}
	triggerSlug := rules[0].SystemSlug
	bt := 3
	minA := 0.0
	if err := stores.OrgTemplate.PromoteHandler(ctx, org, rules[0].ID, domain.EventHandler{
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            "tmpl-multi-id",
		BreakerThreshold:       &bt,
		MinAutonomySuitability: &minA,
	}); err != nil {
		t.Fatalf("PromoteHandler → multi-step blueprint: %v", err)
	}

	// --- Materialize a NEW team from the edited template ---
	const newTeamID = "00000000-0000-0000-0000-0000000000d4"
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO teams (id, org_id, slug, name) VALUES (?, ?, 'delta', 'Delta')`, newTeamID, org,
	); err != nil {
		t.Fatalf("insert team: %v", err)
	}
	if err := db.BootstrapNewTeam(ctx, stores, org, newTeamID); err != nil {
		t.Fatalf("BootstrapNewTeam: %v", err)
	}

	// (1) The new team has a REAL multi-step blueprint copy.
	teamBP, err := stores.Blueprints.GetBySystemSlug(ctx, org, newTeamID, multiSlug)
	if err != nil || teamBP == nil {
		t.Fatalf("team blueprint %q missing after materialize: err=%v", multiSlug, err)
	}
	steps, err := stores.Blueprints.ListSteps(ctx, org, teamBP.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("team multi-step blueprint has %d steps; want 2 (a deep-copy, not a 1-step synthesized wrapper)", len(steps))
	}
	// Steps re-point at the team's OWN prompt copies, in order.
	teamA, err := stores.Prompts.GetBySystemSlug(ctx, org, newTeamID, "tmpl-step-a")
	if err != nil || teamA == nil {
		t.Fatalf("team prompt tmpl-step-a missing: err=%v", err)
	}
	teamB, err := stores.Prompts.GetBySystemSlug(ctx, org, newTeamID, "tmpl-step-b")
	if err != nil || teamB == nil {
		t.Fatalf("team prompt tmpl-step-b missing: err=%v", err)
	}
	if steps[0].StepPromptID != teamA.ID || steps[0].Brief != "do a" {
		t.Errorf("step 0 = {prompt:%s brief:%q}; want {prompt:%s brief:%q}", steps[0].StepPromptID, steps[0].Brief, teamA.ID, "do a")
	}
	if steps[1].StepPromptID != teamB.ID || steps[1].Brief != "do b" {
		t.Errorf("step 1 = {prompt:%s brief:%q}; want {prompt:%s brief:%q}", steps[1].StepPromptID, steps[1].Brief, teamB.ID, "do b")
	}

	// (2) The materialized team trigger fires the team's blueprint copy — a real
	// blueprint_id, no prompt_id-in-blueprint_id muddiness.
	var teamTriggerBP, teamTriggerKind string
	if err := conn.QueryRowContext(ctx,
		`SELECT blueprint_id, kind FROM event_handlers WHERE org_id = ? AND team_id = ? AND system_slug = ?`,
		org, newTeamID, triggerSlug,
	).Scan(&teamTriggerBP, &teamTriggerKind); err != nil {
		t.Fatalf("read team trigger %q: %v", triggerSlug, err)
	}
	if teamTriggerKind != domain.EventHandlerKindTrigger {
		t.Errorf("materialized handler kind=%q; want trigger", teamTriggerKind)
	}
	if teamTriggerBP != teamBP.ID {
		t.Errorf("team trigger blueprint_id=%q; want the team's multi-step blueprint copy %q", teamTriggerBP, teamBP.ID)
	}

	// (3) Shipped 1-step blueprints still deep-copy with exactly one step.
	shippedBP, err := stores.Blueprints.GetBySystemSlug(ctx, org, newTeamID, "system-ci-fix")
	if err != nil || shippedBP == nil {
		t.Fatalf("shipped team blueprint system-ci-fix missing: err=%v", err)
	}
	shippedSteps, err := stores.Blueprints.ListSteps(ctx, org, shippedBP.ID)
	if err != nil {
		t.Fatalf("ListSteps(shipped): %v", err)
	}
	if len(shippedSteps) != 1 {
		t.Errorf("shipped blueprint system-ci-fix has %d steps; want 1", len(shippedSteps))
	}

	// (4) Idempotency: if the team edits its blueprint (removes a step), a
	// materialize re-run must NOT re-add it — steps are written only when the
	// team blueprint header is freshly inserted.
	if _, err := conn.ExecContext(ctx,
		`DELETE FROM blueprint_steps WHERE blueprint_id = ? AND step_index = 1`, teamBP.ID); err != nil {
		t.Fatalf("delete team step: %v", err)
	}
	if err := db.BootstrapNewTeam(ctx, stores, org, newTeamID); err != nil {
		t.Fatalf("BootstrapNewTeam re-run: %v", err)
	}
	stepsAfter, err := stores.Blueprints.ListSteps(ctx, org, teamBP.ID)
	if err != nil {
		t.Fatalf("ListSteps after re-run: %v", err)
	}
	if len(stepsAfter) != 1 {
		t.Errorf("materialize re-run re-added a team-removed step: %d steps, want 1 (existing team blueprints' steps must stay untouched)", len(stepsAfter))
	}
}

// TestOrgTemplate_MergeAndSplit pins the template mirror of the canvas
// composition gestures: MergeBlueprints absorbs a trigger-less source onto a
// host's tail and retires it (dense indices, copy-only owner moves with the
// step), and SplitBlueprint peels the tail onto a new trigger-less downstream
// blueprint (re-densified 0-based). These back the org-template merge/split
// endpoints the binding canvas calls at template scope.
func TestOrgTemplate_MergeAndSplit(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	org := runmode.LocalDefaultOrg
	if err := db.BootstrapNewOrg(ctx, stores, org, runmode.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}
	ot := stores.OrgTemplate

	mustPrompt := func(id, slug, name string) {
		t.Helper()
		if err := ot.CreatePrompt(ctx, org, domain.Prompt{ID: id, SystemSlug: slug, Name: name, Body: "b", Source: "user"}); err != nil {
			t.Fatalf("CreatePrompt %s: %v", id, err)
		}
	}
	mustBP := func(id, slug, name string) {
		t.Helper()
		if err := ot.CreateBlueprint(ctx, org, domain.Blueprint{ID: id, SystemSlug: slug, Name: name, Source: "user"}); err != nil {
			t.Fatalf("CreateBlueprint %s: %v", id, err)
		}
	}
	mustPrompt("ms-h", "ms-h-slug", "Host Step")
	mustPrompt("ms-s", "ms-s-slug", "Source Step")
	mustBP("ms-host", "ms-host-slug", "Host")
	mustBP("ms-src", "ms-src-slug", "Source")
	if err := ot.ReplaceBlueprintSteps(ctx, org, "ms-host", []string{"ms-h"}, nil); err != nil {
		t.Fatalf("ReplaceBlueprintSteps host: %v", err)
	}
	if err := ot.ReplaceBlueprintSteps(ctx, org, "ms-src", []string{"ms-s"}, nil); err != nil {
		t.Fatalf("ReplaceBlueprintSteps source: %v", err)
	}

	// --- Merge source onto the host's tail ---
	if err := ot.MergeBlueprints(ctx, org, "ms-host", "ms-src"); err != nil {
		t.Fatalf("MergeBlueprints: %v", err)
	}
	steps, err := ot.ListBlueprintSteps(ctx, org, "ms-host")
	if err != nil {
		t.Fatalf("ListBlueprintSteps host after merge: %v", err)
	}
	if len(steps) != 2 || steps[0].StepPromptID != "ms-h" || steps[1].StepPromptID != "ms-s" {
		t.Fatalf("merged steps = %+v; want [ms-h, ms-s]", steps)
	}
	if steps[0].StepIndex != 0 || steps[1].StepIndex != 1 {
		t.Fatalf("merged step indices not dense: %+v", steps)
	}
	if bp, err := ot.GetBlueprint(ctx, org, "ms-src"); err != nil || bp != nil {
		t.Fatalf("source after merge = (%v, %v); want (nil, nil) — retired", bp, err)
	}

	// --- Split the merged host back at the boundary ---
	newID, err := ot.SplitBlueprint(ctx, org, "ms-host", 1, "ms-down", "ms-down-slug", "Downstream")
	if err != nil {
		t.Fatalf("SplitBlueprint: %v", err)
	}
	if newID != "ms-down" {
		t.Fatalf("SplitBlueprint returned %q; want ms-down", newID)
	}
	upSteps, err := ot.ListBlueprintSteps(ctx, org, "ms-host")
	if err != nil {
		t.Fatalf("ListBlueprintSteps upstream: %v", err)
	}
	if len(upSteps) != 1 || upSteps[0].StepPromptID != "ms-h" {
		t.Fatalf("upstream steps = %+v; want [ms-h]", upSteps)
	}
	downSteps, err := ot.ListBlueprintSteps(ctx, org, "ms-down")
	if err != nil {
		t.Fatalf("ListBlueprintSteps downstream: %v", err)
	}
	if len(downSteps) != 1 || downSteps[0].StepPromptID != "ms-s" || downSteps[0].StepIndex != 0 {
		t.Fatalf("downstream steps = %+v; want [ms-s] at index 0 (re-densified)", downSteps)
	}
	// The copy-only owner moved with the step onto the new downstream blueprint.
	if owner, ok, err := ot.BlueprintStepPromptOwner(ctx, org, "ms-s"); err != nil || !ok || owner != "ms-down" {
		t.Fatalf("owner(ms-s) = (%q, %v, %v); want (ms-down, true, nil)", owner, ok, err)
	}
}

// TestOrgTemplate_DuplicateBlueprintPrompts pins the org-template mirror of
// blueprint duplication: a flat set of template prompt ids deep-copies into a
// new trigger-less template blueprint following the induced-contiguous-runs
// rule. Full run → "{src} (copy)"; copied prompts are fresh user rows (system
// source flips to user); briefs carry; originals untouched. Empty set rejected.
func TestOrgTemplate_DuplicateBlueprintPrompts(t *testing.T) {
	conn := openInMemorySQLite(t)
	stores := sqlitestore.New(conn)
	ctx := t.Context()
	org := runmode.LocalDefaultOrg
	if err := db.BootstrapNewOrg(ctx, stores, org, runmode.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}
	tmpl := stores.OrgTemplate

	slug := func() string { return "tmpl-" + uuid.New().String() }
	bpID := uuid.New().String()
	if err := tmpl.CreateBlueprint(ctx, org, domain.Blueprint{ID: bpID, SystemSlug: slug(), Name: "Src", Source: "user"}); err != nil {
		t.Fatalf("CreateBlueprint: %v", err)
	}
	p0, p1 := uuid.New().String(), uuid.New().String()
	if err := tmpl.CreatePrompt(ctx, org, domain.Prompt{ID: p0, SystemSlug: slug(), Name: "P0", Body: "b0", Source: "user"}); err != nil {
		t.Fatalf("CreatePrompt p0: %v", err)
	}
	// A system-source template prompt to prove the copy decouples to user.
	if err := tmpl.CreatePrompt(ctx, org, domain.Prompt{ID: p1, SystemSlug: slug(), Name: "P1", Body: "b1", Source: "system"}); err != nil {
		t.Fatalf("CreatePrompt p1: %v", err)
	}
	if err := tmpl.ReplaceBlueprintSteps(ctx, org, bpID, []string{p0, p1}, []string{"br0", "br1"}); err != nil {
		t.Fatalf("ReplaceBlueprintSteps: %v", err)
	}

	newIDs, err := tmpl.DuplicateBlueprintPrompts(ctx, org, []string{p0, p1})
	if err != nil {
		t.Fatalf("DuplicateBlueprintPrompts: %v", err)
	}
	if len(newIDs) != 1 {
		t.Fatalf("full-run template duplicate produced %d blueprints, want 1", len(newIDs))
	}
	bp, err := tmpl.GetBlueprint(ctx, org, newIDs[0])
	if err != nil || bp == nil {
		t.Fatalf("GetBlueprint(new) = (%v, %v)", bp, err)
	}
	if bp.Name != "Src (copy)" {
		t.Errorf("template clone name = %q, want %q", bp.Name, "Src (copy)")
	}
	steps, err := tmpl.ListBlueprintSteps(ctx, org, newIDs[0])
	if err != nil {
		t.Fatalf("ListBlueprintSteps(new): %v", err)
	}
	if len(steps) != 2 {
		t.Fatalf("new template blueprint has %d steps, want 2", len(steps))
	}
	wantBody := []string{"b0", "b1"}
	wantBrief := []string{"br0", "br1"}
	for i, st := range steps {
		if st.StepIndex != i {
			t.Errorf("step %d index = %d, want %d", i, st.StepIndex, i)
		}
		if st.StepPromptID == p0 || st.StepPromptID == p1 {
			t.Errorf("step %d reuses a source template prompt id; want a fresh copy", i)
		}
		if st.Brief != wantBrief[i] {
			t.Errorf("step %d brief = %q, want %q", i, st.Brief, wantBrief[i])
		}
		cp, err := tmpl.GetPrompt(ctx, org, st.StepPromptID)
		if err != nil || cp == nil {
			t.Fatalf("GetPrompt(copy %d): (%v, %v)", i, cp, err)
		}
		if cp.Body != wantBody[i] {
			t.Errorf("copied template prompt %d body = %q, want %q", i, cp.Body, wantBody[i])
		}
		if cp.Source != "user" {
			t.Errorf("copied template prompt %d source = %q, want user", i, cp.Source)
		}
		owner, ok, err := tmpl.BlueprintStepPromptOwner(ctx, org, st.StepPromptID)
		if err != nil || !ok || owner != newIDs[0] {
			t.Fatalf("BlueprintStepPromptOwner(%s) = (%q, %v, %v), want (%s, true, nil)", st.StepPromptID, owner, ok, err, newIDs[0])
		}
	}

	// Originals untouched: the source template prompt p1 is still a system prompt.
	src, err := tmpl.GetPrompt(ctx, org, p1)
	if err != nil || src == nil {
		t.Fatalf("GetPrompt(src p1): (%v, %v)", src, err)
	}
	if src.Source != "system" {
		t.Errorf("source template prompt source = %q, want system (unchanged)", src.Source)
	}

	if _, err := tmpl.DuplicateBlueprintPrompts(ctx, org, nil); !errors.Is(err, db.ErrDuplicateNoPrompts) {
		t.Errorf("DuplicateBlueprintPrompts(nil) err = %v, want ErrDuplicateNoPrompts", err)
	}
}
