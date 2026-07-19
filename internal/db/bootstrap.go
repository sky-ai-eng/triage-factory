package db

import (
	"context"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// Bootstrap functions for agent identity.
//
// All entry points share one property: they're idempotent. The org /
// team / local bootstrap can run at every signup, handler call, or
// explicit provision action without changing user state — INSERT-OR-
// IGNORE semantics live in the AgentStore + TeamAgentStore impls and
// the ShippedDefaults seed.
//
// Callers:
//
//   - BootstrapLocalOrg — the local "Start your factory"
//     provision action (POST /api/setup/start). Creates the synthetic
//     tenant rows, then runs the shared BootstrapNewOrg chain. Nothing
//     provisions at boot anymore.
//   - BootstrapAgentForOrg — multi-mode org-create handler (D7),
//     runs after the orgs row inserts + before any team is
//     created for that org.
//   - BootstrapTeamAgent — multi-mode team-create handler (also D7),
//     runs after each new teams row inserts.

// BootstrapLocalOrg is the local-mode analog of multi's "create the
// tenant rows + BootstrapNewOrg" signup path. It idempotently creates
// the synthetic orgs / users / org_memberships / teams(Default) /
// memberships / org_settings / team_settings(auto_delegate_enabled=1)
// rows for the runmode.LocalDefault* sentinels (via
// OrgsStore.CreateLocalTenant), then runs the shared BootstrapNewOrg
// chain to seed the founder team's agent + prompts + blueprints +
// handlers directly from the shipped defaults.
//
// This is the ONE shared provision operation, triggered by a deliberate
// user action: multi fires it via the admin's create-org flow,
// local fires it via "Start your factory". The only differences
// are what fires it and what identity is auto-filled (local hardcodes
// the LocalDefault* sentinels); the seed path is identical. Nothing
// runs it at boot — a fresh local DB has zero tenant rows until the
// user provisions.
//
// Fully re-entrant for crash-mid-provision recovery: re-running after a
// partial provision reaches the same end state. Note that if it runs
// again after a user has deleted shipped defaults, the seed/materialize
// steps can re-create them — POST /api/setup/start avoids that by
// no-op'ing once a tenant exists, which is where the non-resurrection
// guarantee lives. shippedPrompts + shippedBlueprints are passed in
// (rather than read from internal/ai) so internal/db stays free of the
// ai dependency — the caller supplies ai.ShippedPrompts() /
// ai.ShippedBlueprints().
func BootstrapLocalOrg(ctx context.Context, stores Stores, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	if err := stores.Orgs.CreateLocalTenant(ctx); err != nil {
		return fmt.Errorf("bootstrap local org: create tenant rows: %w", err)
	}
	if err := BootstrapNewOrg(ctx, stores, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, shippedPrompts, shippedBlueprints); err != nil {
		return fmt.Errorf("bootstrap local org: %w", err)
	}
	return nil
}

// BootstrapAgentForOrg inserts the org's single agents row. Called by
// the org-create handler (D7) right after the orgs row +
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
// in an existing org*: the team's default-enabled bot
// membership (team_agents) plus its own copies of the prompts + blueprints +
// event handlers (rules/triggers) — seeded directly from the TF-shipped Go
// slices (ai.ShippedPrompts() / ai.ShippedBlueprints() / db.ShippedEventHandlers),
// same as the founder's first team (BootstrapNewOrg). shippedPrompts +
// shippedBlueprints are passed in (rather than read from internal/ai) so
// internal/db stays free of the ai dependency — the caller supplies
// ai.ShippedPrompts() / ai.ShippedBlueprints().
//
// Per-team seeding is correct: handler + prompt rows carry a
// random-UUID id and a system_slug, deduped on (org_id, team_id,
// system_slug), so a 2nd+ team materializes its own distinct copies instead
// of ON CONFLICT-vanishing against the org's first team.
//
// The bot row is enabled, so manual delegation (swipe / factory drag) to
// the new team works immediately.
//
// Must run OUTSIDE any caller WithTx: TeamAgents.AddForTeam + SeedShippedIntoTeam
// route through the Postgres admin pool and guard against being called inside
// an app-pool tx. Idempotent — re-runs no-op via ON CONFLICT, and never flips a
// team's bot back to enabled if the team disabled it.
func BootstrapNewTeam(ctx context.Context, stores Stores, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	if err := BootstrapTeamAgent(ctx, stores, orgID, teamID); err != nil {
		return err
	}
	if err := stores.ShippedDefaults.SeedShippedIntoTeam(ctx, orgID, teamID, shippedPrompts, shippedBlueprints); err != nil {
		return fmt.Errorf("bootstrap new team: %w", err)
	}
	return nil
}

// BootstrapNewOrg materializes the defaults for a brand-new org + its
// default team (the multi-mode founder-signup path, D7):
// the org's single agents row, the founder's first team's prompts +
// blueprints + handlers (seeded directly from the shipped lists), and the
// team's default-enabled bot membership. shippedPrompts + shippedBlueprints
// are passed in (rather than read from internal/ai) so internal/db stays
// free of the ai dependency — main / server supply ai.ShippedPrompts() /
// ai.ShippedBlueprints().
//
// Order is load-bearing: agent → seed shipped defaults into the first team →
// team_agents. The first team is seeded the exact same way every later team
// is (BootstrapNewTeam) — straight from the shipped Go slices, no org
// template detour. The team_agents row needs the agent created in step 1.
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
	if err := stores.ShippedDefaults.SeedShippedIntoTeam(ctx, orgID, teamID, shippedPrompts, shippedBlueprints); err != nil {
		return fmt.Errorf("bootstrap new org: seed shipped defaults: %w", err)
	}
	if err := BootstrapTeamAgent(ctx, stores, orgID, teamID); err != nil {
		return err
	}
	return nil
}
