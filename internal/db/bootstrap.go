package db

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Bootstrap functions for agent identity (SKY-260 D-Agent).
//
// All entry points share one property: they're idempotent. The org /
// team / local bootstrap can run at every signup, handler call, or
// explicit provision action without changing user state — INSERT-OR-
// IGNORE semantics live in the AgentStore + TeamAgentStore impls and
// the OrgTemplate copy.
//
// Callers:
//
//   - BootstrapLocalOrg — the local "Start your Triage Factory"
//     provision action (POST /api/setup/start). Creates the synthetic
//     tenant rows, then runs the shared BootstrapNewOrg chain. Nothing
//     provisions at boot anymore.
//   - BootstrapAgentForOrg — multi-mode org-create handler (D7
//     SKY-251), runs after the orgs row inserts + before any team is
//     created for that org.
//   - BootstrapTeamAgent — multi-mode team-create handler (also D7),
//     runs after each new teams row inserts.

// BootstrapLocalOrg is the local-mode analog of multi's "create the
// tenant rows + BootstrapNewOrg" signup path. It idempotently creates
// the synthetic orgs / users / org_memberships / teams(Default) /
// memberships / org_settings / team_settings(auto_delegate_enabled=1)
// rows for the runmode.LocalDefault* sentinels (via
// OrgsStore.CreateLocalTenant), then runs the shared BootstrapNewOrg
// chain to seed the org template + materialize the founder team's
// agent + prompts + blueprints + handlers.
//
// This is the ONE shared provision operation, triggered by a deliberate
// user action: multi fires it via the admin's create-org flow,
// local fires it via "Start your Triage Factory". The only differences
// are what fires it and what identity is auto-filled (local hardcodes
// the LocalDefault* sentinels); the seed path is identical. Nothing
// runs it at boot — a fresh local DB has zero tenant rows until the
// user provisions.
//
// Fully re-entrant: re-running after a partial provision (or after the
// user already provisioned) reaches the same end state — the tenant-row
// inserts are INSERT OR IGNORE and BootstrapNewOrg's seeders skip via
// ON CONFLICT, so a previously deleted shipped default is never
// resurrected. shippedPrompts + shippedBlueprints are passed in (rather
// than read from internal/ai) so internal/db stays free of the ai
// dependency — the caller supplies ai.ShippedPrompts() /
// ai.ShippedBlueprints().
func BootstrapLocalOrg(ctx context.Context, stores Stores, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	if err := stores.Orgs.CreateLocalTenant(ctx); err != nil {
		return fmt.Errorf("bootstrap local org: create tenant rows: %w", err)
	}
	if err := BootstrapNewOrg(ctx, stores, runmode.LocalDefaultOrg, LocalDefaultTeamID, shippedPrompts, shippedBlueprints); err != nil {
		return fmt.Errorf("bootstrap local org: %w", err)
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
// lists), the founder's first team's prompts + blueprints + handlers (copied
// *from the template*, SKY-381), and the team's default-enabled bot membership.
// shippedPrompts + shippedBlueprints are passed in (rather than read from
// internal/ai) so internal/db stays free of the ai dependency — main / server
// supply ai.ShippedPrompts() / ai.ShippedBlueprints().
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
func BootstrapNewOrg(ctx context.Context, stores Stores, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	if _, err := BootstrapAgentForOrg(ctx, stores, orgID); err != nil {
		return err
	}
	if err := stores.OrgTemplate.SeedFromShipped(ctx, orgID, shippedPrompts, shippedBlueprints); err != nil {
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
