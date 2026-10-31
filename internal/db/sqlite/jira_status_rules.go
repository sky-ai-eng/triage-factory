package sqlite

import (
	"context"
	"database/sql"
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

func (s *jiraStatusRulesStore) ListForTeam(ctx context.Context, teamID, site string) ([]domain.JiraProjectStatusRules, error) {
	return listJiraStatusRules(ctx, s.q, teamID, site)
}

func (s *jiraStatusRulesStore) ListForTeamSystem(ctx context.Context, teamID, site string) ([]domain.JiraProjectStatusRules, error) {
	return listJiraStatusRules(ctx, s.q, teamID, site)
}

// jiraRuleCols is the SELECT list every reader shares, over the rules (alias
// r) joined to their registry row (alias p) — team_id first so TeamID is
// populated, then the project as the registry holds it now, then each rule's
// columns in the order the scanner binds them.
const jiraRuleCols = `r.team_id, r.jira_project_id, p.project_key, p.project_name, COALESCE(p.external_id, ''),
	       r.pickup_members,
	       r.in_progress_members, r.in_progress_canonical,
	       r.in_review_members, r.in_review_canonical,
	       r.done_members, r.done_canonical`

// jiraRuleFrom joins each rule to its registry row and confines the read to
// one site, bound first. An empty site matches no row: every registry row
// carries the site it was saved under.
const jiraRuleFrom = `FROM jira_project_status_rules r
		JOIN jira_projects p ON p.id = r.jira_project_id
		WHERE p.site = ? AND p.site <> ''`

func (s *jiraStatusRulesStore) ListForOrgSystem(ctx context.Context, orgID, site string) ([]domain.JiraProjectStatusRules, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// SQLite is single-org, so the union is every rule on the site. team_id
	// rides each row and is scanned into TeamID for the poller's per-project
	// merge tie-break.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+jiraRuleCols+`
		`+jiraRuleFrom+`
		ORDER BY p.project_key ASC, r.jira_project_id ASC, r.team_id ASC
	`, site)
	return scanJiraStatusRules(rows, err)
}

func (s *jiraStatusRulesStore) TracksJiraProjectSystem(ctx context.Context, teamID, site, jiraProjectID string) (bool, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT 1 `+jiraRuleFrom+`
		  AND r.team_id = ? AND r.jira_project_id = ?
		LIMIT 1
	`, site, teamID, jiraProjectID).Scan(&n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("tracks project: %w", err)
	}
	return true, nil
}

func (s *jiraStatusRulesStore) ArmingTeamIDsSystem(ctx context.Context, orgID, site, jiraProjectID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+jiraRuleCols+`
		`+jiraRuleFrom+`
		  AND r.jira_project_id = ?
		  AND EXISTS (SELECT 1 FROM teams t WHERE t.id = r.team_id AND t.deleted_at IS NULL)
	`, site, jiraProjectID)
	rules, err := scanJiraStatusRules(rows, err)
	if err != nil {
		return nil, fmt.Errorf("list teams arming jira project: %w", err)
	}
	return db.ArmingTeamIDs(rules), nil
}

