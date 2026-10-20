package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jiraProjectStore is the Postgres impl of db.JiraProjectStore. See the
// interface comment for the pool split: app for FindOrCreate, admin for
// RefreshSystem.
type jiraProjectStore struct {
	app   queryer
	admin queryer
}

func newJiraProjectStore(app, admin queryer) db.JiraProjectStore {
	return &jiraProjectStore{app: app, admin: admin}
}

var _ db.JiraProjectStore = (*jiraProjectStore)(nil)

func (s *jiraProjectStore) GetSystem(ctx context.Context, orgID, id string) (*domain.JiraProject, error) {
	p, err := db.ScanJiraProject(s.admin.QueryRowContext(ctx,
		`SELECT `+db.JiraProjectColumns+` FROM jira_projects WHERE id = $1 AND org_id = $2`, id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read jira project %s: %w", id, err)
	}
	return &p, nil
}

func (s *jiraProjectStore) FindOrCreate(ctx context.Context, orgID, site, externalID, key, name string) (domain.JiraProject, error) {
	if orgID == "" || site == "" || externalID == "" || key == "" {
		return domain.JiraProject{}, fmt.Errorf("find or create jira project: org, site, project id and key are required")
	}
	var out domain.JiraProject
	err := inTx(ctx, s.app, func(tx queryer) error {
		// An unresolved row under the key is this project, unless another row
		// already carries the id; then that row is the project and the
		// unresolved one is left for the poll to merge.
		p, err := db.ScanJiraProject(tx.QueryRowContext(ctx, `
			UPDATE jira_projects
			   SET external_id = $4, project_name = $5
			 WHERE org_id = $1 AND site = $2 AND project_key = $3 AND external_id IS NULL
			   AND NOT EXISTS (
			       SELECT 1 FROM jira_projects o
			        WHERE o.org_id = $1 AND o.site = $2 AND o.external_id = $4)
			RETURNING `+db.JiraProjectColumns,
			orgID, site, key, externalID, name))
		if err == nil {
			out = p
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("resolve jira project %s: %w", key, err)
		}
		out, err = db.ScanJiraProject(tx.QueryRowContext(ctx, `
			INSERT INTO jira_projects (org_id, site, external_id, project_key, project_name)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (org_id, site, external_id) WHERE external_id IS NOT NULL DO UPDATE SET
				project_key = EXCLUDED.project_key,
				project_name = EXCLUDED.project_name
			RETURNING `+db.JiraProjectColumns,
			orgID, site, externalID, key, name))
		if err != nil {
			return fmt.Errorf("upsert jira project %s: %w", key, err)
		}
		return nil
	})
	if err != nil {
		return domain.JiraProject{}, err
	}
	return out, nil
}

func (s *jiraProjectStore) RefreshSystem(ctx context.Context, orgID, id, externalID, key, name string) (domain.JiraProject, error) {
	if externalID == "" || key == "" {
		return domain.JiraProject{}, fmt.Errorf("refresh jira project %s: project id and key are required", id)
	}
	var out domain.JiraProject
	err := inTx(ctx, s.admin, func(tx queryer) error {
		row, err := db.ScanJiraProject(tx.QueryRowContext(ctx,
			`SELECT `+db.JiraProjectColumns+` FROM jira_projects WHERE id = $1 AND org_id = $2 FOR UPDATE`, id, orgID))
		if errors.Is(err, sql.ErrNoRows) {
			return db.ErrNoSuchJiraProject
		}
		if err != nil {
			return fmt.Errorf("read jira project %s: %w", id, err)
		}
		if row.ExternalID != "" && row.ExternalID != externalID {
			return db.ErrJiraProjectIdentityConflict
		}
		target := id
		if row.ExternalID == "" {
			var other string
			err := tx.QueryRowContext(ctx, `
				SELECT id FROM jira_projects
				 WHERE org_id = $1 AND site = $2 AND external_id = $3 AND id <> $4`,
				orgID, row.Site, externalID, id).Scan(&other)
			switch {
			case err == nil:
				if err := mergeJiraProject(ctx, tx, orgID, id, other); err != nil {
					return err
				}
				target = other
			case !errors.Is(err, sql.ErrNoRows):
				return fmt.Errorf("find jira project %s: %w", externalID, err)
			}
		}
		out, err = db.ScanJiraProject(tx.QueryRowContext(ctx, `
			UPDATE jira_projects
			   SET external_id = $2, project_key = $3, project_name = $4
			 WHERE id = $1
			RETURNING `+db.JiraProjectColumns,
			target, externalID, key, name))
		if err != nil {
			return fmt.Errorf("refresh jira project %s: %w", target, err)
		}
		return nil
	})
	if err != nil {
		return domain.JiraProject{}, err
	}
	return out, nil
}

// mergeJiraProject folds registry row from into row to: from's rules move to
// to, except where the team already has a rule for to, which it keeps; every
// team's display order names to where it named from; and from is deleted.
func mergeJiraProject(ctx context.Context, tx queryer, orgID, from, to string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE jira_project_status_rules SET jira_project_id = $1
		 WHERE jira_project_id = $2
		   AND team_id NOT IN (SELECT team_id FROM jira_project_status_rules WHERE jira_project_id = $1)`,
		to, from); err != nil {
		return fmt.Errorf("move jira rules to %s: %w", to, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM jira_project_status_rules WHERE jira_project_id = $1`, from); err != nil {
		return fmt.Errorf("drop jira rules of %s: %w", from, err)
	}

	type teamOrder struct {
		teamID string
		order  []string
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT ts.team_id, array_to_json(ts.jira_projects)::text
		  FROM team_settings ts
		  JOIN teams t ON t.id = ts.team_id
		 WHERE t.org_id = $1 AND $2 = ANY (ts.jira_projects)`,
		orgID, from)
	if err != nil {
		return fmt.Errorf("read team project orders: %w", err)
	}
	var orders []teamOrder
	for rows.Next() {
		var (
			o   teamOrder
			raw string
		)
		if err := rows.Scan(&o.teamID, &raw); err != nil {
			rows.Close()
			return fmt.Errorf("scan team project order: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &o.order); err != nil {
			rows.Close()
			return fmt.Errorf("unmarshal team_settings.jira_projects: %w", err)
		}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, o := range orders {
		if _, err := tx.ExecContext(ctx,
			`UPDATE team_settings SET jira_projects = $2 WHERE team_id = $1`,
			o.teamID, db.ReplaceJiraProjectInOrder(o.order, from, to)); err != nil {
			return fmt.Errorf("rewrite project order of team %s: %w", o.teamID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM jira_projects WHERE id = $1`, from); err != nil {
		return fmt.Errorf("delete merged jira project %s: %w", from, err)
	}
	return nil
}
