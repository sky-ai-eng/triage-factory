package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jiraStatusRulesStore is the Postgres impl of db.JiraStatusRulesStore.
// Holds both pools — see the JiraStatusRulesStore interface comment
// for the pool-split rationale.
//
//   - admin: ListForTeamSystem. Boot-time pollers/scorer without a
//     JWT-claims context.
//   - app: ListForTeam, ReplaceForTeam. Request-handler reads/writes
//     gated by jira_rules_select / jira_rules_insert /
//     jira_rules_update / jira_rules_delete (team membership / team
//     admin).
type jiraStatusRulesStore struct {
	app   queryer
	admin queryer
}

func newJiraStatusRulesStore(app, admin queryer) db.JiraStatusRulesStore {
	return &jiraStatusRulesStore{app: app, admin: admin}
}

var _ db.JiraStatusRulesStore = (*jiraStatusRulesStore)(nil)

func (s *jiraStatusRulesStore) ListForTeam(ctx context.Context, teamID, site string) ([]domain.JiraProjectStatusRules, error) {
	return listJiraStatusRules(ctx, s.app, teamID, site)
}

func (s *jiraStatusRulesStore) ListForTeamSystem(ctx context.Context, teamID, site string) ([]domain.JiraProjectStatusRules, error) {
	return listJiraStatusRules(ctx, s.admin, teamID, site)
}

// jiraRuleCols is the SELECT list every reader shares, over the rules (alias
// r) joined to their registry row (alias p) — team_id first so TeamID is
// populated, then the project as the registry holds it now, then the jsonb
// columns rendered as text, which is what database/sql + pgx stdlib hands a
// Go string. The members and the canonicals are jsonb rather than text[] /
// text because a rule member is a status REFERENCE — the workflow's stable id
// plus the display name captured with it — and an array of scalars has
// nowhere to put the second half.
const jiraRuleCols = `r.team_id, r.jira_project_id, p.project_key, p.project_name, COALESCE(p.external_id, ''),
	       r.pickup_members::text,
	       r.in_progress_members::text,
	       r.in_progress_canonical::text,
	       r.in_review_members::text,
	       r.in_review_canonical::text,
	       r.done_members::text,
	       r.done_canonical::text`

// jiraRuleFrom joins each rule to its registry row and confines the read to
// the site bound as $1. An empty site matches no row: every registry row
// carries the site it was saved under.
const jiraRuleFrom = `FROM jira_project_status_rules r
		JOIN jira_projects p ON p.id = r.jira_project_id
		WHERE p.site = $1 AND p.site <> ''`

