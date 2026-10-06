package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// linearTeamRulesStore is the Postgres impl of db.LinearTeamRulesStore. See
// the interface for the pool split: app for ListForTeam and ReplaceForTeam
// (the linear_rules_* RLS policies gate them), admin for the ...System reads.
type linearTeamRulesStore struct {
	app   queryer
	admin queryer
}

func newLinearTeamRulesStore(app, admin queryer) db.LinearTeamRulesStore {
	return &linearTeamRulesStore{app: app, admin: admin}
}

var _ db.LinearTeamRulesStore = (*linearTeamRulesStore)(nil)

// linearRuleCols is the projection db.ScanLinearTeamRules reads, in its order.
// The jsonb columns render as text, which is what database/sql + pgx stdlib
// hands a Go string.
const linearRuleCols = `r.team_id, r.linear_team_id, r.linear_team_key, r.linear_team_name,
	       r.pickup_members::text,
	       r.in_progress_members::text, r.in_progress_canonical::text,
	       r.done_members::text, r.done_canonical::text`

func (s *linearTeamRulesStore) ListForTeam(ctx context.Context, teamID string) ([]domain.LinearTeamRules, error) {
	return listLinearTeamRules(ctx, s.app, teamID)
}

func (s *linearTeamRulesStore) ListForTeamSystem(ctx context.Context, teamID string) ([]domain.LinearTeamRules, error) {
	return listLinearTeamRules(ctx, s.admin, teamID)
}

func (s *linearTeamRulesStore) ListForOrgSystem(ctx context.Context, orgID string) ([]domain.LinearTeamRules, error) {
	// Admin pool: the union spans teams the caller may not belong to. The org
	// scope rides the teams join; the table carries no org_id.
	rows, err := s.admin.QueryContext(ctx, `
		SELECT `+linearRuleCols+`
		FROM linear_team_rules r
		JOIN teams t ON t.id = r.team_id
		WHERE t.org_id = $1
		ORDER BY r.linear_team_id ASC, r.team_id ASC
	`, orgID)
	return db.ScanLinearTeamRulesRows(rows, err)
}

func (s *linearTeamRulesStore) TracksTeamSystem(ctx context.Context, teamID, linearTeamID string) (bool, error) {
	var n int
	err := s.admin.QueryRowContext(ctx, `
		SELECT 1 FROM linear_team_rules
		WHERE team_id = $1 AND linear_team_id = $2
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
		FROM linear_team_rules r
		WHERE r.team_id = $1
		ORDER BY r.linear_team_id ASC
	`, teamID)
	return db.ScanLinearTeamRulesRows(rows, err)
}

func (s *linearTeamRulesStore) ReplaceForTeam(ctx context.Context, teamID string, rules []domain.LinearTeamRules) ([]domain.LinearTeamRules, error) {
	if err := db.ValidateLinearTeamRulesInput(rules); err != nil {
		return nil, err
	}
	var stored []domain.LinearTeamRules
	err := inTx(ctx, s.app, func(tx queryer) error {
		ids := make([]string, 0, len(rules))
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
				) VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7::jsonb, $8::jsonb, $9::jsonb, now())
				ON CONFLICT (team_id, linear_team_id) DO UPDATE SET
					linear_team_key = EXCLUDED.linear_team_key,
					linear_team_name = EXCLUDED.linear_team_name,
					pickup_members = EXCLUDED.pickup_members,
					in_progress_members = EXCLUDED.in_progress_members,
					in_progress_canonical = EXCLUDED.in_progress_canonical,
					done_members = EXCLUDED.done_members,
					done_canonical = EXCLUDED.done_canonical,
					updated_at = now()
			`,
				teamID, r.LinearTeamID, r.LinearTeamKey, r.LinearTeamName,
				cols.Pickup,
				cols.InProgress, nullString(cols.InProgressCanonical),
				cols.Done, nullString(cols.DoneCanonical),
			); err != nil {
				return fmt.Errorf("upsert linear_team_rules[%s]: %w", r.LinearTeamID, err)
			}
			ids = append(ids, r.LinearTeamID)
		}

		// Prune the rows the input no longer names. <> ALL of an empty array
		// is true for every row, so an empty input clears the team through the
		// same statement.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM linear_team_rules WHERE team_id = $1 AND linear_team_id <> ALL($2)`,
			teamID, ids,
		); err != nil {
			return fmt.Errorf("prune linear_team_rules: %w", err)
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
