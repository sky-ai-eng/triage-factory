package postgres

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// scoreStore is the Postgres impl of db.ScoreStore. Wired against the
// admin pool (see postgres.New): the scorer is a system service that
// must read/write every queued task in the org, regardless of who
// created each one. Running as the per-request tf_app role with RLS
// active would only show the scorer its own creator_user_id rows
// (per the tasks_select / tasks_modify policies in D3), which is
// not what we want.
//
// SQL is written fresh against D3's schema: org_id in every WHERE
// clause as defense in depth, $N placeholders, JSONB extraction for
// snapshot_json.
type scoreStore struct{ q queryer }

func newScoreStore(q queryer) db.ScoreStore { return &scoreStore{q: q} }

var _ db.ScoreStore = (*scoreStore)(nil)

func (s *scoreStore) MarkScoring(ctx context.Context, orgID string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE tasks SET scoring_status = 'in_progress' WHERE org_id = $1 AND id = ANY($2)`,
		orgID, taskIDs)
	return err
}

func (s *scoreStore) ResetScoringToPending(ctx context.Context, orgID string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE tasks SET scoring_status = 'pending' WHERE org_id = $1 AND id = ANY($2)`,
		orgID, taskIDs)
	return err
}

func (s *scoreStore) ResetStaleScoring(ctx context.Context, orgID string) (int, error) {
	// org_id is load-bearing here rather than the usual defense in depth:
	// the per-org runners are concurrent, so an unscoped reset would strip
	// a live cycle's claims on another org.
	res, err := s.q.ExecContext(ctx,
		`UPDATE tasks SET scoring_status = 'pending' WHERE org_id = $1 AND scoring_status = 'in_progress'`,
		orgID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *scoreStore) UpdateTaskScores(ctx context.Context, orgID string, updates []domain.TaskScoreUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	// Single UPDATE ... FROM (VALUES ...) so the whole batch lands in
	// one round-trip. Atomic by construction (single statement =
	// implicit tx in Postgres), so no inTx wrapper needed. Avoids the
	// N-round-trip-per-cycle bottleneck the per-row loop had.
	//
	// Placeholders are emitted with explicit ::uuid/::real/::text casts
	// because the VALUES literal's types are inferred from the first
	// row, and a NULL or empty string there would make later rows fail
	// to coerce. Explicit casts pin every column's type at parse time.
	var (
		rowExprs []string
		args     = []any{orgID}
		n        = 2 // $1 is orgID
	)
	for _, u := range updates {
		rowExprs = append(rowExprs, fmt.Sprintf(
			"($%d::uuid, $%d::real, $%d::real, $%d::text, $%d::text)",
			n, n+1, n+2, n+3, n+4))
		args = append(args, u.ID, u.PriorityScore, u.AutonomySuitability, u.Summary, u.PriorityReasoning)
		n += 5
	}
	// rederive_owed rides the same statement as the scores it is owed for:
	// one write, so no crash can commit a score whose deferred triggers
	// nothing is on the hook to evaluate.
	query := fmt.Sprintf(`
		UPDATE tasks t
		SET priority_score = v.priority_score,
		    autonomy_suitability = v.autonomy_suitability,
		    ai_summary = v.ai_summary,
		    priority_reasoning = v.priority_reasoning,
		    scoring_status = 'scored',
		    rederive_owed = true
		FROM (VALUES %s) AS v(id, priority_score, autonomy_suitability, ai_summary, priority_reasoning)
		WHERE t.id = v.id AND t.org_id = $1
	`, strings.Join(rowExprs, ", "))
	_, err := s.q.ExecContext(ctx, query, args...)
	return err
}

func (s *scoreStore) TasksOwedReDerive(ctx context.Context, orgID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx,
		`SELECT id FROM tasks WHERE org_id = $1 AND rederive_owed ORDER BY created_at ASC`,
		orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *scoreStore) ClearReDeriveOwed(ctx context.Context, orgID string, taskIDs []string) error {
	if len(taskIDs) == 0 {
		return nil
	}
	// org_id is load-bearing rather than defense in depth, as in
	// ResetStaleScoring: the per-org runners drain concurrently, and an
	// unscoped clear would discharge another tenant's debt on IDs it never
	// evaluated.
	_, err := s.q.ExecContext(ctx,
		`UPDATE tasks SET rederive_owed = false WHERE org_id = $1 AND id = ANY($2)`,
		orgID, taskIDs)
	return err
}

func (s *scoreStore) UnscoredTasks(ctx context.Context, orgID string) ([]domain.Task, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+pgTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id AND e.org_id = t.org_id
		WHERE t.org_id = $1 AND t.status = 'queued' AND t.scoring_status = 'pending'
		ORDER BY t.created_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []domain.Task
	for rows.Next() {
		var t domain.Task
		if err := scanTaskFields(rows, &t); err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// Scan helpers (pgTaskColumnsWithEntity, taskScanState,
// scanTaskFields) live in postgres/tasks.go now that TaskStore owns
// task-row scanning. ScoreStore.UnscoredTasks references them via the
// same-package import.
