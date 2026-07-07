package db_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	pgstore "github.com/sky-ai-eng/triage-factory/internal/db/postgres"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestOrgTemplate_Postgres_ForwardOnlyAndIsolation is the multi-tenant
// acceptance pin, on the real two-pool Postgres store:
//
//   - an org admin can edit their org's template (RLS allows the write);
//   - a non-admin member cannot (RLS denies it — defense in depth behind the
//     handler's requireOrgAdminRole gate);
//   - org A's edited template flows to A's *next* new team;
//   - the team that already existed when the edit landed is untouched
//     (forward-only);
//   - org B is unaffected by A's edit (cross-org isolation).
//
// Skips when Docker is unavailable (pgtest harness).
func TestOrgTemplate_Postgres_ForwardOnlyAndIsolation(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, ownerA, founderTeamA := pgtest.SeedOrgWithUser(t, h, "orgA-owner")
	orgB, _, founderTeamB := pgtest.SeedOrgWithUser(t, h, "orgB-owner")

	// A plain member of org A (not an admin) — used for the RLS-denial check.
	memberA := pgtest.SeedUser(t, h, "orgA-member")
	pgtest.AddOrgMember(t, h, memberA, orgA, founderTeamA, "member", "member")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	// Org-create seeds each org's template + founder team from the shipped lists.
	if err := db.BootstrapNewOrg(ctx, stores, orgA, founderTeamA, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg A: %v", err)
	}
	if err := db.BootstrapNewOrg(ctx, stores, orgB, founderTeamB, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg B: %v", err)
	}

	const customSlug = "tmpl-custom-A"

	// A non-admin member's template write is refused by RLS.
	memberErr := h.WithUser(t, memberA, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).OrgTemplate.CreatePrompt(ctx, orgA, domain.Prompt{
			ID: uuid.New().String(), SystemSlug: "tmpl-member-attempt", Name: "Nope", Body: "x", Source: "user",
		})
	})
	if memberErr == nil {
		t.Error("non-admin member created an org_template_prompts row; RLS should refuse it")
	}

	// The org admin (owner) edits the template — adds a house-rules prompt.
	if err := h.WithUser(t, ownerA, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).OrgTemplate.CreatePrompt(ctx, orgA, domain.Prompt{
			ID: uuid.New().String(), SystemSlug: customSlug, Name: "Org A House Rules", Body: "house rules", Source: "user",
		})
	}); err != nil {
		t.Fatalf("admin CreatePrompt: %v", err)
	}

	// A 2nd team in org A, created AFTER the edit, copies the edited template.
	team2A := pgtest.SeedTeam(t, h, orgA, "team2-a")
	if err := db.BootstrapNewTeam(ctx, stores, orgA, team2A); err != nil {
		t.Fatalf("BootstrapNewTeam A team2: %v", err)
	}

	// A team in org B, to confirm B never sees A's edit.
	team2B := pgtest.SeedTeam(t, h, orgB, "team2-b")
	if err := db.BootstrapNewTeam(ctx, stores, orgB, team2B); err != nil {
		t.Fatalf("BootstrapNewTeam B team2: %v", err)
	}

	countPrompt := func(org, team, slug string) int {
		t.Helper()
		var n int
		if err := h.AdminDB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM prompts WHERE org_id = $1 AND team_id = $2 AND system_slug = $3`, org, team, slug,
		).Scan(&n); err != nil {
			t.Fatalf("count prompt: %v", err)
		}
		return n
	}

	// Flows forward: A's new team carries the edit.
	if got := countPrompt(orgA, team2A, customSlug); got != 1 {
		t.Errorf("org A's new team has %d copies of the template-added prompt; want 1", got)
	}
	// Forward-only: A's founder team (pre-existing) is untouched.
	if got := countPrompt(orgA, founderTeamA, customSlug); got != 0 {
		t.Errorf("org A's pre-existing founder team gained %d copies of the post-create template edit; want 0 (forward-only)", got)
	}
	// Cross-org isolation: org B's new team never sees A's edit.
	if got := countPrompt(orgB, team2B, customSlug); got != 0 {
		t.Errorf("org B's new team has %d copies of org A's template prompt; want 0 (cross-org isolation)", got)
	}
	// And org B's template itself never received A's row.
	var bTemplateCustom int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM org_template_prompts WHERE org_id = $1 AND system_slug = $2`, orgB, customSlug,
	).Scan(&bTemplateCustom); err != nil {
		t.Fatalf("count org B template: %v", err)
	}
	if bTemplateCustom != 0 {
		t.Errorf("org B's template has %d copies of org A's custom slug; want 0", bTemplateCustom)
	}
}