func listJiraStatusRules(ctx context.Context, q queryer, teamID, site string) ([]domain.JiraProjectStatusRules, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+jiraRuleCols+`
		`+jiraRuleFrom+`
		  AND r.team_id = ?
		ORDER BY p.project_key ASC, r.jira_project_id ASC
	`, site, teamID)
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
			r                                                  domain.JiraProjectStatusRules
			pickupJSON, inProgressJSON, inReviewJSON, doneJSON string
			inProgressCanon, inReviewCanon, doneCanon          sql.NullString
		)
		if err := rows.Scan(&r.TeamID, &r.JiraProjectID, &r.ProjectKey, &r.ProjectName, &r.ProjectExternalID, &pickupJSON,
			&inProgressJSON, &inProgressCanon,
			&inReviewJSON, &inReviewCanon,
			&doneJSON, &doneCanon); err != nil {
			return nil, fmt.Errorf("scan jira_project_status_rules: %w", err)
		}
		pickup, err := domain.UnmarshalJiraStatusRefs(pickupJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal pickup_members for %s: %w", r.ProjectKey, err)
		}
		inProgress, err := domain.UnmarshalJiraStatusRefs(inProgressJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal in_progress_members for %s: %w", r.ProjectKey, err)
		}
		inReview, err := domain.UnmarshalJiraStatusRefs(inReviewJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal in_review_members for %s: %w", r.ProjectKey, err)
		}
		done, err := domain.UnmarshalJiraStatusRefs(doneJSON)
		if err != nil {
			return nil, fmt.Errorf("unmarshal done_members for %s: %w", r.ProjectKey, err)
		}
		inProgressCanonical, err := domain.UnmarshalJiraStatusRef(inProgressCanon.String)
		if err != nil {
			return nil, fmt.Errorf("unmarshal in_progress_canonical for %s: %w", r.ProjectKey, err)
		}
		inReviewCanonical, err := domain.UnmarshalJiraStatusRef(inReviewCanon.String)
		if err != nil {
			return nil, fmt.Errorf("unmarshal in_review_canonical for %s: %w", r.ProjectKey, err)
		}
		doneCanonical, err := domain.UnmarshalJiraStatusRef(doneCanon.String)
		if err != nil {
			return nil, fmt.Errorf("unmarshal done_canonical for %s: %w", r.ProjectKey, err)
		}
		r.PickupMembers = pickup
		r.InProgressMembers = inProgress
		r.InProgressCanonical = inProgressCanonical
		r.InReviewMembers = inReview
		r.InReviewCanonical = inReviewCanonical
		r.DoneMembers = done
		r.DoneCanonical = doneCanonical
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *jiraStatusRulesStore) ReplaceForTeam(ctx context.Context, teamID, site string, rules []domain.JiraProjectStatusRules) error {
	if err := db.ValidateJiraStatusRulesInput(site, rules); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(tx queryer) error {
		for _, r := range rules {
			pickupJSON, err := domain.MarshalJiraStatusRefs(r.PickupMembers)
			if err != nil {
				return fmt.Errorf("marshal pickup_members for %s: %w", r.ProjectKey, err)
			}
			inProgressJSON, err := domain.MarshalJiraStatusRefs(r.InProgressMembers)
			if err != nil {
				return fmt.Errorf("marshal in_progress_members for %s: %w", r.ProjectKey, err)
			}
			inReviewJSON, err := domain.MarshalJiraStatusRefs(r.InReviewMembers)
			if err != nil {
				return fmt.Errorf("marshal in_review_members for %s: %w", r.ProjectKey, err)
			}
			doneJSON, err := domain.MarshalJiraStatusRefs(r.DoneMembers)
			if err != nil {
				return fmt.Errorf("marshal done_members for %s: %w", r.ProjectKey, err)
			}
			inProgressCanonJSON, err := domain.MarshalJiraStatusRef(r.InProgressCanonical)
			if err != nil {
				return fmt.Errorf("marshal in_progress_canonical for %s: %w", r.ProjectKey, err)
			}
			inReviewCanonJSON, err := domain.MarshalJiraStatusRef(r.InReviewCanonical)
			if err != nil {
				return fmt.Errorf("marshal in_review_canonical for %s: %w", r.ProjectKey, err)
			}
			doneCanonJSON, err := domain.MarshalJiraStatusRef(r.DoneCanonical)
			if err != nil {
				return fmt.Errorf("marshal done_canonical for %s: %w", r.ProjectKey, err)
			}
			// The SELECT resolves org_id from the team and admits the registry
			// row only on the site being saved and in the team's org, so a rule
			// naming any other row inserts nothing and is refused below.
			res, err := tx.ExecContext(ctx, `
				INSERT INTO jira_project_status_rules (
					team_id, jira_project_id, org_id,
					pickup_members, in_progress_members, in_progress_canonical,
					in_review_members, in_review_canonical,
					done_members, done_canonical, updated_at
				)
				SELECT t.id, p.id, t.org_id, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP
				  FROM teams t
				  JOIN jira_projects p ON p.org_id = t.org_id
				 WHERE t.id = ? AND p.id = ? AND p.site = ?
				ON CONFLICT(team_id, jira_project_id) DO UPDATE SET
					pickup_members = excluded.pickup_members,
					in_progress_members = excluded.in_progress_members,
					in_progress_canonical = excluded.in_progress_canonical,
					in_review_members = excluded.in_review_members,
					in_review_canonical = excluded.in_review_canonical,
					done_members = excluded.done_members,
					done_canonical = excluded.done_canonical,
					updated_at = CURRENT_TIMESTAMP
			`,
				pickupJSON,
				inProgressJSON, nullStringValue(inProgressCanonJSON),
				inReviewJSON, nullStringValue(inReviewCanonJSON),
				doneJSON, nullStringValue(doneCanonJSON),
				teamID, r.JiraProjectID, site,
			)
			if err != nil {
				return fmt.Errorf("upsert jira_project_status_rules[%s]: %w", r.JiraProjectID, err)
			}
			if n, err := res.RowsAffected(); err != nil {
				return fmt.Errorf("upsert jira_project_status_rules[%s]: %w", r.JiraProjectID, err)
			} else if n == 0 {
				return fmt.Errorf("upsert jira_project_status_rules[%s]: %w", r.JiraProjectID, db.ErrJiraRuleProjectNotOnSite)
			}
		}

		// Prune the team's rules for projects on this site that the input no
		// longer names. SQLite has no array binding, so the NOT IN list is
		// built per call; an empty input takes the unconditional delete,
		// since NOT IN () would keep every row. A rule for a project on
		// another site is never this save's to delete.
		onSite := `jira_project_id IN (SELECT id FROM jira_projects WHERE site = ?)`
		if len(rules) == 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM jira_project_status_rules WHERE team_id = ? AND `+onSite,
				teamID, site,
			); err != nil {
				return fmt.Errorf("clear jira_project_status_rules: %w", err)
			}
			return nil
		}
		placeholders := make([]string, len(rules))
		args := make([]any, 0, len(rules)+2)
		args = append(args, teamID, site)
		for i, r := range rules {
			placeholders[i] = "?"
			args = append(args, r.JiraProjectID)
		}
		query := fmt.Sprintf(
			`DELETE FROM jira_project_status_rules WHERE team_id = ? AND %s AND jira_project_id NOT IN (%s)`,
			onSite, strings.Join(placeholders, ", "),
		)
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("prune jira_project_status_rules: %w", err)
		}
		return nil
	})
}
