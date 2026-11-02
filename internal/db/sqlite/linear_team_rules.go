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

// linearTeamRulesStore is the SQLite impl of db.LinearTeamRulesStore. The rule
// columns are JSON text. The constructor takes two queryers for signature
// parity with Postgres; SQLite has one connection, so both collapse and every
// ...System method is its non-System counterpart.
type linearTeamRulesStore struct{ q queryer }

func newLinearTeamRulesStore(q, _ queryer) db.LinearTeamRulesStore {
	return &linearTeamRulesStore{q: q}
}

var _ db.LinearTeamRulesStore = (*linearTeamRulesStore)(nil)

// linearRuleCols is the projection db.ScanLinearTeamRules reads, in its order.
const linearRuleCols = `team_id, linear_team_id, linear_team_key, linear_team_name,
	       pickup_members,
	       in_progress_members, in_progress_canonical,
	       in_review_members, in_review_canonical,
	       done_members, done_canonical`

func (s *linearTeamRulesStore) ListForTeam(ctx context.Context, teamID, workspaceID string) ([]domain.LinearTeamRules, error) {
	return listLinearTeamRules(ctx, s.q, teamID, workspaceID)
}

func (s *linearTeamRulesStore) ListForTeamSystem(ctx context.Context, teamID, workspaceID string) ([]domain.LinearTeamRules, error) {
	return listLinearTeamRules(ctx, s.q, teamID, workspaceID)
}

func (s *linearTeamRulesStore) ListForOrgSystem(ctx context.Context, orgID, workspaceID string) ([]domain.LinearTeamRules, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Single-org, so the union is every live team's row in the workspace; the
	// teams lookup only leaves out an archived team. An empty workspace id
	// matches no row, since every row carries the one it was saved under.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+linearRuleCols+`
		FROM linear_team_rules
		WHERE linear_workspace_id = ? AND linear_workspace_id <> ''
		  AND EXISTS (SELECT 1 FROM teams t WHERE t.id = linear_team_rules.team_id AND t.deleted_at IS NULL)
		ORDER BY linear_team_id ASC, team_id ASC
	`, workspaceID)
	return db.ScanLinearTeamRulesRows(rows, err)
}

func (s *linearTeamRulesStore) TracksTeamSystem(ctx context.Context, teamID, workspaceID, linearTeamID string) (bool, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT 1 FROM linear_team_rules
		WHERE team_id = ? AND linear_workspace_id = ? AND linear_workspace_id <> '' AND linear_team_id = ?
		LIMIT 1
	`, teamID, workspaceID, linearTeamID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("tracks linear team: %w", err)
	}
	return true, nil
}

func (s *linearTeamRulesStore) TrackingTeamIDsSystem(ctx context.Context, orgID, workspaceID, linearTeamID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT r.team_id
		FROM linear_team_rules r
		JOIN teams t ON t.id = r.team_id
		WHERE t.deleted_at IS NULL
		  AND r.linear_workspace_id = ? AND r.linear_workspace_id <> '' AND r.linear_team_id = ?
		ORDER BY 1 ASC
	`, workspaceID, linearTeamID)
	if err != nil {
		return nil, fmt.Errorf("list teams tracking linear team: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan linear_team_rules.team_id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func listLinearTeamRules(ctx context.Context, q queryer, teamID, workspaceID string) ([]domain.LinearTeamRules, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+linearRuleCols+`
		FROM linear_team_rules
		WHERE team_id = ? AND linear_workspace_id = ? AND linear_workspace_id <> ''
		ORDER BY linear_team_id ASC
	`, teamID, workspaceID)
	return db.ScanLinearTeamRulesRows(rows, err)
}

func (s *linearTeamRulesStore) ReplaceForTeam(ctx context.Context, teamID, workspaceID string, rules []domain.LinearTeamRules) ([]domain.LinearTeamRules, error) {
	if err := db.ValidateLinearTeamRulesInput(workspaceID, rules); err != nil {
		return nil, err
	}
	var stored []domain.LinearTeamRules
	err := inTx(ctx, s.q, func(tx queryer) error {
		for _, r := range rules {
			cols, err := db.MarshalLinearTeamRules(r)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO linear_team_rules (
					team_id, linear_workspace_id, linear_team_id, linear_team_key, linear_team_name,
					pickup_members, in_progress_members, in_progress_canonical,
					in_review_members, in_review_canonical,
					done_members, done_canonical, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
				ON CONFLICT(team_id, linear_team_id) DO UPDATE SET
					linear_workspace_id = excluded.linear_workspace_id,
					linear_team_key = excluded.linear_team_key,
					linear_team_name = excluded.linear_team_name,
					pickup_members = excluded.pickup_members,
					in_progress_members = excluded.in_progress_members,
					in_progress_canonical = excluded.in_progress_canonical,
					in_review_members = excluded.in_review_members,
					in_review_canonical = excluded.in_review_canonical,
					done_members = excluded.done_members,
					done_canonical = excluded.done_canonical,
					updated_at = CURRENT_TIMESTAMP
			`,
				teamID, workspaceID, r.LinearTeamID, r.LinearTeamKey, r.LinearTeamName,
				cols.Pickup,
				cols.InProgress, nullStringValue(cols.InProgressCanonical),
				cols.InReview, nullStringValue(cols.InReviewCanonical),
				cols.Done, nullStringValue(cols.DoneCanonical),
			); err != nil {
				return fmt.Errorf("upsert linear_team_rules[%s]: %w", r.LinearTeamID, err)
			}
		}

		// Prune the workspace's rows the input no longer names. SQLite has no
		// array binding, so the NOT IN list is built per call; an empty input
		// takes the unconditional delete, since NOT IN () would keep every
		// row. Another workspace's rows are never this save's to delete.
		if len(rules) == 0 {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM linear_team_rules WHERE team_id = ? AND linear_workspace_id = ?`,
				teamID, workspaceID,
			); err != nil {
				return fmt.Errorf("clear linear_team_rules: %w", err)
			}
		} else {
			placeholders := make([]string, len(rules))
			args := make([]any, 0, len(rules)+2)
			args = append(args, teamID, workspaceID)
			for i, r := range rules {
				placeholders[i] = "?"
				args = append(args, r.LinearTeamID)
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM linear_team_rules WHERE team_id = ? AND linear_workspace_id = ? AND linear_team_id NOT IN (`+strings.Join(placeholders, ", ")+`)`,
				args...,
			); err != nil {
				return fmt.Errorf("prune linear_team_rules: %w", err)
			}
		}

		var err error
		stored, err = listLinearTeamRules(ctx, tx, teamID, workspaceID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}