// TestOrgTemplate_Postgres_MultiStepDeepCopy is the multi-step authoring
// acceptance pin on the real two-pool Postgres store: an org admin authors a
// multi-step template
// blueprint (app pool, org-admin RLS), and a new team materialized from it
// (admin pool) gets a deep-copied multi-step team blueprint with its steps
// re-pointed at the team's prompt copies — plus a team trigger that fires the
// team's blueprint copy (clean blueprint_id). Also pins that a non-admin member
// can't write the new org_template_blueprints table (RLS).
//
// Skips when Docker is unavailable (pgtest harness).
func TestOrgTemplate_Postgres_MultiStepDeepCopy(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, ownerA, founderTeamA := pgtest.SeedOrgWithUser(t, h, "orgA-deepcopy")
	memberA := pgtest.SeedUser(t, h, "orgA-deepcopy-member")
	pgtest.AddOrgMember(t, h, memberA, orgA, founderTeamA, "member", "member")

	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()

	if err := db.BootstrapNewOrg(ctx, stores, orgA, founderTeamA, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	// A non-admin member's template-blueprint write is refused by RLS.
	memberErr := h.WithUser(t, memberA, orgA, func(tx *sql.Tx) error {
		return pgstore.NewForTx(tx, pgtest.SecretKey).OrgTemplate.CreateBlueprint(ctx, orgA, domain.Blueprint{
			ID: uuid.New().String(), SystemSlug: "tmpl-member-bp", Name: "Nope", Source: "user",
		})
	})
	if memberErr == nil {
		t.Error("non-admin member created an org_template_blueprints row; RLS should refuse it")
	}

	// The org admin authors two template prompts + a 2-step template blueprint,
	// then promotes a rule to a trigger firing it — all on the app pool.
	promptAID := uuid.New().String()
	promptBID := uuid.New().String()
	blueprintID := uuid.New().String()
	var triggerSlug string
	if err := h.WithUser(t, ownerA, orgA, func(tx *sql.Tx) error {
		st := pgstore.NewForTx(tx, pgtest.SecretKey).OrgTemplate
		if e := st.CreatePrompt(ctx, orgA, domain.Prompt{ID: promptAID, SystemSlug: "tmpl-step-a", Name: "Step A", Body: "a", Source: "user"}); e != nil {
			return e
		}
		if e := st.CreatePrompt(ctx, orgA, domain.Prompt{ID: promptBID, SystemSlug: "tmpl-step-b", Name: "Step B", Body: "b", Source: "user"}); e != nil {
			return e
		}
		if e := st.CreateBlueprint(ctx, orgA, domain.Blueprint{ID: blueprintID, SystemSlug: "tmpl-multi", Name: "Two-step", Source: "user"}); e != nil {
			return e
		}
		if e := st.ReplaceBlueprintSteps(ctx, orgA, blueprintID, []string{promptAID, promptBID}, []string{"do a", "do b"}); e != nil {
			return e
		}
		rules, e := st.ListHandlers(ctx, orgA, domain.EventHandlerKindRule)
		if e != nil || len(rules) == 0 {
			t.Fatalf("ListHandlers(rule): err=%v n=%d", e, len(rules))
		}
		triggerSlug = rules[0].SystemSlug
		bt := 3
		minA := 0.0
		return st.PromoteHandler(ctx, orgA, rules[0].ID, domain.EventHandler{
			Kind: domain.EventHandlerKindTrigger, BlueprintID: blueprintID, BreakerThreshold: &bt, MinAutonomySuitability: &minA,
		})
	}); err != nil {
		t.Fatalf("author multi-step template: %v", err)
	}

	// Materialize a new team from the edited template.
	team2 := pgtest.SeedTeam(t, h, orgA, "team2-deepcopy")
	if err := db.BootstrapNewTeam(ctx, stores, orgA, team2); err != nil {
		t.Fatalf("BootstrapNewTeam: %v", err)
	}

	// The new team got a real multi-step blueprint copy (read via admin pool).
	var teamBPID string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT id FROM blueprints WHERE org_id = $1 AND team_id = $2 AND system_slug = 'tmpl-multi'`, orgA, team2,
	).Scan(&teamBPID); err != nil {
		t.Fatalf("resolve team multi-step blueprint: %v", err)
	}
	var stepCount int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM blueprint_steps WHERE org_id = $1 AND blueprint_id = $2`, orgA, teamBPID,
	).Scan(&stepCount); err != nil {
		t.Fatalf("count team steps: %v", err)
	}
	if stepCount != 2 {
		t.Fatalf("team multi-step blueprint has %d steps; want 2 (deep-copy, not a 1-step wrapper)", stepCount)
	}

	// Steps re-point at the team's own prompt copies, in order.
	rows, err := h.AdminDB.QueryContext(ctx, `
		SELECT bs.step_prompt_id, p.system_slug
		FROM blueprint_steps bs JOIN prompts p ON p.id = bs.step_prompt_id AND p.org_id = bs.org_id
		WHERE bs.org_id = $1 AND bs.blueprint_id = $2 ORDER BY bs.step_index`, orgA, teamBPID)
	if err != nil {
		t.Fatalf("read team steps: %v", err)
	}
	defer rows.Close()
	var gotSlugs []string
	for rows.Next() {
		var pid, slug string
		if err := rows.Scan(&pid, &slug); err != nil {
			t.Fatalf("scan step: %v", err)
		}
		gotSlugs = append(gotSlugs, slug)
	}
	if len(gotSlugs) != 2 || gotSlugs[0] != "tmpl-step-a" || gotSlugs[1] != "tmpl-step-b" {
		t.Errorf("team step prompt slugs = %v; want [tmpl-step-a tmpl-step-b]", gotSlugs)
	}

	// The materialized team trigger fires the team's blueprint copy.
	var teamTriggerBP string
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT blueprint_id FROM event_handlers WHERE org_id = $1 AND team_id = $2 AND system_slug = $3`,
		orgA, team2, triggerSlug,
	).Scan(&teamTriggerBP); err != nil {
		t.Fatalf("read team trigger: %v", err)
	}
	if teamTriggerBP != teamBPID {
		t.Errorf("team trigger blueprint_id=%q; want the team's multi-step blueprint copy %q", teamTriggerBP, teamBPID)
	}

	// Idempotency: if the team removes a step, a materialize re-run must NOT
	// re-add it — steps are written only when the team blueprint header is
	// freshly inserted (a re-run finds the existing header and skips steps).
	if _, err := h.AdminDB.ExecContext(ctx,
		`DELETE FROM blueprint_steps WHERE org_id = $1 AND blueprint_id = $2 AND step_index = 1`, orgA, teamBPID); err != nil {
		t.Fatalf("delete team step: %v", err)
	}
	if err := db.BootstrapNewTeam(ctx, stores, orgA, team2); err != nil {
		t.Fatalf("BootstrapNewTeam re-run: %v", err)
	}
	var stepsAfterRerun int
	if err := h.AdminDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM blueprint_steps WHERE org_id = $1 AND blueprint_id = $2`, orgA, teamBPID,
	).Scan(&stepsAfterRerun); err != nil {
		t.Fatalf("count steps after re-run: %v", err)
	}
	if stepsAfterRerun != 1 {
		t.Errorf("materialize re-run re-added a team-removed step: step count = %d, want 1 (existing team blueprints' steps must stay untouched)", stepsAfterRerun)
	}
}