func (s *jiraStatusRulesStore) ListForOrgSystem(ctx context.Context, orgID, site string) ([]domain.JiraProjectStatusRules, error) {
	// Admin pool — the union spans teams the caller may not belong to.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+jiraRuleCols+`
		`+jiraRuleFrom+`
		  AND r.org_id = $2
		ORDER BY p.project_key ASC, r.jira_project_id ASC, r.team_id ASC
	`, site, orgID)
	return scanJiraStatusRules(rows, err)
}

// TracksJiraProjectSystem compares the id as text, so an id that is not a uuid
// (metadata is external input to this read) answers false rather than failing
// the cast.
func (s *jiraStatusRulesStore) TracksJiraProjectSystem(ctx context.Context, teamID, site, jiraProjectID string) (bool, error) {
	var n int
	err := s.admin.QueryRowContext(ctx, `
		SELECT 1 `+jiraRuleFrom+`
		  AND r.team_id = $2 AND r.jira_project_id::text = $3
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

func listJiraStatusRules(ctx context.Context, q queryer, teamID, site string) ([]domain.JiraProjectStatusRules, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+jiraRuleCols+`
		`+jiraRuleFrom+`
		  AND r.team_id = $2
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
	return inTx(ctx, s.app, func(tx queryer) error {
		for _, r := range rules {
			cols, err := marshalJiraRule(r)
			if err != nil {
				return err
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
				SELECT t.id, p.id, t.org_id,
				       $4::jsonb, $5::jsonb, $6::jsonb, $7::jsonb, $8::jsonb, $9::jsonb, $10::jsonb, now()
				  FROM teams t
				  JOIN jira_projects p ON p.org_id = t.org_id
				 WHERE t.id = $1 AND p.id = $2 AND p.site = $3
				ON CONFLICT (team_id, jira_project_id) DO UPDATE SET
					pickup_members = EXCLUDED.pickup_members,
					in_progress_members = EXCLUDED.in_progress_members,
					in_progress_canonical = EXCLUDED.in_progress_canonical,
					in_review_members = EXCLUDED.in_review_members,
					in_review_canonical = EXCLUDED.in_review_canonical,
					done_members = EXCLUDED.done_members,
					done_canonical = EXCLUDED.done_canonical,
					updated_at = now()
			`,
				teamID, r.JiraProjectID, site,
				cols.pickup,
				cols.inProgress, nullString(cols.inProgressCanonical),
				cols.inReview, nullString(cols.inReviewCanonical),
				cols.done, nullString(cols.doneCanonical),
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
		// longer names. <> ALL against an empty list keeps nothing back, so
		// an empty input clears the team's rules on the site. A rule for a
		// project on another site is never this save's to delete.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM jira_project_status_rules
			 WHERE team_id = $1
			   AND jira_project_id IN (SELECT id FROM jira_projects WHERE site = $2)
			   AND jira_project_id <> ALL($3::uuid[])`,
			teamID, site, jiraProjectIDs(rules),
		); err != nil {
			return fmt.Errorf("prune jira_project_status_rules: %w", err)
		}
		return nil
	})
}

func jiraProjectIDs(rules []domain.JiraProjectStatusRules) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.JiraProjectID)
	}
	return out
}

// jiraRuleJSON is one rule rendered into the JSON the upsert binds, one field
// per column. A canonical is "" when unset, which nullString turns into the SQL
// NULL the complete-or-empty CHECK constraints expect.
type jiraRuleJSON struct {
	pickup              string
	inProgress          string
	inProgressCanonical string
	inReview            string
	inReviewCanonical   string
	done                string
	doneCanonical       string
}

// marshalJiraRule renders one rule's JSON columns, naming the column that
// failed so a bad ref is traceable to the rule it came from.
func marshalJiraRule(r domain.JiraProjectStatusRules) (jiraRuleJSON, error) {
	var (
		cols jiraRuleJSON
		err  error
	)
	if cols.pickup, err = domain.MarshalJiraStatusRefs(r.PickupMembers); err != nil {
		return jiraRuleJSON{}, fmt.Errorf("marshal pickup_members for %s: %w", r.ProjectKey, err)
	}
	if cols.inProgress, err = domain.MarshalJiraStatusRefs(r.InProgressMembers); err != nil {
		return jiraRuleJSON{}, fmt.Errorf("marshal in_progress_members for %s: %w", r.ProjectKey, err)
	}
	if cols.inProgressCanonical, err = domain.MarshalJiraStatusRef(r.InProgressCanonical); err != nil {
		return jiraRuleJSON{}, fmt.Errorf("marshal in_progress_canonical for %s: %w", r.ProjectKey, err)
	}
	if cols.inReview, err = domain.MarshalJiraStatusRefs(r.InReviewMembers); err != nil {
		return jiraRuleJSON{}, fmt.Errorf("marshal in_review_members for %s: %w", r.ProjectKey, err)
	}
	if cols.inReviewCanonical, err = domain.MarshalJiraStatusRef(r.InReviewCanonical); err != nil {
		return jiraRuleJSON{}, fmt.Errorf("marshal in_review_canonical for %s: %w", r.ProjectKey, err)
	}
	if cols.done, err = domain.MarshalJiraStatusRefs(r.DoneMembers); err != nil {
		return jiraRuleJSON{}, fmt.Errorf("marshal done_members for %s: %w", r.ProjectKey, err)
	}
	if cols.doneCanonical, err = domain.MarshalJiraStatusRef(r.DoneCanonical); err != nil {
		return jiraRuleJSON{}, fmt.Errorf("marshal done_canonical for %s: %w", r.ProjectKey, err)
	}
	return cols, nil
}
