package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamGitHubGroupsStore is the SQLite impl of db.TeamGitHubGroupsStore.
// The constructor accepts two queryers for signature parity with the
// Postgres impl; SQLite has one connection so both collapse. The
// `...System` variants delegate to their non-System counterparts.
type teamGitHubGroupsStore struct{ q queryer }

func newTeamGitHubGroupsStore(q, _ queryer) db.TeamGitHubGroupsStore {
	return &teamGitHubGroupsStore{q: q}
}

var _ db.TeamGitHubGroupsStore = (*teamGitHubGroupsStore)(nil)

func (s *teamGitHubGroupsStore) ListForTeam(ctx context.Context, teamID string) ([]domain.TeamGitHubGroup, error) {
	return listTeamGitHubGroups(ctx, s.q, teamID)
}

func (s *teamGitHubGroupsStore) ListForTeamSystem(ctx context.Context, teamID string) ([]domain.TeamGitHubGroup, error) {
	return listTeamGitHubGroups(ctx, s.q, teamID)
}

func listTeamGitHubGroups(ctx context.Context, q queryer, teamID string) ([]domain.TeamGitHubGroup, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT github_org_login, github_team_slug
		FROM team_github_groups
		WHERE team_id = ?
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
	// Replace-set: every column is part of the primary key, so delete
	// the whole set and re-insert the survivors inside one tx — there is
	// nothing to update in place.
	return inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM team_github_groups WHERE team_id = ?`, teamID,
		); err != nil {
			return fmt.Errorf("clear team_github_groups: %w", err)
		}
		for _, g := range norm {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_github_groups (team_id, github_org_login, github_team_slug)
				VALUES (?, ?, ?)
				ON CONFLICT(team_id, github_org_login, github_team_slug) DO NOTHING
			`, teamID, g.OrgLogin, g.TeamSlug); err != nil {
				return fmt.Errorf("insert team_github_groups[%s/%s]: %w", g.OrgLogin, g.TeamSlug, err)
			}
		}
		return nil
	})
}

func (s *teamGitHubGroupsStore) TeamsForGroup(ctx context.Context, orgID, orgLogin, teamSlug string) ([]string, error) {
	return teamsForGroup(ctx, s.q, orgID, orgLogin, teamSlug)
}

func (s *teamGitHubGroupsStore) TeamsForGroupSystem(ctx context.Context, orgID, orgLogin, teamSlug string) ([]string, error) {
	return teamsForGroup(ctx, s.q, orgID, orgLogin, teamSlug)
}

func teamsForGroup(ctx context.Context, q queryer, orgID, orgLogin, teamSlug string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT g.team_id
		FROM team_github_groups g
		JOIN teams t ON t.id = g.team_id
		WHERE t.org_id = ?
		  AND g.github_org_login = ?
		  AND g.github_team_slug = ?
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
	// Empty present-set → every mapping for this org login is stale.
	// SQLite has no array binding, so build the NOT IN placeholder list
	// dynamically when there is something to keep.
	if len(keep) == 0 {
		res, err := s.q.ExecContext(ctx, `
			DELETE FROM team_github_groups
			WHERE github_org_login = ?
			  AND team_id IN (SELECT id FROM teams WHERE org_id = ?)
		`, login, orgID)
		if err != nil {
			return 0, fmt.Errorf("prune team_github_groups (clear %s): %w", login, err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	placeholders := make([]string, len(keep))
	args := make([]any, 0, len(keep)+2)
	args = append(args, login, orgID)
	for i, slug := range keep {
		placeholders[i] = "?"
		args = append(args, slug)
	}
	query := fmt.Sprintf(`
		DELETE FROM team_github_groups
		WHERE github_org_login = ?
		  AND team_id IN (SELECT id FROM teams WHERE org_id = ?)
		  AND github_team_slug NOT IN (%s)
	`, strings.Join(placeholders, ", "))
	res, err := s.q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("prune team_github_groups (%s): %w", login, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
