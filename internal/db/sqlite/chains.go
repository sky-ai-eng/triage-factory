package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// chainStore is the SQLite impl of db.ChainStore.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl's (app, admin) split. SQLite has one connection so
// the second arg is discarded; `...System` admin-pool variants are
// thin wrappers around their non-System counterparts.
type chainStore struct{ q queryer }

func newChainStore(q, _ queryer) db.ChainStore { return &chainStore{q: q} }

var _ db.ChainStore = (*chainStore)(nil)

func (s *chainStore) ListSteps(ctx context.Context, orgID, chainPromptID string) ([]domain.ChainStep, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT chain_prompt_id, step_index, step_prompt_id, brief, created_at
		FROM prompt_chain_steps
		WHERE chain_prompt_id = ?
		ORDER BY step_index ASC
	`, chainPromptID)
	if err != nil {
		return nil, fmt.Errorf("query chain steps: %w", err)
	}
	defer rows.Close()

	var out []domain.ChainStep
	for rows.Next() {
		var st domain.ChainStep
		if err := rows.Scan(&st.ChainPromptID, &st.StepIndex, &st.StepPromptID, &st.Brief, &st.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *chainStore) CountStepReferences(ctx context.Context, orgID, stepPromptID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT chain_prompt_id)
		FROM prompt_chain_steps
		WHERE step_prompt_id = ?
	`, stepPromptID).Scan(&n)
	return n, err
}

func (s *chainStore) ReplaceSteps(ctx context.Context, orgID, chainPromptID string, stepPromptIDs, briefs []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if len(briefs) != 0 && len(briefs) != len(stepPromptIDs) {
		return fmt.Errorf("briefs length %d must match stepPromptIDs length %d", len(briefs), len(stepPromptIDs))
	}
	return inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx, `DELETE FROM prompt_chain_steps WHERE chain_prompt_id = ?`, chainPromptID); err != nil {
			return fmt.Errorf("clear existing steps: %w", err)
		}
		now := time.Now().UTC()
		for i, stepID := range stepPromptIDs {
			brief := ""
			if i < len(briefs) {
				brief = briefs[i]
			}
			// team_id is intentionally omitted: the schema column is
			// NOT NULL DEFAULT LocalDefaultTeamID, and SQLite is N=1 local
			// mode where every prompt pins that same sentinel team. The
			// same-team FK ((chain_prompt_id, team_id) → prompts(id, team_id))
			// is satisfied because the chain prompt's team_id is the sentinel
			// too. The Postgres impl can't lean on a DEFAULT — it's
			// multi-team — so it derives team_id from the chain prompt in-SQL
			// via a subquery. If SQLite ever ran multi-team this would need
			// the same explicit derivation.
			if _, err := q.ExecContext(ctx, `
				INSERT INTO prompt_chain_steps (chain_prompt_id, step_index, step_prompt_id, brief, created_at)
				VALUES (?, ?, ?, ?, ?)
			`, chainPromptID, i, stepID, brief, now); err != nil {
				return fmt.Errorf("insert step %d: %w", i, err)
			}
		}
		return nil
	})
}

func (s *chainStore) CreateRun(ctx context.Context, orgID string, cr domain.ChainRun) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	if cr.ID == "" {
		cr.ID = uuid.New().String()
	}
	if cr.Status == "" {
		cr.Status = domain.ChainRunStatusRunning
	}
	if cr.TriggerType == "" {
		return "", errors.New("chain run trigger type required")
	}
	var triggerID any
	if cr.TriggerID != "" {
		triggerID = cr.TriggerID
	}
	var abortReason any
	if cr.AbortReason != "" {
		abortReason = cr.AbortReason
	}
	var completedAt any
	if cr.CompletedAt != nil {
		completedAt = cr.CompletedAt.UTC()
	}
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO chain_runs (id, chain_prompt_id, task_id, trigger_type, trigger_id, status, worktree_path, abort_reason, completed_at, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, cr.ID, cr.ChainPromptID, cr.TaskID, cr.TriggerType, triggerID, cr.Status, cr.WorktreePath, abortReason, completedAt); err != nil {
		return "", fmt.Errorf("insert chain_run: %w", err)
	}
	return cr.ID, nil
}

