package postgres

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

// chainStore is the Postgres impl of db.ChainStore. Holds two pools:
//
//   - app: app pool (tf_app, RLS-active). Every request-equivalent
//     consumer (chains handler, pending_prs / reviews handlers,
//     user-initiated chain lifecycle entry points like CancelChain
//     and the ResumeChainAfter* paths) runs here.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). The chain
//     orchestrator goroutine — delegateChain / runChain /
//     terminateChain — detaches from the kicking-off handler's
//     context the moment it spawns, so it has no JWT-claims in
//     scope and routes through admin via the `...System` variants.
//     org_id stays in the WHERE clause as defense in depth.
//
// CreateRun routes internally on ChainRun.TriggerType, mirroring
// the AgentRunStore.Create pattern: event-triggered chains land on
// the admin pool with NULL creator_user_id, manual chains on the
// app pool with COALESCE fallback. There is no separate
// CreateRunSystem.
//
// org_id is threaded through every WHERE for defense in depth; RLS
// enforces the creator predicate on chain_runs and prompt_chain_steps
// RLS gates on the parent prompt's team membership.
//
// Reads against UUID-typed columns (chain_runs.id, runs.id, …) guard
// inputs with isValidUUID and treat non-UUID strings as not-found —
// otherwise Postgres surfaces 22P02 at the column-type layer, which
// doesn't match the SQLite TEXT-keyed semantics callers expect.
type chainStore struct {
	app   queryer
	admin queryer
}

func newChainStore(app, admin queryer) db.ChainStore {
	return &chainStore{app: app, admin: admin}
}

var _ db.ChainStore = (*chainStore)(nil)

func (s *chainStore) ListSteps(ctx context.Context, orgID, chainPromptID string) ([]domain.ChainStep, error) {
	return listChainSteps(ctx, s.app, orgID, chainPromptID)
}

func (s *chainStore) ListStepsSystem(ctx context.Context, orgID, chainPromptID string) ([]domain.ChainStep, error) {
	return listChainSteps(ctx, s.admin, orgID, chainPromptID)
}

func listChainSteps(ctx context.Context, q queryer, orgID, chainPromptID string) ([]domain.ChainStep, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT chain_prompt_id, step_index, step_prompt_id, brief, created_at
		FROM prompt_chain_steps
		WHERE org_id = $1 AND chain_prompt_id = $2
		ORDER BY step_index ASC
	`, orgID, chainPromptID)
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
	var n int
	err := s.app.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT chain_prompt_id)
		FROM prompt_chain_steps
		WHERE org_id = $1 AND step_prompt_id = $2
	`, orgID, stepPromptID).Scan(&n)
	return n, err
}

func (s *chainStore) ReplaceSteps(ctx context.Context, orgID, chainPromptID string, stepPromptIDs, briefs []string) error {
	if len(briefs) != 0 && len(briefs) != len(stepPromptIDs) {
		return fmt.Errorf("briefs length %d must match stepPromptIDs length %d", len(briefs), len(stepPromptIDs))
	}
	return s.runInTx(ctx, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM prompt_chain_steps WHERE org_id = $1 AND chain_prompt_id = $2`,
			orgID, chainPromptID); err != nil {
			return fmt.Errorf("clear existing steps: %w", err)
		}
		now := time.Now().UTC()
		for i, stepID := range stepPromptIDs {
			brief := ""
			if i < len(briefs) {
				brief = briefs[i]
			}
			// team_id is derived from the chain prompt in-SQL so the
			// same-team composite FKs ((chain_prompt_id|step_prompt_id,
			// team_id) → prompts(id, team_id)) enforce that every step
			// belongs to the chain's own team — a cross-team step is
			// refused at the DB layer (SKY-380).
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO prompt_chain_steps (org_id, team_id, chain_prompt_id, step_index, step_prompt_id, brief, created_at)
				VALUES ($1, (SELECT team_id FROM prompts WHERE id = $2 AND org_id = $1), $2, $3, $4, $5, $6)
			`, orgID, chainPromptID, i, stepID, brief, now); err != nil {
				return fmt.Errorf("insert step %d: %w", i, err)
			}
		}
		return nil
	})
}

