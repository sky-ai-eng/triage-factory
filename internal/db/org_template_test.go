package db_test

import (
	"testing"

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

	if err := db.BootstrapNewOrg(ctx, stores, org, db.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
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
	blueprints, err := stores.OrgTemplate.ListBlueprints(ctx, org)
	if err != nil || len(blueprints) == 0 {
		t.Fatalf("ListBlueprints: err=%v n=%d", err, len(blueprints))
	}
	bt := 3
	minA := 0.0
	if err := stores.OrgTemplate.PromoteHandler(ctx, org, rule.ID, domain.EventHandler{
		Kind:                   domain.EventHandlerKindTrigger,
		BlueprintID:            blueprints[0].ID,
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
	if err := db.BootstrapNewOrg(ctx, stores, org, db.LocalDefaultTeamID, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
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
}
