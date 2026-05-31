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

// BootstrapNewTeam materializes the structural defaults for a *new team
// in an existing org* (SKY-378): the team's default-enabled bot
// membership (team_agents). It deliberately does NOT seed event handlers
// (system rules/triggers).
//
// Why bot-only — two reasons converge:
//
//   - Correctness: the shipped-handler row id is UUIDFor(handler.ID,
//     orgID) — team-independent — under PK (org_id, id). Seeding a 2nd+
//     team's handlers would collide with the org's first team's rows and
//     ON CONFLICT-skip them all, so the seed never actually materialized
//     per-team rows anyway (Codex review on PR #263).
//
//   - Product direction: a newly-created team is a blank slate the org
//     admin configures, not a place to re-stamp TF's opinionated
//     defaults. Org-defined default templates that new teams inherit are
//     their own ticket; until then a 2nd+ team starts with no system
//     handlers and the admin adds rules (the org's *first* team still
//     gets the full defaults via BootstrapNewOrg).
//
//     TODO(SKY-381): copy the org's default-team template into the new
//     team here (handlers + prompts, forward-only) instead of seeding
//     bot-only — replaces this blank-slate behavior once org templates
//     land.
//
// The bot row is enabled, so manual delegation (swipe / factory drag) to
// the new team works immediately; only auto-delegation waits on rules.
//
// Must run OUTSIDE any caller WithTx: TeamAgents.AddForTeam routes
// through the Postgres admin pool and guards against being called inside
// an app-pool tx. Idempotent — re-runs no-op via ON CONFLICT, and never
// flip a team's bot back to enabled if the team disabled it.
func BootstrapNewTeam(ctx context.Context, stores Stores, orgID, teamID string) error {
	if err := BootstrapTeamAgent(ctx, stores, orgID, teamID); err != nil {
		return err
	}
	return nil
}

// BootstrapNewOrg materializes the shipped defaults for a brand-new org
// + its default team (the multi-mode founder-signup path, SKY-378 /
// SKY-251 D7): the org's single agents row, the shipped system prompts,
// the shipped event handlers scoped to the default team, and the team's
// default-enabled bot membership. shippedPrompts is passed in (rather
// than read from internal/ai) so internal/db stays free of the ai
// dependency — main / server supply ai.ShippedPrompts().
//
// Order is load-bearing: agent → prompts → handlers → team_agents. The
// trigger rows in EventHandlers.Seed FK into the prompts (composite
// (prompt_id, org_id)), so prompts must land first; the team_agents row
// needs the agent created in step 1.
//
// TODO(SKY-380): when prompts go team-scoped the prompt seed here becomes
// team-owned copies (keyed by system_slug) and the trigger→prompt FK
// becomes same-team; the org-wide visibility='org' prompt rows go away.
// TODO(SKY-381): the org's first team's defaults will be sourced from the
// org template (itself seeded from Shipped* at org-create) rather than
// directly from the TF-shipped lists, so org admins can shape what new
// orgs/teams start with.
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
	for _, p := range shippedPrompts {
		if err := stores.Prompts.SeedOrUpdate(ctx, orgID, p); err != nil {
			return fmt.Errorf("bootstrap new org: seed prompt %s: %w", p.ID, err)
		}
	}
	if err := stores.EventHandlers.Seed(ctx, orgID, teamID); err != nil {
		return fmt.Errorf("bootstrap new org: seed event handlers: %w", err)
	}
	if err := BootstrapTeamAgent(ctx, stores, orgID, teamID); err != nil {
		return err
	}
	return nil
}
