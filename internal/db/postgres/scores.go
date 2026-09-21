package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
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

// UpdateTaskScores is one transaction: every task's task_rederive_queue row
// admitted or raised first, in ascending task id order, then the batched
// tasks statement. The queue row's requested_revision is set from the task's
// pre-increment score_revision plus one, and the tasks statement then
// increments score_revision, so after the commit the two are equal.
//
// Lock order: task_rederive_queue before tasks, every transaction that
// touches both. The admission's conflict arm and the UPDATE that follows it
// both lock the queue row, which is what serializes this writer against a
// completion holding that row — the writer waits, then finds the row done and
// admits a fresh one, or finds it still unsettled and raises it. The fixed
// task order keeps two concurrent score writers from deadlocking on each
// other's queue rows.
func (s *scoreStore) UpdateTaskScores(ctx context.Context, orgID string, updates []domain.TaskScoreUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	ordered := make([]domain.TaskScoreUpdate, len(updates))
	copy(ordered, updates)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	kind := workkinds.TaskReDerive(workitem.Postgres)
	return inTxRaw(ctx, s.q, func(tx *sql.Tx) error {
		for _, u := range ordered {
			id, _, err := workitem.Admit(ctx, tx, kind, orgID, u.ID, db.TaskReDeriveRowCols(u.ID))
			if err != nil {
				return err
			}
			// Runs whether the admission inserted or deduplicated, so a fresh
			// row and a raised one take the same path.
			if _, err := tx.ExecContext(ctx, `
				UPDATE public.task_rederive_queue
				   SET requested_revision = (SELECT score_revision + 1 FROM public.tasks WHERE id = $1 AND org_id = $2)
				 WHERE id = $3 AND org_id = $2
			`, u.ID, orgID, id); err != nil {
				return fmt.Errorf("raise task_rederive_queue row %d: %w", id, err)
			}
		}

		// Single UPDATE ... FROM (VALUES ...) so the whole batch lands in
		// one round-trip.
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
		for _, u := range ordered {
			rowExprs = append(rowExprs, fmt.Sprintf(
				"($%d::uuid, $%d::real, $%d::real, $%d::text, $%d::text)",
				n, n+1, n+2, n+3, n+4))
			args = append(args, u.ID, u.PriorityScore, u.AutonomySuitability, u.Summary, u.PriorityReasoning)
			n += 5
		}
		query := fmt.Sprintf(`
			UPDATE tasks t
			SET priority_score = v.priority_score,
			    autonomy_suitability = v.autonomy_suitability,
			    ai_summary = v.ai_summary,
			    priority_reasoning = v.priority_reasoning,
			    scoring_status = 'scored',
			    score_revision = score_revision + 1
			FROM (VALUES %s) AS v(id, priority_score, autonomy_suitability, ai_summary, priority_reasoning)
			WHERE t.id = v.id AND t.org_id = $1
		`, strings.Join(rowExprs, ", "))
		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
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
