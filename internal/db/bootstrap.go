package db

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Bootstrap functions for agent identity (SKY-260 D-Agent).
//
// All three entry points share one property: they're idempotent. The
// org / team / local bootstrap can run at every startup or every
// handler call without changing user state — INSERT-OR-IGNORE
// semantics live in the AgentStore + TeamAgentStore impls.
//
// Callers:
//
//   - BootstrapLocalAgent — main.go startup, runs alongside
//     seedDefaultPrompts. Existing v1.10.1 → current installs pick up
//     the agents + team_agents rows on first post-upgrade boot.
//   - BootstrapAgentForOrg — multi-mode org-create handler (D7
//     SKY-251), runs after the orgs row inserts + before any team is
//     created for that org.
//   - BootstrapTeamAgent — multi-mode team-create handler (also D7),
//     runs after each new teams row inserts.

// BootstrapLocalAgent inserts the synthetic local-mode agent + the
// local-mode team_agents row. Safe to call at every startup. Returns
// nil if the rows already exist; returns the underlying store error
// otherwise.
//
// In local mode the agent's credential FKs stay NULL — the PAT lives
// in the OS keychain, not in agents.github_pat_user_id (there's no
// users table to FK into). The agents row is metadata that becomes
// load-bearing when D-Claims wires claimed_by_agent_id +
// actor_agent_id; until then, the spawner continues to read PATs
// from the keychain directly.
func BootstrapLocalAgent(ctx context.Context, stores Stores) error {
	agentID, err := stores.Agents.Create(ctx, runmode.LocalDefaultOrg, domain.Agent{
		DisplayName: "Triage Factory Bot",
	})
	if err != nil {
		return fmt.Errorf("bootstrap local agent: %w", err)
	}
	if err := stores.TeamAgents.AddForTeam(ctx, runmode.LocalDefaultOrg, LocalDefaultTeamID, agentID); err != nil {
		return fmt.Errorf("bootstrap local team_agents: %w", err)
	}
	return nil
}

// BootstrapAgentForOrg inserts the org's single agents row. Called by
// the org-create handler (D7 SKY-251) right after the orgs row +
// founder's org_memberships row insert. Returns the agents.id so the
// caller can immediately stamp it into the first team via
// BootstrapTeamAgent.
//
// Routes through the admin pool in the Postgres impl because the
// founder isn't yet an admin per RLS's view at this moment (their
// org_memberships row is in the same transaction).
func BootstrapAgentForOrg(ctx context.Context, stores Stores, orgID string) (string, error) {
	agentID, err := stores.Agents.Create(ctx, orgID, domain.Agent{
		DisplayName: "Triage Factory Bot",
	})
	if err != nil {
		return "", fmt.Errorf("bootstrap agent for org %s: %w", orgID, err)
	}
	return agentID, nil
}

// BootstrapTeamAgent inserts a default-enabled team_agents row for the
// given team. Called by the team-create handler (D7) and by
// org-create after BootstrapAgentForOrg returns. Errors if the org
// has no agent row yet — calling team-bootstrap before org-bootstrap
// is a sequencing bug in the caller and silent-skip would leave teams
// with no bot membership, which is surprising to debug after the fact.
//
// The agent lookup uses the System (admin-pool) variant to match the
// rest of the bootstrap chain — Agents.Create, prompt/handler seeding,
// and TeamAgents.AddForTeam all route through the admin pool. Bootstrap
// runs outside any WithTx with no JWT claims, so the app pool (a bare
// authenticator connection until SET ROLE tf_app + claims inside a
// request tx) has no table grant and the app-pool GetForOrg would fail
// with "permission denied for table agents".
func BootstrapTeamAgent(ctx context.Context, stores Stores, orgID, teamID string) error {
	agent, err := stores.Agents.GetForOrgSystem(ctx, orgID)
	if err != nil {
		return fmt.Errorf("bootstrap team_agents: lookup agent for org %s: %w", orgID, err)
	}
	if agent == nil {
		return fmt.Errorf("bootstrap team_agents: org %s has no agent — call BootstrapAgentForOrg first", orgID)
	}
	if err := stores.TeamAgents.AddForTeam(ctx, orgID, teamID, agent.ID); err != nil {
		return fmt.Errorf("bootstrap team_agents: %w", err)
	}
	return nil
}

