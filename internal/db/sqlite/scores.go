package sqlite

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// scoreStore is the SQLite impl of db.ScoreStore. SQL bodies are
// moved verbatim from the pre-D2 internal/db/scores.go; the only
// behavioral change is the orgID assertion at each method entry.
type scoreStore struct{ q queryer }

func newScoreStore(q queryer) db.ScoreStore { return &scoreStore{q: q} }

// Compile-time check that scoreStore satisfies the interface — the
// build breaks immediately if the interface gains a method this impl
// hasn't grown.
var _ db.ScoreStore = (*scoreStore)(nil)

func (s *scoreStore) MarkScoring(ctx context.Context, orgID string, taskIDs []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	for _, id := range taskIDs {
		if _, err := s.q.ExecContext(ctx, `UPDATE tasks SET scoring_status = 'in_progress' WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *scoreStore) ResetScoringToPending(ctx context.Context, orgID string, taskIDs []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	for _, id := range taskIDs {
		if _, err := s.q.ExecContext(ctx, `UPDATE tasks SET scoring_status = 'pending' WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *scoreStore) ResetStaleScoring(ctx context.Context, orgID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	res, err := s.q.ExecContext(ctx, `UPDATE tasks SET scoring_status = 'pending' WHERE scoring_status = 'in_progress'`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// updateTaskScoresChunkSize is the max rows-per-statement for the
// batched UPDATE. SQLite's bound-parameter cap is driver-dependent
// (modernc.org/sqlite ships with the modern 32766 default; some
// builds historically defaulted to 999). 150 rows × 5 placeholders
// = 750 placeholders, comfortably under any plausible limit and
// leaves margin if the UPDATE row shape grows.
const updateTaskScoresChunkSize = 150

func (s *scoreStore) UpdateTaskScores(ctx context.Context, orgID string, updates []domain.TaskScoreUpdate) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if len(updates) == 0 {
		return nil
	}
	// Single UPDATE ... FROM (VALUES ...) per chunk, all chunks inside
	// one tx for all-or-nothing semantics across the batch. Atomicity
	// matters because a mid-stream failure must NOT leave the cycle's
	// tasks half-marked 'scored' (the scorer's reset path keys off
	// scoring_status, and partial state confuses it). inTx reuses the
	// caller's *sql.Tx if we're already inside one (Stores.Tx.WithTx),
	// or opens a fresh tx otherwise.
	//
	// Dialect note: SQLite supports UPDATE-FROM (>= 3.33) and VALUES as
	// a table source, but does NOT accept column aliasing directly on a
	// VALUES source ("(VALUES ...) AS v(col1, col2)" — that form is a
	// Postgres extension). The VALUES is wrapped in a SELECT that
	// renames the default column1/column2/... aliases into the columns
	// the UPDATE references.
	return inTx(ctx, s.q, func(q queryer) error {
		for start := 0; start < len(updates); start += updateTaskScoresChunkSize {
			end := start + updateTaskScoresChunkSize
			if end > len(updates) {
				end = len(updates)
			}
			if err := applyScoresChunk(ctx, q, updates[start:end]); err != nil {
				return err
			}
		}
		return nil
	})
}

func applyScoresChunk(ctx context.Context, q queryer, chunk []domain.TaskScoreUpdate) error {
	rowExprs := make([]string, 0, len(chunk))
	args := make([]any, 0, len(chunk)*5)
	for _, u := range chunk {
		rowExprs = append(rowExprs, "(?, ?, ?, ?, ?)")
		args = append(args, u.ID, u.PriorityScore, u.AutonomySuitability, u.Summary, u.PriorityReasoning)
	}
	query := `
		UPDATE tasks
		SET priority_score = v.priority_score,
		    autonomy_suitability = v.autonomy_suitability,
		    ai_summary = v.ai_summary,
		    priority_reasoning = v.priority_reasoning,
		    scoring_status = 'scored'
		FROM (
			SELECT column1 AS id,
			       column2 AS priority_score,
			       column3 AS autonomy_suitability,
			       column4 AS ai_summary,
			       column5 AS priority_reasoning
			FROM (VALUES ` + strings.Join(rowExprs, ", ") + `)
		) AS v
		WHERE tasks.id = v.id
	`
	_, err := q.ExecContext(ctx, query, args...)
	return err
}

func (s *scoreStore) UnscoredTasks(ctx context.Context, orgID string) ([]domain.Task, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+sqliteTaskColumnsWithEntity+`
		FROM tasks t
		JOIN entities e ON t.entity_id = e.id
		WHERE t.status = 'queued' AND t.scoring_status = 'pending'
		ORDER BY t.created_at DESC
	`)
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

// assertLocalOrg returns an error if the caller passed anything other
// than runmode.LocalDefaultOrgID. Local-mode SQLite tables have no org_id
// column, so any non-default value indicates a confused caller and
// must be rejected loudly.
func assertLocalOrg(orgID string) error {
	if orgID != runmode.LocalDefaultOrgID {
		return fmt.Errorf("sqlite store: orgID must be %q in local mode, got %q", runmode.LocalDefaultOrgID, orgID)
	}
	return nil
}

// Scan helpers (sqliteTaskColumnsWithEntity, taskScanState,
// scanTaskFields) live in sqlite/tasks.go now that TaskStore owns
// task-row scanning. ScoreStore.UnscoredTasks references them via the
// same-package import.
