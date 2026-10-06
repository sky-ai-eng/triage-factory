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
	       done_members, done_canonical`

func (s *linearTeamRulesStore) ListForTeam(ctx context.Context, teamID string) ([]domain.LinearTeamRules, error) {
	return listLinearTeamRules(ctx, s.q, teamID)
}

func (s *linearTeamRulesStore) ListForTeamSystem(ctx context.Context, teamID string) ([]domain.LinearTeamRules, error) {
	return listLinearTeamRules(ctx, s.q, teamID)
}

func (s *linearTeamRulesStore) ListForOrgSystem(ctx context.Context, orgID string) ([]domain.LinearTeamRules, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Single-org, so the union is every row; no teams join needed.
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+linearRuleCols+`
		FROM linear_team_rules
		ORDER BY linear_team_id ASC, team_id ASC
	`)
	return db.ScanLinearTeamRulesRows(rows, err)
}

func (s *linearTeamRulesStore) TracksTeamSystem(ctx context.Context, teamID, linearTeamID string) (bool, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT 1 FROM linear_team_rules
		WHERE team_id = ? AND linear_team_id = ?
		LIMIT 1
	`, teamID, linearTeamID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("tracks linear team: %w", err)
	}
	return true, nil
}

func listLinearTeamRules(ctx context.Context, q queryer, teamID string) ([]domain.LinearTeamRules, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+linearRuleCols+`
		FROM linear_team_rules
		WHERE team_id = ?
		ORDER BY linear_team_id ASC
	`, teamID)
	return db.ScanLinearTeamRulesRows(rows, err)
}

func (s *linearTeamRulesStore) ReplaceForTeam(ctx context.Context, teamID string, rules []domain.LinearTeamRules) ([]domain.LinearTeamRules, error) {
	if err := db.ValidateLinearTeamRulesInput(rules); err != nil {
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
					team_id, linear_team_id, linear_team_key, linear_team_name,
					pickup_members, in_progress_members, in_progress_canonical,
					done_members, done_canonical, updated_at
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
				ON CONFLICT(team_id, linear_team_id) DO UPDATE SET
					linear_team_key = excluded.linear_team_key,
					linear_team_name = excluded.linear_team_name,
					pickup_members = excluded.pickup_members,
					in_progress_members = excluded.in_progress_members,
					in_progress_canonical = excluded.in_progress_canonical,
					done_members = excluded.done_members,
					done_canonical = excluded.done_canonical,
					updated_at = CURRENT_TIMESTAMP
			`,
				teamID, r.LinearTeamID, r.LinearTeamKey, r.LinearTeamName,
				cols.Pickup,
				cols.InProgress, nullStringValue(cols.InProgressCanonical),
				cols.Done, nullStringValue(cols.DoneCanonical),
			); err != nil {
				return fmt.Errorf("upsert linear_team_rules[%s]: %w", r.LinearTeamID, err)
			}
		}

		// Prune the rows the input no longer names. SQLite has no array
		// binding, so the NOT IN list is built per call; an empty input takes
		// the unconditional delete, since NOT IN () would keep every row.
		if len(rules) == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM linear_team_rules WHERE team_id = ?`, teamID); err != nil {
				return fmt.Errorf("clear linear_team_rules: %w", err)
			}
		} else {
			placeholders := make([]string, len(rules))
			args := make([]any, 0, len(rules)+1)
			args = append(args, teamID)
			for i, r := range rules {
				placeholders[i] = "?"
				args = append(args, r.LinearTeamID)
			}
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM linear_team_rules WHERE team_id = ? AND linear_team_id NOT IN (`+strings.Join(placeholders, ", ")+`)`,
				args...,
			); err != nil {
				return fmt.Errorf("prune linear_team_rules: %w", err)
			}
		}

		var err error
		stored, err = listLinearTeamRules(ctx, tx, teamID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return stored, nil
}