// TestOrgTemplate_Postgres_MergeAndSplit pins the org-template composition
// mechanics on the real two-pool store: an org admin merges a trigger-less
// source onto a host's tail (the source retires, indices stay dense, the
// copy-only owner moves with the step) and splits it back at the boundary into
// a new trigger-less downstream blueprint — all on the app pool under org-admin
// RLS, in one transaction.
//
// Skips when Docker is unavailable (pgtest harness).
func TestOrgTemplate_Postgres_MergeAndSplit(t *testing.T) {
	h := pgtest.Shared(t)
	h.Reset(t)

	orgA, ownerA, founderTeamA := pgtest.SeedOrgWithUser(t, h, "orgA-compose")
	stores := pgstore.New(h.AdminDB, h.AppDB, pgtest.SecretKey)
	ctx := context.Background()
	if err := db.BootstrapNewOrg(ctx, stores, orgA, founderTeamA, ai.ShippedPrompts(), ai.ShippedBlueprints()); err != nil {
		t.Fatalf("BootstrapNewOrg: %v", err)
	}

	hostPrompt := uuid.New().String()
	srcPrompt := uuid.New().String()
	hostBP := uuid.New().String()
	srcBP := uuid.New().String()
	downBP := uuid.New().String()

	// All composition runs on the app pool with the admin's claims (RLS allows),
	// in one transaction — merge/split share it via withTemplateTx's *sql.Tx case.
	if err := h.WithUser(t, ownerA, orgA, func(tx *sql.Tx) error {
		st := pgstore.NewForTx(tx, pgtest.SecretKey).OrgTemplate
		if e := st.CreatePrompt(ctx, orgA, domain.Prompt{ID: hostPrompt, SystemSlug: "compose-h", Name: "Host Step", Body: "h", Source: "user"}); e != nil {
			return e
		}
		if e := st.CreatePrompt(ctx, orgA, domain.Prompt{ID: srcPrompt, SystemSlug: "compose-s", Name: "Source Step", Body: "s", Source: "user"}); e != nil {
			return e
		}
		if e := st.CreateBlueprint(ctx, orgA, domain.Blueprint{ID: hostBP, SystemSlug: "compose-h-bp", Name: "Host", Source: "user"}); e != nil {
			return e
		}
		if e := st.CreateBlueprint(ctx, orgA, domain.Blueprint{ID: srcBP, SystemSlug: "compose-s-bp", Name: "Source", Source: "user"}); e != nil {
			return e
		}
		if e := st.ReplaceBlueprintSteps(ctx, orgA, hostBP, []string{hostPrompt}, nil); e != nil {
			return e
		}
		if e := st.ReplaceBlueprintSteps(ctx, orgA, srcBP, []string{srcPrompt}, nil); e != nil {
			return e
		}

		// Merge source onto the host's tail.
		if e := st.MergeBlueprints(ctx, orgA, hostBP, srcBP); e != nil {
			return e
		}
		steps, e := st.ListBlueprintSteps(ctx, orgA, hostBP)
		if e != nil {
			return e
		}
		if len(steps) != 2 || steps[0].StepPromptID != hostPrompt || steps[1].StepPromptID != srcPrompt {
			t.Fatalf("merged steps = %+v; want [host, source]", steps)
		}
		if src, e := st.GetBlueprint(ctx, orgA, srcBP); e != nil || src != nil {
			t.Fatalf("source after merge = (%v, %v); want (nil, nil) — retired", src, e)
		}

		// Split back at the boundary.
		if _, e := st.SplitBlueprint(ctx, orgA, hostBP, 1, downBP, "compose-down-bp", "Downstream"); e != nil {
			return e
		}
		up, e := st.ListBlueprintSteps(ctx, orgA, hostBP)
		if e != nil {
			return e
		}
		if len(up) != 1 || up[0].StepPromptID != hostPrompt {
			t.Fatalf("upstream steps = %+v; want [host]", up)
		}
		down, e := st.ListBlueprintSteps(ctx, orgA, downBP)
		if e != nil {
			return e
		}
		if len(down) != 1 || down[0].StepPromptID != srcPrompt || down[0].StepIndex != 0 {
			t.Fatalf("downstream steps = %+v; want [source] at index 0", down)
		}
		if owner, ok, e := st.BlueprintStepPromptOwner(ctx, orgA, srcPrompt); e != nil || !ok || owner != downBP {
			t.Fatalf("owner(source) = (%q, %v, %v); want (downstream, true, nil)", owner, ok, e)
		}
		return nil
	}); err != nil {
		t.Fatalf("compose template blueprints: %v", err)
	}
}
