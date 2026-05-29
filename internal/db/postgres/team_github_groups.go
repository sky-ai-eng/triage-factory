package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamGitHubGroupsStore is the Postgres impl of
// db.TeamGitHubGroupsStore. Holds both pools — see the
// TeamGitHubGroupsStore interface comment for the pool-split rationale.
//
//   - admin: ListForTeamSystem, TeamsForGroupSystem, PruneMissingSystem.
//     Routing + reconcile from background callers without JWT claims.
//   - app: ListForTeam, SetForTeam, TeamsForGroup. Request-handler
//     reads/writes gated by team_github_groups_select / _insert /
//     _delete (team membership / team admin).
type teamGitHubGroupsStore struct {
	app   queryer
	admin queryer
}

func newTeamGitHubGroupsStore(app, admin queryer) db.TeamGitHubGroupsStore {
	return &teamGitHubGroupsStore{app: app, admin: admin}
}

var _ db.TeamGitHubGroupsStore = (*teamGitHubGroupsStore)(nil)

func (s *teamGitHubGroupsStore) ListForTeam(ctx context.Context, teamID string) ([]domain.TeamGitHubGroup, error) {
	return listTeamGitHubGroups(ctx, s.app, teamID)
}

func (s *teamGitHubGroupsStore) ListForTeamSystem(ctx context.Context, teamID string) ([]domain.TeamGitHubGroup, error) {
	return listTeamGitHubGroups(ctx, s.admin, teamID)
}

func listTeamGitHubGroups(ctx context.Context, q queryer, teamID string) ([]domain.TeamGitHubGroup, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT github_org_login, github_team_slug
		FROM team_github_groups
		WHERE team_id = $1
		ORDER BY github_org_login ASC, github_team_slug ASC
	`, teamID)
	if err != nil {
		return nil, fmt.Errorf("read team_github_groups: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamGitHubGroup{}
	for rows.Next() {
		var g domain.TeamGitHubGroup
		if err := rows.Scan(&g.OrgLogin, &g.TeamSlug); err != nil {
			return nil, fmt.Errorf("scan team_github_groups: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *teamGitHubGroupsStore) SetForTeam(ctx context.Context, teamID string, groups []domain.TeamGitHubGroup) error {
	norm, err := domain.NormalizeTeamGitHubGroups(groups)
	if err != nil {
		return err
	}
	// Replace-set: every column is part of the primary key, so there is
	// nothing to update in place. Delete the whole set and re-insert the
	// survivors inside one tx so the table never observes a partial
	// mid-sync state.
	return inTx(ctx, s.app, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM team_github_groups WHERE team_id = $1`, teamID,
		); err != nil {
			return fmt.Errorf("clear team_github_groups: %w", err)
		}
		for _, g := range norm {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_github_groups (team_id, github_org_login, github_team_slug)
				VALUES ($1, $2, $3)
				ON CONFLICT (team_id, github_org_login, github_team_slug) DO NOTHING
			`, teamID, g.OrgLogin, g.TeamSlug); err != nil {
				return fmt.Errorf("insert team_github_groups[%s/%s]: %w", g.OrgLogin, g.TeamSlug, err)
			}
		}
		return nil
	})
}

func (s *teamGitHubGroupsStore) TeamsForGroup(ctx context.Context, orgID, orgLogin, teamSlug string) ([]string, error) {
	return teamsForGroup(ctx, s.app, orgID, orgLogin, teamSlug)
}

func (s *teamGitHubGroupsStore) TeamsForGroupSystem(ctx context.Context, orgID, orgLogin, teamSlug string) ([]string, error) {
	return teamsForGroup(ctx, s.admin, orgID, orgLogin, teamSlug)
}

func teamsForGroup(ctx context.Context, q queryer, orgID, orgLogin, teamSlug string) ([]string, error) {
	// Join teams to scope by org — team_github_groups carries no org_id
	// (it FKs to teams), so org scoping rides the parent. Match the
	// GitHub identifiers case-insensitively against the lowercase-
	// normalized stored values.
	rows, err := q.QueryContext(ctx, `
		SELECT g.team_id
		FROM team_github_groups g
		JOIN teams t ON t.id = g.team_id
		WHERE t.org_id = $1
		  AND g.github_org_login = $2
		  AND g.github_team_slug = $3
		ORDER BY g.team_id ASC
	`, orgID, strings.ToLower(strings.TrimSpace(orgLogin)), strings.ToLower(strings.TrimSpace(teamSlug)))
	if err != nil {
		return nil, fmt.Errorf("teams for github group: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan team_id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *teamGitHubGroupsStore) PruneMissingSystem(ctx context.Context, orgID, orgLogin string, presentSlugs []string) (int, error) {
	login := strings.ToLower(strings.TrimSpace(orgLogin))
	if login == "" {
		return 0, fmt.Errorf("PruneMissingSystem: empty orgLogin")
	}
	keep := domain.NormalizeGitHubTeamSlugs(presentSlugs)
	// An empty present-set means the org genuinely has no GitHub teams,
	// so every mapping for this org login is stale. `<> ALL($3)` with a
	// nil array binds as NULL and would match nothing, so take the
	// unconditional delete in that case (mirrors the jira store's
	// empty-keys branch).
	if len(keep) == 0 {
		res, err := s.admin.ExecContext(ctx, `
			DELETE FROM team_github_groups
			WHERE github_org_login = $2
			  AND team_id IN (SELECT id FROM teams WHERE org_id = $1)
		`, orgID, login)
		if err != nil {
			return 0, fmt.Errorf("prune team_github_groups (clear %s): %w", login, err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	res, err := s.admin.ExecContext(ctx, `
		DELETE FROM team_github_groups
		WHERE github_org_login = $2
		  AND github_team_slug <> ALL($3)
		  AND team_id IN (SELECT id FROM teams WHERE org_id = $1)
	`, orgID, login, keep)
	if err != nil {
		return 0, fmt.Errorf("prune team_github_groups (%s): %w", login, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
