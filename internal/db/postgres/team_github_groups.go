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
//   - admin: TeamsForGitHubTeamSystem, ReconcileOrgTeamsSystem. Routing +
//     reconcile from background callers without JWT claims.
//   - app: ListForTeam, SetForTeam. Request-handler reads/writes gated by
//     team_github_groups_select / _insert / _delete (team membership /
//     team admin).
//
// Every row carries both GitHub ids (NOT NULL), so the interface's handling
// of a row without them never arises here.
type teamGitHubGroupsStore struct {
	app   queryer
	admin queryer
}

func newTeamGitHubGroupsStore(app, admin queryer) db.TeamGitHubGroupsStore {
	return &teamGitHubGroupsStore{app: app, admin: admin}
}

var _ db.TeamGitHubGroupsStore = (*teamGitHubGroupsStore)(nil)

func (s *teamGitHubGroupsStore) ListForTeam(ctx context.Context, teamID, host string) ([]domain.TeamGitHubGroup, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	rows, err := s.app.QueryContext(ctx, `
		SELECT github_team_id, github_org_id, github_org_login, github_team_slug
		FROM team_github_groups
		WHERE team_id = $1 AND host = $2
		ORDER BY lower(github_org_login), lower(github_team_slug), github_team_id
	`, teamID, host)
	if err != nil {
		return nil, fmt.Errorf("read team_github_groups: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamGitHubGroup{}
	for rows.Next() {
		var g domain.TeamGitHubGroup
		if err := rows.Scan(&g.GitHubTeamID, &g.GitHubOrgID, &g.OrgLogin, &g.TeamSlug); err != nil {
			return nil, fmt.Errorf("scan team_github_groups: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *teamGitHubGroupsStore) SetForTeam(ctx context.Context, teamID, host string, groups []domain.TeamGitHubGroup) error {
	norm, err := domain.NormalizeTeamGitHubGroups(groups)
	if err != nil {
		return err
	}
	if err := db.RequireRepoHost(host); err != nil {
		return err
	}
	// Every row here carries its ids, so a group without them names no
	// stored row to keep.
	for _, g := range norm {
		if g.GitHubTeamID == "" {
			return fmt.Errorf("no github group without ids is stored for %s/%s", g.OrgLogin, g.TeamSlug)
		}
	}
	// Replace-set: delete the host's set and re-insert inside one tx so the
	// table never observes a partial mid-sync state.
	return inTx(ctx, s.app, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM team_github_groups WHERE team_id = $1 AND host = $2`, teamID, host,
		); err != nil {
			return fmt.Errorf("clear team_github_groups: %w", err)
		}
		for _, g := range norm {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_github_groups (team_id, host, github_team_id, github_org_id, github_org_login, github_team_slug)
				VALUES ($1, $2, $3, $4, $5, $6)
			`, teamID, host, g.GitHubTeamID, g.GitHubOrgID, g.OrgLogin, g.TeamSlug); err != nil {
				return fmt.Errorf("insert team_github_groups[%s]: %w", g.GitHubTeamID, err)
			}
		}
		return nil
	})
}

func (s *teamGitHubGroupsStore) TeamsForGitHubTeamSystem(ctx context.Context, orgID, host, githubTeamID, orgLogin, teamSlug string) ([]string, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	// Join teams to scope by org — team_github_groups carries no org_id
	// (it FKs to teams), so org scoping rides the parent.
	var (
		match string
		args  = []any{orgID, host}
	)
	if githubTeamID != "" {
		match = `g.github_team_id = $3`
		args = append(args, githubTeamID)
	} else {
		login, slug := strings.TrimSpace(orgLogin), strings.TrimSpace(teamSlug)
		if login == "" || slug == "" {
			return []string{}, nil
		}
		match = `lower(g.github_org_login) = lower($3) AND lower(g.github_team_slug) = lower($4)`
		args = append(args, login, slug)
	}
	rows, err := s.admin.QueryContext(ctx, `
		SELECT DISTINCT g.team_id
		FROM team_github_groups g
		JOIN teams t ON t.id = g.team_id
		WHERE t.org_id = $1
		  AND g.host = $2
		  AND `+match+`
		ORDER BY g.team_id ASC
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("teams for github team: %w", err)
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

func (s *teamGitHubGroupsStore) ReconcileOrgTeamsSystem(ctx context.Context, orgID, host string, listing domain.GitHubOrgTeams) (int, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return 0, err
	}
	if !domain.IsGitHubID(listing.OrgID) {
		return 0, fmt.Errorf("ReconcileOrgTeamsSystem: %q is not a GitHub organization id", listing.OrgID)
	}
	// Non-nil even when empty: a nil slice binds as NULL, and `= ANY(NULL)`
	// would delete nothing instead of every row of an organization with no
	// teams left.
	ids, slugs := []string{}, []string{}
	for _, t := range listing.Teams {
		if domain.IsGitHubID(t.ID) && strings.TrimSpace(t.Slug) != "" {
			ids = append(ids, t.ID)
			slugs = append(slugs, strings.TrimSpace(t.Slug))
		}
	}
	login := strings.TrimSpace(listing.OrgLogin)
	var pruned int
	err := inTx(ctx, s.admin, func(tx queryer) error {
		// Display refresh. Only rows whose names moved are written, so the
		// steady state (nothing renamed) writes nothing.
		if login != "" {
			if _, err := tx.ExecContext(ctx, `
				UPDATE team_github_groups
				   SET github_org_login = $4
				 WHERE host = $2 AND github_org_id = $3
				   AND team_id IN (SELECT id FROM teams WHERE org_id = $1)
				   AND github_org_login <> $4
			`, orgID, host, listing.OrgID, login); err != nil {
				return fmt.Errorf("refresh team_github_groups org login: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE team_github_groups g
			   SET github_team_slug = l.slug
			  FROM unnest($4::text[], $5::text[]) AS l(id, slug)
			 WHERE g.github_team_id = l.id
			   AND g.host = $2 AND g.github_org_id = $3
			   AND g.team_id IN (SELECT id FROM teams WHERE org_id = $1)
			   AND g.github_team_slug <> l.slug
		`, orgID, host, listing.OrgID, ids, slugs); err != nil {
			return fmt.Errorf("refresh team_github_groups team slugs: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
			DELETE FROM team_github_groups
			 WHERE host = $2 AND github_org_id = $3
			   AND team_id IN (SELECT id FROM teams WHERE org_id = $1)
			   AND NOT (github_team_id = ANY($4::text[]))
		`, orgID, host, listing.OrgID, ids)
		if err != nil {
			return fmt.Errorf("prune team_github_groups: %w", err)
		}
		n, _ := res.RowsAffected()
		pruned = int(n)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return pruned, nil
}