func (s *chainStore) GetRun(ctx context.Context, orgID, id string) (*domain.ChainRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var (
		cr            domain.ChainRun
		triggerID     sql.NullString
		abortReason   sql.NullString
		abortedAtStep sql.NullInt64
		completedAt   sql.NullTime
	)
	err := s.q.QueryRowContext(ctx, `
		SELECT id, chain_prompt_id, task_id, trigger_type, trigger_id, status,
		       abort_reason, aborted_at_step, worktree_path, started_at, completed_at
		FROM chain_runs WHERE id = ?
	`, id).Scan(&cr.ID, &cr.ChainPromptID, &cr.TaskID, &cr.TriggerType, &triggerID, &cr.Status,
		&abortReason, &abortedAtStep, &cr.WorktreePath, &cr.StartedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if triggerID.Valid {
		cr.TriggerID = triggerID.String
	}
	if abortReason.Valid {
		cr.AbortReason = abortReason.String
	}
	if abortedAtStep.Valid {
		i := int(abortedAtStep.Int64)
		cr.AbortedAtStep = &i
	}
	if completedAt.Valid {
		t := completedAt.Time
		cr.CompletedAt = &t
	}
	return &cr, nil
}

func (s *chainStore) GetRunForRun(ctx context.Context, orgID, runID string) (*domain.ChainRun, *int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, nil, err
	}
	var (
		chainRunID sql.NullString
		stepIndex  sql.NullInt64
	)
	err := s.q.QueryRowContext(ctx, `SELECT chain_run_id, chain_step_index FROM runs WHERE id = ?`, runID).
		Scan(&chainRunID, &stepIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !chainRunID.Valid {
		return nil, nil, nil
	}
	cr, err := s.GetRun(ctx, orgID, chainRunID.String)
	if err != nil || cr == nil {
		return nil, nil, err
	}
	var idx *int
	if stepIndex.Valid {
		v := int(stepIndex.Int64)
		idx = &v
	}
	return cr, idx, nil
}

// GetRunForRunSystem is a passthrough to GetRunForRun on SQLite — one
// connection, no dual-pool routing. The method exists for signature
// parity with the Postgres impl so spawner / chain orchestrator code
// targets the System variant uniformly.
func (s *chainStore) GetRunForRunSystem(ctx context.Context, orgID, runID string) (*domain.ChainRun, *int, error) {
	return s.GetRunForRun(ctx, orgID, runID)
}

func (s *chainStore) MarkRunStatus(ctx context.Context, orgID, id string, status domain.ChainRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	var (
		reasonArg any
		stepArg   any
	)
	if abortReason != "" {
		reasonArg = abortReason
	}
	if abortedAtStep != nil {
		stepArg = *abortedAtStep
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE chain_runs
		SET status = ?, abort_reason = ?, aborted_at_step = ?, completed_at = ?
		WHERE id = ? AND status IN ('running','pending_approval','awaiting_input')
	`, string(status), reasonArg, stepArg, time.Now().UTC(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *chainStore) RunsForChain(ctx context.Context, orgID, chainRunID string) ([]domain.AgentRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, task_id, prompt_id, status, model, started_at, completed_at,
		       total_cost_usd, duration_ms, num_turns, stop_reason, worktree_path,
		       result_summary, session_id, chain_run_id, chain_step_index
		FROM runs
		WHERE chain_run_id = ?
		ORDER BY chain_step_index ASC, started_at ASC
	`, chainRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AgentRun
	for rows.Next() {
		var (
			r           domain.AgentRun
			completedAt sql.NullTime
			costUSD     sql.NullFloat64
			durationMs  sql.NullInt64
			numTurns    sql.NullInt64
			chainStep   sql.NullInt64
			promptID    sql.NullString
			model       sql.NullString
			stopReason  sql.NullString
			worktreeP   sql.NullString
			resultSum   sql.NullString
			sessionID   sql.NullString
			chainRunIDs sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.TaskID, &promptID, &r.Status, &model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns, &stopReason, &worktreeP, &resultSum, &sessionID,
			&chainRunIDs, &chainStep); err != nil {
			return nil, err
		}
		r.PromptID = promptID.String
		r.Model = model.String
		r.StopReason = stopReason.String
		r.WorktreePath = worktreeP.String
		r.ResultSummary = resultSum.String
		r.SessionID = sessionID.String
		if chainRunIDs.Valid {
			r.ChainRunID = chainRunIDs.String
		}
		if chainStep.Valid {
			v := int(chainStep.Int64)
			r.ChainStepIndex = &v
		}
		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}
		if costUSD.Valid {
			r.TotalCostUSD = &costUSD.Float64
		}
		if durationMs.Valid {
			v := int(durationMs.Int64)
			r.DurationMs = &v
		}
		if numTurns.Valid {
			v := int(numTurns.Int64)
			r.NumTurns = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// chainVerdictTimestampLayout is the microsecond-precision UTC format
// used for run_artifacts.created_at on chain:verdict rows. SQLite's
// CURRENT_TIMESTAMP is second-precision, which is too coarse: two
// verdicts written within the same wall-clock second would tie, and
// GetLatestVerdict / LatestVerdictsForRuns wouldn't be able to recover
// insertion order from ORDER BY created_at DESC. See InsertVerdict.
const chainVerdictTimestampLayout = "2006-01-02 15:04:05.000000"

func (s *chainStore) InsertVerdict(ctx context.Context, orgID, runID, metadataJSON string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	id := uuid.New().String()
	now := time.Now().UTC().Format(chainVerdictTimestampLayout)
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO run_artifacts (id, run_id, kind, metadata_json, is_primary, created_at)
		VALUES (?, ?, 'chain:verdict', ?, 0, ?)
	`, id, runID, metadataJSON, now)
	return err
}

func (s *chainStore) GetLatestVerdict(ctx context.Context, orgID, runID string) (*domain.ChainVerdict, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var raw string
	err := s.q.QueryRowContext(ctx, `
		SELECT metadata_json FROM run_artifacts
		WHERE run_id = ? AND kind = 'chain:verdict'
		ORDER BY created_at DESC, id DESC LIMIT 1
	`, runID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if raw == "" {
		return &domain.ChainVerdict{}, nil
	}
	var v domain.ChainVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("decode verdict: %w", err)
	}
	return &v, nil
}

func (s *chainStore) LatestVerdictsForRuns(ctx context.Context, orgID string, runIDs []string) (map[string]*domain.ChainVerdict, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(runIDs) == 0 {
		return map[string]*domain.ChainVerdict{}, nil
	}

	placeholders := strings.TrimRight(strings.Repeat("?,", len(runIDs)), ",")
	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		args[i] = id
	}

	// Window function: rank artifacts per run by created_at DESC, then
	// id DESC (id is UUID — not a real tie-breaker, but gives stable sort).
	// Return only rank=1 rows (the latest per run).
	query := fmt.Sprintf(`
		SELECT run_id, metadata_json
		FROM (
			SELECT run_id, metadata_json,
			       ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY created_at DESC, id DESC) AS rn
			FROM run_artifacts
			WHERE run_id IN (%s) AND kind = 'chain:verdict'
		)
		WHERE rn = 1
	`, placeholders)

	rows, err := s.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query latest verdicts: %w", err)
	}
	defer rows.Close()

	out := make(map[string]*domain.ChainVerdict)
	for rows.Next() {
		var (
			runID string
			raw   string
		)
		if err := rows.Scan(&runID, &raw); err != nil {
			return nil, err
		}
		if raw == "" {
			out[runID] = &domain.ChainVerdict{}
			continue
		}
		var v domain.ChainVerdict
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			return nil, fmt.Errorf("decode verdict for run %s: %w", runID, err)
		}
		out[runID] = &v
	}
	return out, rows.Err()
}

// --- Admin-pool variants ---
//
// All `...System` methods below delegate straight through to their
// non-System counterparts. SQLite has one connection so the pool
// distinction doesn't exist; the wrappers are kept for signature
// parity with Postgres. The chain orchestrator goroutine consumes
// these from its detached-context lifecycle.

func (s *chainStore) ActiveStepRunIDs(ctx context.Context, orgID, chainRunID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE chain_run_id = ?
		  AND status NOT IN ('completed','failed','cancelled','task_unsolvable',
		                     'pending_approval','taken_over','awaiting_input')
	`, chainRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *chainStore) ListStepsSystem(ctx context.Context, orgID, chainPromptID string) ([]domain.ChainStep, error) {
	return s.ListSteps(ctx, orgID, chainPromptID)
}

func (s *chainStore) MarkRunStatusSystem(ctx context.Context, orgID, id string, status domain.ChainRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	return s.MarkRunStatus(ctx, orgID, id, status, abortReason, abortedAtStep)
}

func (s *chainStore) GetRunSystem(ctx context.Context, orgID, id string) (*domain.ChainRun, error) {
	return s.GetRun(ctx, orgID, id)
}

func (s *chainStore) ActiveStepRunIDsSystem(ctx context.Context, orgID, chainRunID string) ([]string, error) {
	return s.ActiveStepRunIDs(ctx, orgID, chainRunID)
}

func (s *chainStore) RunsForChainSystem(ctx context.Context, orgID, chainRunID string) ([]domain.AgentRun, error) {
	return s.RunsForChain(ctx, orgID, chainRunID)
}

func (s *chainStore) InsertVerdictSystem(ctx context.Context, orgID, runID, metadataJSON string) error {
	return s.InsertVerdict(ctx, orgID, runID, metadataJSON)
}

func (s *chainStore) GetLatestVerdictSystem(ctx context.Context, orgID, runID string) (*domain.ChainVerdict, error) {
	return s.GetLatestVerdict(ctx, orgID, runID)
}
