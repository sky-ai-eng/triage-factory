package main

import (
	"context"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedDefaultPrompts seeds the shipped system prompts via PromptStore
// AND the shipped system rules + triggers via EventHandlerStore per
// (org, team). Post-SKY-259 rules and triggers are one table; post-
// SKY-295 system rules + triggers materialize as team-scoped rows
// (visibility='team', team_id=<team>) rather than org-visible — so the
// router can route each matched handler to its team's queue without a
// membership-blind fallback. Local mode iterates a single synthetic
// (org, team); multi mode will iterate stores.Orgs.ListAll + each
// org's teams once Wave 4 lands OrgStore. Multi-mode team creation
// (when that flow ships) also calls EventHandlerStore.Seed for the
// new team so it inherits the shipped defaults.
//
// Calls into:
//   - PromptStore.SeedOrUpdate — SQLite: single conn; Postgres: admin
//     pool internally (the system_prompt_versions sidecar is REVOKE'd
//     from tf_app per D3). Prompts stay org-visible: they're
//     referenced by triggers across teams via the composite FK
//     (prompt_id, org_id), so duplicating per-team would proliferate
//     identical rows for no benefit.
//   - EventHandlerStore.Seed — admin-pool routing in Postgres because
//     shipped rows have NULL creator_user_id and the modify policies
//     gate user-visible writes on tf.current_user_id() which is
//     unset at boot.
//
// Order matters: prompts seed first so the FK from
// event_handlers.prompt_id → prompts.id is satisfied for shipped
// trigger rows.
func seedDefaultPrompts(prompts db.PromptStore, handlers db.EventHandlerStore) {
	ctx := context.Background()

	// TODO(SKY-246 wave 4): when OrgStore lands, replace this hard-coded
	// slice with stores.Orgs.ListAll(ctx) × per-org teams. Until then
	// multi mode is fatal at startup so the local-only (org, team)
	// pair is correct.
	type orgTeam struct{ orgID, teamID string }
	orgs := []orgTeam{{runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID}}

	// Shipped system prompts live in ai.ShippedPrompts() — the single
	// source of truth shared with the multi-mode org-create bootstrap
	// (db.BootstrapNewOrg) so the two seed paths can't drift.
	shipped := ai.ShippedPrompts()

	for _, ot := range orgs {
		for _, p := range shipped {
			if err := prompts.SeedOrUpdate(ctx, ot.orgID, p); err != nil {
				log.Printf("[seed] warning: failed to seed %s in org %s: %v", p.ID, ot.orgID, err)
			}
		}
		// Event handlers (rules + triggers, post-SKY-259) ship after
		// prompts in the same org iteration so the FK from
		// event_handlers.prompt_id → prompts.id resolves in Postgres
		// (the constraint is composite on (prompt_id, org_id)).
		// SKY-295: materialize per team — handler rows carry team_id
		// so the router can route directly to the correct team's
		// queue.
		if err := handlers.Seed(ctx, ot.orgID, ot.teamID); err != nil {
			log.Printf("[seed] warning: failed to seed event_handlers in org %s team %s: %v", ot.orgID, ot.teamID, err)
		}
	}
	// The shipped rule + trigger entries live in db.ShippedEventHandlers
	// (post-SKY-259 successor to ShippedTaskRules + ShippedPromptTriggers).
	// Edit that list to add or modify a shipped row.
}