// SeedTeamDefaults seeds a team's own copies of the shipped prompts,
// blueprints, and event handlers in three phases:
//
//	(1) seed each prompt copy, capturing system_slug → the team's
//	    prompt-copy UUID;
//	(2) seed each blueprint header + its steps (resolving each step's prompt
//	    slug via the phase-1 map), capturing system_slug → the team's
//	    blueprint-copy UUID;
//	(3) seed the handlers, resolving each trigger's blueprint slug via the
//	    phase-2 map so the trigger→blueprint same-team FK
//	    ((blueprint_id, team_id) → blueprints(id, team_id)) is satisfied.
//
// Idempotent: re-runs no-op via the (org_id, team_id, system_slug) unique keys
// on prompts + blueprints + event_handlers (steps re-write to the same dense
// 0..N-1 list). shippedPrompts + shippedBlueprints are passed in (rather than
// imported from internal/ai) so internal/db stays free of the ai dependency —
// callers supply ai.ShippedPrompts() / ai.ShippedBlueprints().
//
// Must run OUTSIDE any caller WithTx: SeedOrUpdate + EventHandlers.Seed route
// through the Postgres admin pool (the system_prompt_versions sidecar +
// claims-less system rows) and refuse to run inside an app-pool tx.
func SeedTeamDefaults(ctx context.Context, prompts PromptStore, blueprints BlueprintStore, handlers EventHandlerStore, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	promptIDsBySlug := make(map[string]string, len(shippedPrompts))
	for _, p := range shippedPrompts {
		id, err := prompts.SeedOrUpdate(ctx, orgID, teamID, p)
		if err != nil {
			return fmt.Errorf("seed team defaults: prompt %s: %w", p.SystemSlug, err)
		}
		if p.SystemSlug != "" {
			promptIDsBySlug[p.SystemSlug] = id
		}
	}

	blueprintIDsBySlug := make(map[string]string, len(shippedBlueprints))
	for _, b := range shippedBlueprints {
		bpID, err := blueprints.SeedOrUpdate(ctx, orgID, teamID, domain.Blueprint{
			SystemSlug: b.SystemSlug, Name: b.Name, Source: "system",
		})
		if err != nil {
			return fmt.Errorf("seed team defaults: blueprint %s: %w", b.SystemSlug, err)
		}
		stepPromptIDs := make([]string, 0, len(b.StepPromptSlugs))
		for _, slug := range b.StepPromptSlugs {
			pid, ok := promptIDsBySlug[slug]
			if !ok || pid == "" {
				return fmt.Errorf("seed team defaults: blueprint %s step prompt slug %q not seeded (seed prompts before blueprints)", b.SystemSlug, slug)
			}
			stepPromptIDs = append(stepPromptIDs, pid)
		}
		if err := blueprints.ReplaceSteps(ctx, orgID, bpID, stepPromptIDs, nil); err != nil {
			return fmt.Errorf("seed team defaults: blueprint %s steps: %w", b.SystemSlug, err)
		}
		if b.SystemSlug != "" {
			blueprintIDsBySlug[b.SystemSlug] = bpID
		}
	}

	if err := handlers.Seed(ctx, orgID, teamID, blueprintIDsBySlug); err != nil {
		return fmt.Errorf("seed team defaults: event handlers: %w", err)
	}
	return nil
}

