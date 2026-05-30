package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jiraStatusRulesStore is the SQLite impl of db.JiraStatusRulesStore.
// SQLite has no array type, so members[] are stored as JSON text. The
// constructor accepts two queryers for signature parity with the
// Postgres impl; SQLite has one connection so both collapse. The
// `...System` variant delegates to its non-System counterpart.
type jiraStatusRulesStore struct{ q queryer }

func newJiraStatusRulesStore(q, _ queryer) db.JiraStatusRulesStore {
	return &jiraStatusRulesStore{q: q}
}

var _ db.JiraStatusRulesStore = (*jiraStatusRulesStore)(nil)

func (s *jiraStatusRulesStore) ListForTeam(ctx context.Context, teamID string) ([]domain.JiraProjectStatusRules, error) {
	return listJiraStatusRules(ctx, s.q, teamID)
}

func (s *jiraStatusRulesStore) ListForTeamSystem(ctx context.Context, teamID string) ([]domain.JiraProjectStatusRules, error) {
	return listJiraStatusRules(ctx, s.q, teamID)
}

func (s *jiraStatusRulesStore) ListForOrgSystem(ctx context.Context, orgID string) ([]domain.JiraProjectStatusRules, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// SQLite is single-org, so the union is every row. team_id rides each
	// row (no teams join needed) and is scanned into TeamID for the
	// poller's per-project merge tie-break.
	rows, err := s.q.QueryContext(ctx, `
		SELECT team_id, project_key,
		       pickup_members, in_progress_members, in_progress_canonical,
		       done_members, done_canonical
		FROM jira_project_status_rules
		ORDER BY project_key ASC, team_id ASC
	`)
	return scanJiraStatusRules(rows, err)
}

func (s *jiraStatusRulesStore) TracksProjectSystem(ctx context.Context, teamID, projectKey string) (bool, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT 1 FROM jira_project_status_rules
		WHERE team_id = ? AND project_key = ?
		LIMIT 1
	`, teamID, projectKey).Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("tracks project: %w", err)
	}
	return true, nil
}

func listJiraStatusRules(ctx context.Context, q queryer, teamID string) ([]domain.JiraProjectStatusRules, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT team_id, project_key,
		       pickup_members, in_progress_members, in_progress_canonical,
		       done_members, done_canonical
		FROM jira_project_status_rules
		WHERE team_id = ?
		ORDER BY project_key ASC
	`, teamID)
	return scanJiraStatusRules(rows, err)
}

func scanJiraStatusRules(rows *sql.Rows, err error) ([]domain.JiraProjectStatusRules, error) {
	if err != nil {
		return nil, fmt.Errorf("read jira_project_status_rules: %w", err)
	}
	defer rows.Close()
	out := []domain.JiraProjectStatusRules{}
	for rows.Next() {
		var (
			r                                    domain.JiraProjectStatusRules
			pickupJSON, inProgressJSON, doneJSON string
			inProgressCanon, doneCanon           sql.NullString
		)
		if err := rows.Scan(&r.TeamID, &r.ProjectKey, &pickupJSON, &inProgressJSON, &inProgressCanon, &doneJSON, &doneCanon); err != nil {
			return nil, fmt.Errorf("scan jira_project_status_rules: %w", err)
		}
		pickup, err := unmarshalStringSlice(pickupJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal pickup_members for %s: %w", r.ProjectKey, err)
		}
		inProgress, err := unmarshalStringSlice(inProgressJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal in_progress_members for %s: %w", r.ProjectKey, err)
		}
		done, err := unmarshalStringSlice(doneJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal done_members for %s: %w", r.ProjectKey, err)
		}
		r.PickupMembers = pickup
		r.InProgressMembers = inProgress
		r.InProgressCanonical = inProgressCanon.String
		r.DoneMembers = done
		r.DoneCanonical = doneCanon.String
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *jiraStatusRulesStore) ReplaceForTeam(ctx context.Context, teamID string, rules []domain.JiraProjectStatusRules) error {
	// Refuse empty ProjectKey up front — see the Postgres impl's
	// matching guard. Clear-all semantics stay exclusively gated on
	// rules being nil/empty so an all-empty input doesn't silently
	// wipe the team's rows.
	for i, r := range rules {
		if r.ProjectKey == "" {
			return fmt.Errorf("ReplaceForTeam: rules[%d] has empty ProjectKey", i)
		}
	}
	return inTx(ctx, s.q, func(tx queryer) error {
		for _, r := range rules {
			pickupJSON, err := marshalJSONArray(r.PickupMembers)
			if err != nil {
				return fmt.Errorf("marshal pickup_members for %s: %w", r.ProjectKey, err)
			}
			inProgressJSON, err := marshalJSONArray(r.InProgressMembers)
			if err != nil {
				return fmt.Errorf("marshal in_progress_members for %s: %w", r.ProjectKey, err)
			}
			doneJSON, err := marshalJSONArray(r.DoneMembers)
			if err != nil {
				return fmt.Errorf("marshal done_members for %s: %w", r.ProjectKey, err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO jira_project_status_rules (
					team_id, project_key,
					pickup_members, in_progress_members, in_progress_canonical,
					done_members, done_canonical, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
				ON CONFLICT(team_id, project_key) DO UPDATE SET
					pickup_members = excluded.pickup_members,
					in_progress_members = excluded.in_progress_members,
					in_progress_canonical = excluded.in_progress_canonical,
					done_members = excluded.done_members,
					done_canonical = excluded.done_canonical,
					updated_at = CURRENT_TIMESTAMP
			`,
				teamID, r.ProjectKey,
				pickupJSON, inProgressJSON, nullStringValue(r.InProgressCanonical),
				doneJSON, nullStringValue(r.DoneCanonical),
			); err != nil {
				return fmt.Errorf("upsert jira_project_status_rules[%s]: %w", r.ProjectKey, err)
			}
		}

		// Prune rows for project keys no longer in the input. SQLite
		// has no array binding; build the placeholder list dynamically.
		// Empty ProjectKey entries can't reach here — the pre-loop
		// validation refuses them.
		keys := make([]string, 0, len(rules))
		for _, r := range rules {
			keys = append(keys, r.ProjectKey)
		}
		if len(keys) == 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM jira_project_status_rules WHERE team_id = ?`,
				teamID,
			); err != nil {
				return fmt.Errorf("clear jira_project_status_rules: %w", err)
			}
			return nil
		}
		placeholders := make([]string, len(keys))
		args := make([]any, 0, len(keys)+1)
		args = append(args, teamID)
		for i, k := range keys {
			placeholders[i] = "?"
			args = append(args, k)
		}
		query := fmt.Sprintf(
			`DELETE FROM jira_project_status_rules WHERE team_id = ? AND project_key NOT IN (%s)`,
			strings.Join(placeholders, ", "),
		)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("prune jira_project_status_rules: %w", err)
		}
		return nil
	})
}

func unmarshalStringSlice(s string) ([]string, error) {
	if s == "" {
		return []string{}, nil
	}
	out := []string{}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}
