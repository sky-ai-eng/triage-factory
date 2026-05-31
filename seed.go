package main

import (
	"context"
	"log"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedDefaultPrompts seeds the local team's own copies of the shipped
// system prompts AND the shipped system rules + triggers via the shared
// two-phase helper (db.SeedTeamDefaults). Post-SKY-380 prompts are
// team-scoped (team_id NOT NULL, visibility ∈ {private, team}); each is a
// team-owned copy keyed by system_slug, and shipped triggers wire to the
// team's own prompt copies via the slug→id resolve. Local mode is the single
// synthetic (org, team); multi-mode org/team creation runs the same helper
// through db.BootstrapNewOrg / db.BootstrapNewTeam, so the two seed paths
// can't drift.
//
// SeedTeamDefaults routes through the Postgres admin pool internally (the
// system_prompt_versions sidecar is REVOKE'd from tf_app per D3, and shipped
// rows carry NULL creator_user_id). The shipped rule + trigger entries live
// in db.ShippedEventHandlers; the prompt content lives in ai.ShippedPrompts().
func seedDefaultPrompts(prompts db.PromptStore, handlers db.EventHandlerStore) {
	ctx := context.Background()

	// TODO(SKY-246 wave 4): when OrgStore lands, iterate stores.Orgs.ListAll ×
	// per-org teams here. Until then multi mode is fatal at startup so the
	// local-only (org, team) pair is correct.
	if err := db.SeedTeamDefaults(ctx, prompts, handlers,
		runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, ai.ShippedPrompts()); err != nil {
		log.Printf("[seed] warning: failed to seed team defaults: %v", err)
	}
}