// CreateRun routes internally on cr.TriggerType, mirroring the
// AgentRunStore.Create pattern:
//
//   - trigger_type='event' lands on the admin pool with
//     creator_user_id NULL. The chain_runs_creator_matches_trigger_type
//     CHECK pairs that nullability with trigger_type. The RLS policy
//     chain_runs_modify gates app-pool inserts on
//     creator_user_id = tf.current_user_id(), which is mutually
//     exclusive with creator NULL — admin pool (BYPASSRLS) is the
//     only path that can satisfy the CHECK.
//
//   - trigger_type='manual' lands on the app pool with
//     COALESCE(tf.current_user_id(), orgs.owner_user_id) so the
//     audit row carries the JWT-bound caller in production and
//     falls back to the org owner in admin-pooled tests where no
//     JWT is set.
//
// Both paths commit autonomously from any outer WithTx the caller
// is composed inside — admin is a separate connection. No caller
// composes this method under WithTx today; document the edge here.
//
// The SQL is split because Postgres needs USAGE on schema `tf` to
// plan a reference to `tf.current_user_id()`, and the admin role
// (supabase_admin) running the event-triggered insert shouldn't
// need to touch `tf` at all.
func (s *chainStore) CreateRun(ctx context.Context, orgID string, cr domain.ChainRun) (string, error) {
	if cr.ID == "" {
		cr.ID = uuid.New().String()
	}
	if cr.Status == "" {
		cr.Status = domain.ChainRunStatusRunning
	}
	if cr.TriggerType == "" {
		return "", errors.New("chain run trigger type required")
	}
	if cr.TriggerType == domain.ChainTriggerEvent {
		return s.createRunEventTriggered(ctx, orgID, cr)
	}
	return s.createRunManual(ctx, orgID, cr)
}

func (s *chainStore) createRunEventTriggered(ctx context.Context, orgID string, cr domain.ChainRun) (string, error) {
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
	if _, err := s.admin.ExecContext(ctx, `
		INSERT INTO chain_runs
			(id, org_id, creator_user_id, chain_prompt_id, task_id, trigger_type, trigger_id,
			 status, worktree_path, abort_reason, completed_at, started_at)
		VALUES (
			$1, $2, NULL,
			$3, $4, $5, $6,
			$7, $8, $9, $10, now()
		)
	`, cr.ID, orgID, cr.ChainPromptID, cr.TaskID, cr.TriggerType, triggerID, cr.Status, cr.WorktreePath, abortReason, completedAt); err != nil {
		return "", fmt.Errorf("insert chain_run (event): %w", err)
	}
	return cr.ID, nil
}

func (s *chainStore) createRunManual(ctx context.Context, orgID string, cr domain.ChainRun) (string, error) {
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
	// creator_user_id resolution mirrors PromptStore / TaskRuleStore
	// / TriggerStore: prefer the JWT-bound caller, fall back to org
	// owner so admin-pooled tests still satisfy the chain_runs
	// _creator_matches_trigger_type CHECK (NOT NULL on the manual
	// branch).
	if _, err := s.app.ExecContext(ctx, `
		INSERT INTO chain_runs
			(id, org_id, creator_user_id, chain_prompt_id, task_id, trigger_type, trigger_id,
			 status, worktree_path, abort_reason, completed_at, started_at)
		VALUES (
			$1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3, $4, $5, $6,
			$7, $8, $9, $10, now()
		)
	`, cr.ID, orgID, cr.ChainPromptID, cr.TaskID, cr.TriggerType, triggerID, cr.Status, cr.WorktreePath, abortReason, completedAt); err != nil {
		return "", fmt.Errorf("insert chain_run (manual): %w", err)
	}
	return cr.ID, nil
}

func (s *chainStore) GetRun(ctx context.Context, orgID, id string) (*domain.ChainRun, error) {
	return getChainRun(ctx, s.app, orgID, id)
}

func (s *chainStore) GetRunSystem(ctx context.Context, orgID, id string) (*domain.ChainRun, error) {
	return getChainRun(ctx, s.admin, orgID, id)
}