// BootstrapNewTeam materializes the structural defaults for a *new team
// in an existing org* (SKY-378): the team's default-enabled bot
// membership (team_agents) plus its own copies of the prompts + event
// handlers (rules/triggers) — copied from the org template (SKY-381).
//
// Per-team seeding is correct (SKY-380): handler + prompt rows carry a
// random-UUID id and a system_slug, deduped on (org_id, team_id,
// system_slug), so a 2nd+ team materializes its own distinct copies instead
// of ON CONFLICT-vanishing against the org's first team.
//
// The *source* of those copies is the org template, not the TF-shipped Go
// slices (SKY-381): an org admin edits org_template_prompts +
// org_template_handlers, and MaterializeIntoTeam copies the *current*
// template into the new team — so the team inherits the org's house rules
// (an extra trigger, a tweaked prompt body, an enabled trigger). Editing the
// template is forward-only: it changes what the next new team inherits and
// never touches existing teams. The org template is seeded from the shipped
// lists once at org-create (BootstrapNewOrg), so every org always has one.
//
// The bot row is enabled, so manual delegation (swipe / factory drag) to
// the new team works immediately.
//
// Must run OUTSIDE any caller WithTx: TeamAgents.AddForTeam + MaterializeIntoTeam
// route through the Postgres admin pool and guard against being called inside
// an app-pool tx. Idempotent — re-runs no-op via ON CONFLICT, and never flips a
// team's bot back to enabled if the team disabled it.
func BootstrapNewTeam(ctx context.Context, stores Stores, orgID, teamID string) error {
	if err := BootstrapTeamAgent(ctx, stores, orgID, teamID); err != nil {
		return err
	}
	if err := stores.OrgTemplate.MaterializeIntoTeam(ctx, orgID, teamID); err != nil {
		return fmt.Errorf("bootstrap new team: %w", err)
	}
	return nil
}

// BootstrapNewOrg materializes the defaults for a brand-new org + its
// default team (the multi-mode founder-signup path, SKY-378 / SKY-251 D7):
// the org's single agents row, the org template (seeded from the shipped
// lists), the founder's first team's prompts + handlers (copied *from the
// template*, SKY-381), and the team's default-enabled bot membership.
// shippedPrompts is passed in (rather than read from internal/ai) so
// internal/db stays free of the ai dependency — main / server supply
// ai.ShippedPrompts().
//
// Order is load-bearing: agent → seed template → materialize first team →
// team_agents. The org template is seeded from the shipped lists first
// (SeedFromShipped), then the founder's team is materialized by *copying the
// template* rather than the shipped lists directly (SKY-381) — the path is now
// uniform (template → team) for the first team and every later one, and an
// org admin who edits the template before adding a 2nd team gets those edits
// in the 2nd team. First-team contents are unchanged from the direct shipped
// seed because the template == the shipped lists at org-create. The
// team_agents row needs the agent created in step 1.
//
// Like BootstrapNewTeam this must run OUTSIDE any WithTx (admin-pool
// seeders) and is fully idempotent. The org-provisioning caller runs it
// AFTER the tenant-row transaction commits, and logs-and-continues on
// error: a provisioned org with un-seeded defaults is usable (the user
// is signed in with an org + team) and a later bootstrap re-run repairs
// auto-delegation, whereas failing the signup callback strands the user.
func BootstrapNewOrg(ctx context.Context, stores Stores, orgID, teamID string, shippedPrompts []domain.Prompt) error {
	if _, err := BootstrapAgentForOrg(ctx, stores, orgID); err != nil {
		return err
	}
	if err := stores.OrgTemplate.SeedFromShipped(ctx, orgID, shippedPrompts); err != nil {
		return fmt.Errorf("bootstrap new org: seed template: %w", err)
	}
	if err := stores.OrgTemplate.MaterializeIntoTeam(ctx, orgID, teamID); err != nil {
		return fmt.Errorf("bootstrap new org: materialize first team: %w", err)
	}
	if err := BootstrapTeamAgent(ctx, stores, orgID, teamID); err != nil {
		return err
	}
	return nil
}
