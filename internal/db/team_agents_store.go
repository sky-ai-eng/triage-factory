package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

//go:generate go run github.com/vektra/mockery/v2 --name=TeamAgentStore --output=./mocks --case=underscore --with-expecter

// TeamAgentStore owns team_agents — the per-team membership row for
// the org's agent, plus per-team config overrides. One row per
// (team_id, agent_id), default-enabled at team creation by the
// bootstrap path. Nothing in the product flips enabled or writes an
// override yet: the row's toggle and override columns are read on every
// trigger fire and delegate gesture, but the only writer is bootstrap.
//
// Audiences:
//
//   - Bootstrap (internal/db/bootstrap.go) — AddForTeam on org-create
//     (for each team that already exists) and on every subsequent
//     team-create handler call.
//   - D-Claims router — GetForTeamSystem to decide whether a
//     trigger fire creates a claimed task (enabled team) or falls
//     back to unclaimed (disabled team, prompt pre-filled).
//   - The delegate + team-member handlers — GetForTeam inside the
//     request's claims tx, to refuse a gesture on a team whose bot is
//     off.
//
// # Pool split (Postgres)
//
//   - app pool — tf_app, RLS-active. GetForTeam. team_agents_select
//     gates on tf.user_in_team(team_id), so team members read their
//     own team's row but not other teams'. The write policies
//     (team_agents_insert/team_agents_update/team_agents_delete) gate
//     the same way: per the locked architecture decision, team-bot
//     toggling is a team-member power, not an admin-only power, and the
//     policies already admit it for the day a door writes it.
//   - admin pool — supabase_admin, BYPASSRLS. AddForTeam and
//     GetForTeamSystem. Same reasoning as AgentStore.Create: bootstrap
//     and the router run without claims.
//
// SQLite collapses both pools to one connection; assertLocalOrg pins
// orgID to LocalDefaultOrgID.
//
// # The returned-row rule
//
// The store's one write is AddForTeam, exempt as said at the method.
type TeamAgentStore interface {
	// GetForTeam returns the row for (team_id, agent_id), or (nil, nil)
	// if absent — "this team has no membership row yet" is the state
	// AddForTeam exists to fix. The request handlers call this inside the
	// claims tx to gate a delegate gesture.
	GetForTeam(ctx context.Context, orgID, teamID, agentID string) (*domain.TeamAgent, error)

	// AddForTeam inserts a default-enabled membership row. Idempotent
	// on (team_id, agent_id) — duplicate calls leave the existing row
	// alone (no re-flipping Enabled). Bootstrap-only path; Postgres
	// routes through the admin pool.
	//
	// Exempt from the returned-row rule, by decision rather than by shape: the
	// insert is ON CONFLICT DO NOTHING so a re-run leaves the row's toggle
	// and overrides alone, and that returns zero rows precisely on the re-run.
	// Bootstrap does not read the row back.
	AddForTeam(ctx context.Context, orgID, teamID, agentID string) error

	// GetForTeamSystem mirrors GetForTeam but routes through the admin
	// pool in Postgres. The router reads this on every auto-trigger
	// fire from its eventbus subscriber goroutine, which has no JWT
	// claims set.
	GetForTeamSystem(ctx context.Context, orgID, teamID, agentID string) (*domain.TeamAgent, error)
}

// LocalDefaultTeamID is the synthetic team id used in local mode.
// It equals runmode.LocalDefaultTeamID — the sentinel
// UUID that the SQLite migration inserts as the one row of the
// teams table. Kept as an alias here for the migration period; new
// code should reference runmode.LocalDefaultTeamID directly.
const LocalDefaultTeamID = runmode.LocalDefaultTeamID