func getChainRun(ctx context.Context, q queryer, orgID, id string) (*domain.ChainRun, error) {
	// chain_runs.id is UUID-typed; non-UUID strings would error at the
	// column type layer (22P02) before WHERE evaluates. Treat as
	// not-found to match the SQLite TEXT-keyed semantics.
	if !isValidUUID(id) {
		return nil, nil
	}
	var (
		cr            domain.ChainRun
		triggerID     sql.NullString
		abortReason   sql.NullString
		abortedAtStep sql.NullInt64
		completedAt   sql.NullTime
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, chain_prompt_id, task_id, trigger_type, trigger_id, status,
		       abort_reason, aborted_at_step, worktree_path, started_at, completed_at
		FROM chain_runs WHERE org_id = $1 AND id = $2
	`, orgID, id).Scan(&cr.ID, &cr.ChainPromptID, &cr.TaskID, &cr.TriggerType, &triggerID, &cr.Status,
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
	return getRunForRun(ctx, s.app, s, orgID, runID)
}

func (s *chainStore) GetRunForRunSystem(ctx context.Context, orgID, runID string) (*domain.ChainRun, *int, error) {
	return getRunForRunSystem(ctx, s.admin, s, orgID, runID)
}

func getRunForRun(ctx context.Context, q queryer, store *chainStore, orgID, runID string) (*domain.ChainRun, *int, error) {
	if !isValidUUID(runID) {
		return nil, nil, nil
	}
	chainRunID, stepIndex, err := readRunChainPointer(ctx, q, orgID, runID)
	if err != nil || chainRunID == "" {
		return nil, nil, err
	}
	cr, err := store.GetRun(ctx, orgID, chainRunID)
	if err != nil || cr == nil {
		return nil, nil, err
	}
	return cr, stepIndex, nil
}

func getRunForRunSystem(ctx context.Context, q queryer, store *chainStore, orgID, runID string) (*domain.ChainRun, *int, error) {
	if !isValidUUID(runID) {
		return nil, nil, nil
	}
	chainRunID, stepIndex, err := readRunChainPointer(ctx, q, orgID, runID)
	if err != nil || chainRunID == "" {
		return nil, nil, err
	}
	cr, err := store.GetRunSystem(ctx, orgID, chainRunID)
	if err != nil || cr == nil {
		return nil, nil, err
	}
	return cr, stepIndex, nil
}

// readRunChainPointer reads runs.chain_run_id + chain_step_index for a
// single run. Factored so GetRunForRun + GetRunForRunSystem can share
// the pre-join lookup with the appropriate pool while delegating the
// chain-row read to GetRun / GetRunSystem.
func readRunChainPointer(ctx context.Context, q queryer, orgID, runID string) (string, *int, error) {
	var (
		chainRunID sql.NullString
		stepIndex  sql.NullInt64
	)
	err := q.QueryRowContext(ctx,
		`SELECT chain_run_id, chain_step_index FROM runs WHERE org_id = $1 AND id = $2`,
		orgID, runID).Scan(&chainRunID, &stepIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if !chainRunID.Valid {
		return "", nil, nil
	}
	var idx *int
	if stepIndex.Valid {
		v := int(stepIndex.Int64)
		idx = &v
	}
	return chainRunID.String, idx, nil
}

func (s *chainStore) MarkRunStatus(ctx context.Context, orgID, id string, status domain.ChainRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	return markChainRunStatus(ctx, s.app, orgID, id, status, abortReason, abortedAtStep)
}

func (s *chainStore) MarkRunStatusSystem(ctx context.Context, orgID, id string, status domain.ChainRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	return markChainRunStatus(ctx, s.admin, orgID, id, status, abortReason, abortedAtStep)
}

func markChainRunStatus(ctx context.Context, q queryer, orgID, id string, status domain.ChainRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	if !isValidUUID(id) {
		return false, nil
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
	res, err := q.ExecContext(ctx, `
		UPDATE chain_runs
		SET status = $1, abort_reason = $2, aborted_at_step = $3, completed_at = now()
		WHERE org_id = $4 AND id = $5
		  AND status IN ('running','pending_approval','awaiting_input')
	`, string(status), reasonArg, stepArg, orgID, id)
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
	return runsForChain(ctx, s.app, orgID, chainRunID)
}

func (s *chainStore) RunsForChainSystem(ctx context.Context, orgID, chainRunID string) ([]domain.AgentRun, error) {
	return runsForChain(ctx, s.admin, orgID, chainRunID)
}

func (s *chainStore) ActiveStepRunIDs(ctx context.Context, orgID, chainRunID string) ([]string, error) {
	return activeStepRunIDs(ctx, s.app, orgID, chainRunID)
}

func (s *chainStore) ActiveStepRunIDsSystem(ctx context.Context, orgID, chainRunID string) ([]string, error) {
	return activeStepRunIDs(ctx, s.admin, orgID, chainRunID)
}

func activeStepRunIDs(ctx context.Context, q queryer, orgID, chainRunID string) ([]string, error) {
	if !isValidUUID(chainRunID) {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE org_id = $1 AND chain_run_id = $2
		  AND status NOT IN ('completed','failed','cancelled','task_unsolvable',
		                     'pending_approval','taken_over','awaiting_input')
	`, orgID, chainRunID)
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

func runsForChain(ctx context.Context, q queryer, orgID, chainRunID string) ([]domain.AgentRun, error) {
	if !isValidUUID(chainRunID) {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, task_id, prompt_id, status, model, started_at, completed_at,
		       total_cost_usd, duration_ms, num_turns, stop_reason, worktree_path,
		       result_summary, session_id, chain_run_id, chain_step_index
		FROM runs
		WHERE org_id = $1 AND chain_run_id = $2
		ORDER BY chain_step_index ASC, started_at ASC
	`, orgID, chainRunID)
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

func (s *chainStore) InsertVerdict(ctx context.Context, orgID, runID, metadataJSON string) error {
	return insertChainVerdict(ctx, s.app, orgID, runID, metadataJSON)
}

func (s *chainStore) InsertVerdictSystem(ctx context.Context, orgID, runID, metadataJSON string) error {
	return insertChainVerdict(ctx, s.admin, orgID, runID, metadataJSON)
}

func insertChainVerdict(ctx context.Context, q queryer, orgID, runID, metadataJSON string) error {
	if !isValidUUID(runID) {
		return fmt.Errorf("postgres chains: invalid run_id %q", runID)
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO run_artifacts (id, org_id, run_id, kind, metadata_json, is_primary, created_at)
		VALUES (gen_random_uuid(), $1, $2, 'chain:verdict', $3::jsonb, FALSE, now())
	`, orgID, runID, metadataJSON)
	return err
}

func (s *chainStore) GetLatestVerdict(ctx context.Context, orgID, runID string) (*domain.ChainVerdict, error) {
	return getLatestChainVerdict(ctx, s.app, orgID, runID)
}

func (s *chainStore) GetLatestVerdictSystem(ctx context.Context, orgID, runID string) (*domain.ChainVerdict, error) {
	return getLatestChainVerdict(ctx, s.admin, orgID, runID)
}

func getLatestChainVerdict(ctx context.Context, q queryer, orgID, runID string) (*domain.ChainVerdict, error) {
	if !isValidUUID(runID) {
		return nil, nil
	}
	var raw sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT metadata_json::text FROM run_artifacts
		WHERE org_id = $1 AND run_id = $2 AND kind = 'chain:verdict'
		ORDER BY created_at DESC, id DESC LIMIT 1
	`, orgID, runID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return &domain.ChainVerdict{}, nil
	}
	var v domain.ChainVerdict
	if err := json.Unmarshal([]byte(raw.String), &v); err != nil {
		return nil, fmt.Errorf("decode verdict: %w", err)
	}
	return &v, nil
}

func (s *chainStore) LatestVerdictsForRuns(ctx context.Context, orgID string, runIDs []string) (map[string]*domain.ChainVerdict, error) {
	if len(runIDs) == 0 {
		return map[string]*domain.ChainVerdict{}, nil
	}
	// Drop non-UUID ids up front: passing them as parameters to a
	// UUID-typed column would 22P02. Mirrors the per-id isValidUUID
	// guards on the single-key reads above.
	valid := make([]string, 0, len(runIDs))
	for _, id := range runIDs {
		if isValidUUID(id) {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return map[string]*domain.ChainVerdict{}, nil
	}

	placeholders := make([]string, len(valid))
	args := make([]any, len(valid))
	for i, id := range valid {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}

	query := fmt.Sprintf(`
		SELECT run_id, metadata_json::text
		FROM (
			SELECT run_id, metadata_json,
			       ROW_NUMBER() OVER (PARTITION BY run_id ORDER BY created_at DESC, id DESC) AS rn
			FROM run_artifacts
			WHERE run_id IN (%s) AND kind = 'chain:verdict'
		) ranked
		WHERE rn = 1
	`, strings.Join(placeholders, ","))

	rows, err := s.app.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query latest verdicts: %w", err)
	}
	defer rows.Close()

	out := make(map[string]*domain.ChainVerdict)
	for rows.Next() {
		var (
			runID string
			raw   sql.NullString
		)
		if err := rows.Scan(&runID, &raw); err != nil {
			return nil, err
		}
		if !raw.Valid || raw.String == "" {
			out[runID] = &domain.ChainVerdict{}
			continue
		}
		var v domain.ChainVerdict
		if err := json.Unmarshal([]byte(raw.String), &v); err != nil {
			return nil, fmt.Errorf("decode verdict for run %s: %w", runID, err)
		}
		out[runID] = &v
	}
	return out, rows.Err()
}

// runInTx is the multi-statement helper for ReplaceSteps. Composes with
// the caller's *sql.Tx inside WithTx so the DELETE + per-step INSERTs
// land atomically alongside any outer write; otherwise opens a fresh tx
// on the app pool. Same shape taskRuleStore.runInTx uses.
func (s *chainStore) runInTx(ctx context.Context, fn func(queryer) error) error {
	switch v := s.app.(type) {
	case *sql.Tx:
		return fn(v)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return errors.New("postgres chains: unexpected queryer type")
	}
}
