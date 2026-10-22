package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// teamGitHubGroupsStore is the SQLite impl of db.TeamGitHubGroupsStore.
// The constructor accepts two queryers for signature parity with the
// Postgres impl; SQLite has one connection so both collapse.
//
// A row stored before ids were recorded has NULL github_team_id and
// github_org_id; see the interface doc for how each method treats one.
type teamGitHubGroupsStore struct{ q queryer }

func newTeamGitHubGroupsStore(q, _ queryer) db.TeamGitHubGroupsStore {
	return &teamGitHubGroupsStore{q: q}
}

var _ db.TeamGitHubGroupsStore = (*teamGitHubGroupsStore)(nil)

func (s *teamGitHubGroupsStore) ListForTeam(ctx context.Context, teamID, host string) ([]domain.TeamGitHubGroup, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT github_team_id, github_org_id, github_org_login, github_team_slug
		FROM team_github_groups
		WHERE team_id = ? AND host = ?
		ORDER BY lower(github_org_login), lower(github_team_slug), github_team_id
	`, teamID, host)
	if err != nil {
		return nil, fmt.Errorf("read team_github_groups: %w", err)
	}
	defer rows.Close()
	out := []domain.TeamGitHubGroup{}
	for rows.Next() {
		var (
			g             domain.TeamGitHubGroup
			teamID, orgID sql.NullString
		)
		if err := rows.Scan(&teamID, &orgID, &g.OrgLogin, &g.TeamSlug); err != nil {
			return nil, fmt.Errorf("scan team_github_groups: %w", err)
		}
		g.GitHubTeamID, g.GitHubOrgID = teamID.String, orgID.String
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
	// Replace-set: delete the host's set and re-insert inside one tx, except
	// that a group without ids spares the stored row with its handle rather
	// than inserting one.
	keep := []string{}
	for _, g := range norm {
		if g.GitHubTeamID == "" {
			keep = append(keep, g.Handle())
		}
	}
	kept, err := json.Marshal(keep)
	if err != nil {
		return fmt.Errorf("encode kept handles: %w", err)
	}
	const handle = `lower(github_org_login) || '/' || lower(github_team_slug)`
	return inTx(ctx, s.q, func(tx queryer) error {
		var stored int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(DISTINCT `+handle+`) FROM team_github_groups
			WHERE team_id = ? AND host = ? AND github_team_id IS NULL
			  AND `+handle+` IN (SELECT value FROM json_each(?))
		`, teamID, host, string(kept)).Scan(&stored); err != nil {
			return fmt.Errorf("read team_github_groups without ids: %w", err)
		}
		if stored != len(keep) {
			return fmt.Errorf("%d of the %d github groups without ids name no stored row", len(keep)-stored, len(keep))
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM team_github_groups
			WHERE team_id = ? AND host = ?
			  AND NOT (github_team_id IS NULL AND `+handle+` IN (SELECT value FROM json_each(?)))
		`, teamID, host, string(kept)); err != nil {
			return fmt.Errorf("clear team_github_groups: %w", err)
		}
		for _, g := range norm {
			if g.GitHubTeamID == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO team_github_groups (team_id, host, github_team_id, github_org_id, github_org_login, github_team_slug)
				VALUES (?, ?, ?, ?, ?, ?)
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
	login, slug := strings.TrimSpace(orgLogin), strings.TrimSpace(teamSlug)
	// A row without ids is matched by its handle whichever way the event
	// names the team; a row with them only by its id when the event has one.
	byHandle := `lower(g.github_org_login) = lower(?) AND lower(g.github_team_slug) = lower(?)`
	var (
		match string
		args  = []any{orgID, host}
	)
	switch {
	case githubTeamID != "" && login != "" && slug != "":
		match = `(g.github_team_id = ? OR (g.github_team_id IS NULL AND ` + byHandle + `))`
		args = append(args, githubTeamID, login, slug)
	case githubTeamID != "":
		match = `g.github_team_id = ?`
		args = append(args, githubTeamID)
	case login != "" && slug != "":
		match = byHandle
		args = append(args, login, slug)
	default:
		return []string{}, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT g.team_id
		FROM team_github_groups g
		JOIN teams t ON t.id = g.team_id
		WHERE t.org_id = ?
		  AND g.host = ?
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

// listedTeam is one team of a reconcile's listing in the JSON array the
// statements below read through json_each: SQLite binds no arrays.
type listedTeam struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

func (s *teamGitHubGroupsStore) ReconcileOrgTeamsSystem(ctx context.Context, orgID, host string, listing domain.GitHubOrgTeams) (int, error) {
	if err := db.RequireRepoHost(host); err != nil {
		return 0, err
	}
	if !domain.IsGitHubID(listing.OrgID) {
		return 0, fmt.Errorf("ReconcileOrgTeamsSystem: %q is not a GitHub organization id", listing.OrgID)
	}
	teams := []listedTeam{}
	for _, t := range listing.Teams {
		if domain.IsGitHubID(t.ID) && strings.TrimSpace(t.Slug) != "" {
			teams = append(teams, listedTeam{ID: t.ID, Slug: strings.TrimSpace(t.Slug)})
		}
	}
	listed, err := json.Marshal(teams)
	if err != nil {
		return 0, fmt.Errorf("encode team listing: %w", err)
	}
	login := strings.TrimSpace(listing.OrgLogin)
	const orgTeams = `team_id IN (SELECT id FROM teams WHERE org_id = ?)`
	var pruned int
	err = inTx(ctx, s.q, func(tx queryer) error {
		if login != "" {
			// Identify the rows stored without ids, by their handle. One the
			// same TF team already holds under the team's id is a duplicate
			// and goes, so the identified row does not collide with it.
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM team_github_groups
				 WHERE github_team_id IS NULL AND host = ? AND `+orgTeams+`
				   AND lower(github_org_login) = lower(?)
				   AND EXISTS (
				       SELECT 1 FROM json_each(?) l, team_github_groups held
				        WHERE lower(json_extract(l.value, '$.slug')) = lower(team_github_groups.github_team_slug)
				          AND held.team_id = team_github_groups.team_id
				          AND held.host = team_github_groups.host
				          AND held.github_team_id = json_extract(l.value, '$.id'))
			`, host, orgID, login, string(listed)); err != nil {
				return fmt.Errorf("drop duplicate team_github_groups: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE team_github_groups
				   SET github_org_id = ?,
				       github_team_id = (
				           SELECT json_extract(l.value, '$.id') FROM json_each(?) l
				            WHERE lower(json_extract(l.value, '$.slug')) = lower(team_github_groups.github_team_slug))
				 WHERE github_team_id IS NULL AND host = ? AND `+orgTeams+`
				   AND lower(github_org_login) = lower(?)
				   AND EXISTS (
				       SELECT 1 FROM json_each(?) l
				        WHERE lower(json_extract(l.value, '$.slug')) = lower(team_github_groups.github_team_slug))
			`, listing.OrgID, string(listed), host, orgID, login, string(listed)); err != nil {
				return fmt.Errorf("identify team_github_groups: %w", err)
			}
			// Display refresh: only rows whose names moved are written.
			if _, err := tx.ExecContext(ctx, `
				UPDATE team_github_groups
				   SET github_org_login = ?
				 WHERE host = ? AND github_org_id = ? AND `+orgTeams+`
				   AND github_org_login <> ?
			`, login, host, listing.OrgID, orgID, login); err != nil {
				return fmt.Errorf("refresh team_github_groups org login: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE team_github_groups
			   SET github_team_slug = (
			       SELECT json_extract(l.value, '$.slug') FROM json_each(?) l
			        WHERE json_extract(l.value, '$.id') = team_github_groups.github_team_id)
			 WHERE host = ? AND github_org_id = ? AND `+orgTeams+`
			   AND EXISTS (
			       SELECT 1 FROM json_each(?) l
			        WHERE json_extract(l.value, '$.id') = team_github_groups.github_team_id
			          AND json_extract(l.value, '$.slug') <> team_github_groups.github_team_slug)
		`, string(listed), host, listing.OrgID, orgID, string(listed)); err != nil {
			return fmt.Errorf("refresh team_github_groups team slugs: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
			DELETE FROM team_github_groups
			 WHERE host = ? AND github_org_id = ? AND `+orgTeams+`
			   AND github_team_id NOT IN (SELECT json_extract(l.value, '$.id') FROM json_each(?) l)
		`, host, listing.OrgID, orgID, string(listed))
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
