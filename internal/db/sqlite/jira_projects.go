package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jiraProjectStore is the SQLite impl of db.JiraProjectStore. The constructor
// takes two queryers for signature parity with Postgres; SQLite has one
// connection, so both collapse.
type jiraProjectStore struct{ q queryer }

func newJiraProjectStore(q, _ queryer) db.JiraProjectStore {
	return &jiraProjectStore{q: q}
}

var _ db.JiraProjectStore = (*jiraProjectStore)(nil)

func (s *jiraProjectStore) GetSystem(ctx context.Context, orgID, id string) (*domain.JiraProject, error) {
	p, err := db.ScanJiraProject(s.q.QueryRowContext(ctx,
		`SELECT `+db.JiraProjectColumns+` FROM jira_projects WHERE id = ? AND org_id = ?`, id, orgID))
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
	err := inTx(ctx, s.q, func(tx queryer) error {
		// An unresolved row under the key is this project, unless another row
		// already carries the id; then that row is the project and the
		// unresolved one is left for the poll to merge.
		p, err := db.ScanJiraProject(tx.QueryRowContext(ctx, `
			UPDATE jira_projects
			   SET external_id = ?, project_name = ?, updated_at = CURRENT_TIMESTAMP
			 WHERE org_id = ? AND site = ? AND project_key = ? AND external_id IS NULL
			   AND NOT EXISTS (
			       SELECT 1 FROM jira_projects o
			        WHERE o.org_id = ? AND o.site = ? AND o.external_id = ?)
			RETURNING `+db.JiraProjectColumns,
			externalID, name, orgID, site, key, orgID, site, externalID))
		if err == nil {
			out = p
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("resolve jira project %s: %w", key, err)
		}
		out, err = db.ScanJiraProject(tx.QueryRowContext(ctx, `
			INSERT INTO jira_projects (id, org_id, site, external_id, project_key, project_name)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (org_id, site, external_id) WHERE external_id IS NOT NULL DO UPDATE SET
				project_key = excluded.project_key,
				project_name = excluded.project_name,
				updated_at = CURRENT_TIMESTAMP
			RETURNING `+db.JiraProjectColumns,
			uuid.New().String(), orgID, site, externalID, key, name))
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
	err := inTx(ctx, s.q, func(tx queryer) error {
		row, err := db.ScanJiraProject(tx.QueryRowContext(ctx,
			`SELECT `+db.JiraProjectColumns+` FROM jira_projects WHERE id = ? AND org_id = ?`, id, orgID))
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
				 WHERE org_id = ? AND site = ? AND external_id = ? AND id <> ?`,
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
			   SET external_id = ?, project_key = ?, project_name = ?, updated_at = CURRENT_TIMESTAMP
			 WHERE id = ?
			RETURNING `+db.JiraProjectColumns,
			externalID, key, name, target))
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
		UPDATE jira_project_status_rules SET jira_project_id = ?
		 WHERE jira_project_id = ?
		   AND team_id NOT IN (SELECT team_id FROM jira_project_status_rules WHERE jira_project_id = ?)`,
		to, from, to); err != nil {
		return fmt.Errorf("move jira rules to %s: %w", to, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM jira_project_status_rules WHERE jira_project_id = ?`, from); err != nil {
		return fmt.Errorf("drop jira rules of %s: %w", from, err)
	}

	type teamOrder struct {
		teamID string
		order  []string
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT ts.team_id, ts.jira_projects
		  FROM team_settings ts
		  JOIN teams t ON t.id = ts.team_id
		 WHERE t.org_id = ? AND json_valid(ts.jira_projects)
		   AND EXISTS (SELECT 1 FROM json_each(ts.jira_projects) j WHERE j.value = ?)`,
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
		next, err := marshalJSONArray(replaceJiraProjectInOrder(o.order, from, to))
		if err != nil {
			return fmt.Errorf("marshal team_settings.jira_projects: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE team_settings SET jira_projects = ?, updated_at = CURRENT_TIMESTAMP WHERE team_id = ?`,
			next, o.teamID); err != nil {
			return fmt.Errorf("rewrite project order of team %s: %w", o.teamID, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM jira_projects WHERE id = ?`, from); err != nil {
		return fmt.Errorf("delete merged jira project %s: %w", from, err)
	}
	return nil
}

// replaceJiraProjectInOrder is a team's display order of tracked projects with
// the registry row from replaced by to, kept where from stood. When to is
// already listed, from is dropped instead, so the merged project appears once.
func replaceJiraProjectInOrder(order []string, from, to string) []string {
	hasTo := slices.Contains(order, to)
	out := make([]string, 0, len(order))
	for _, id := range order {
		switch {
		case id != from:
			out = append(out, id)
		case !hasTo:
			out = append(out, to)
			hasTo = true
		}
	}
	return out
}
