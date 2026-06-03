package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// blueprintStore is the SQLite impl of db.BlueprintStore.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl's (app, admin) split. SQLite has one connection so the
// second arg is the seeder; `...System` admin-pool variants are thin
// wrappers around their non-System counterparts.
type blueprintStore struct {
	q      queryer
	seeder queryer
}

func newBlueprintStore(app, admin queryer) db.BlueprintStore {
	return &blueprintStore{q: app, seeder: admin}
}

var _ db.BlueprintStore = (*blueprintStore)(nil)

// --- Blueprint header CRUD -----------------------------------------------

func (s *blueprintStore) SeedOrUpdate(ctx context.Context, orgID, teamID string, b domain.Blueprint) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	if teamID == "" {
		teamID = runmode.LocalDefaultTeamID
	}
	if b.Source == "" {
		b.Source = "system"
	}
	if b.Source != "system" {
		return "", fmt.Errorf("sqlite blueprints: SeedOrUpdate only accepts Source=\"system\" (got %q)", b.Source)
	}
	if b.SystemSlug == "" {
		return "", fmt.Errorf("sqlite blueprints: SeedOrUpdate requires a non-empty SystemSlug")
	}
	now := time.Now().UTC()

	// Identity is per-team: (org_id, team_id, system_slug). Re-seed resolves
	// the existing row by this key (ON CONFLICT DO NOTHING semantics, no
	// versions sidecar — step-list updates to shipped blueprints are a future
	// concern).
	var existingID string
	switch err := s.seeder.QueryRowContext(ctx,
		`SELECT id FROM blueprints WHERE org_id = ? AND team_id = ? AND system_slug = ?`,
		orgID, teamID, b.SystemSlug,
	).Scan(&existingID); {
	case errors.Is(err, sql.ErrNoRows):
		newID := uuid.New().String()
		if _, err := s.seeder.ExecContext(ctx, `
			INSERT INTO blueprints (id, org_id, team_id, system_slug, name, source, usage_count, user_modified, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
		`, newID, orgID, teamID, b.SystemSlug, b.Name, b.Source, now, now); err != nil {
			return "", fmt.Errorf("insert blueprint: %w", err)
		}
		return newID, nil
	case err != nil:
		return "", fmt.Errorf("read blueprint: %w", err)
	}
	return existingID, nil
}

// List ignores teamID: local mode is single-team.
func (s *blueprintStore) List(ctx context.Context, orgID string, _ string) ([]domain.Blueprint, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, name, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints WHERE hidden = 0 AND deleted_at IS NULL ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Blueprint
	for rows.Next() {
		b, err := scanBlueprintRowSQLite(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Get is request-facing: it filters deleted_at IS NULL. GetSystem omits the
// filter so a soft-deleted blueprint still resolves for in-flight runs and
// past-run timelines.
func (s *blueprintStore) Get(ctx context.Context, orgID string, id string) (*domain.Blueprint, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	b, err := scanBlueprintRowSQLite(s.q.QueryRowContext(ctx, `
		SELECT id, name, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints WHERE id = ? AND deleted_at IS NULL
	`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *blueprintStore) GetSystem(ctx context.Context, orgID string, id string) (*domain.Blueprint, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	b, err := scanBlueprintRowSQLite(s.q.QueryRowContext(ctx, `
		SELECT id, name, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints WHERE id = ?
	`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *blueprintStore) GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.Blueprint, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	q := `
		SELECT id, name, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints WHERE org_id = ? AND system_slug = ? AND deleted_at IS NULL`
	args := []any{orgID, systemSlug}
	if teamID != "" {
		q += ` AND team_id = ?`
		args = append(args, teamID)
	}
	b, err := scanBlueprintRowSQLite(s.q.QueryRowContext(ctx, q, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *blueprintStore) Create(ctx context.Context, orgID, teamID string, b domain.Blueprint) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_ = teamID // local mode is single-team; the row pins LocalDefaultTeamID below
	now := time.Now().UTC()
	var creatorUserID any = runmode.LocalDefaultUserID
	if b.Source == "system" {
		creatorUserID = nil
	}
	if b.Source == "" {
		b.Source = "user"
	}
	var systemSlug any
	if b.SystemSlug != "" {
		systemSlug = b.SystemSlug
	}
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO blueprints (id, name, source, usage_count, team_id, creator_user_id, system_slug, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?)
	`, b.ID, b.Name, b.Source, runmode.LocalDefaultTeamID, creatorUserID, systemSlug, now, now)
	return err
}

// Delete soft-deletes a blueprint (stamps deleted_at). Its blueprint_steps stay
// so the copy-only unique index keeps the wrapped prompt pinned; the prompt is
// soft-deleted alongside by the delete-pairing.
func (s *blueprintStore) Delete(ctx context.Context, orgID string, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, `UPDATE blueprints SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, time.Now().UTC(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("blueprint %s not found or already deleted", id)
	}
	return nil
}

// StepPromptOwner returns the blueprint that holds promptID as a step. The
// copy-only unique index guarantees at most one row, so a single SELECT
// resolves the owner. Only considers non-deleted blueprints — a step row in a
// soft-deleted blueprint does not count as ownership (the step persists as
// audit trail; the prompt is free to be claimed by a new blueprint).
func (s *blueprintStore) StepPromptOwner(ctx context.Context, orgID string, promptID string) (string, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", false, err
	}
	var blueprintID string
	err := s.q.QueryRowContext(ctx, `
		SELECT bs.blueprint_id
		FROM blueprint_steps bs
		JOIN blueprints b ON b.id = bs.blueprint_id
		WHERE bs.step_prompt_id = ? AND b.deleted_at IS NULL
		LIMIT 1
	`, promptID).Scan(&blueprintID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return blueprintID, true, nil
}

func (s *blueprintStore) IncrementUsage(ctx context.Context, orgID string, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `UPDATE blueprints SET usage_count = usage_count + 1 WHERE id = ?`, id)
	return err
}

func (s *blueprintStore) IncrementUsageSystem(ctx context.Context, orgID string, id string) error {
	return s.IncrementUsage(ctx, orgID, id)
}

// scanBlueprintRowSQLite decodes a blueprints row in canonical column order.
func scanBlueprintRowSQLite(scanFn func(dst ...any) error) (domain.Blueprint, error) {
	var b domain.Blueprint
	var systemSlug sql.NullString
	if err := scanFn(&b.ID, &b.Name, &b.Source, &b.UsageCount, &b.UserModified, &b.TeamID, &systemSlug, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return b, err
	}
	if systemSlug.Valid {
		b.SystemSlug = systemSlug.String
	}
	return b, nil
}

// --- Steps ---------------------------------------------------------------

func (s *blueprintStore) ListSteps(ctx context.Context, orgID, blueprintID string) ([]domain.BlueprintStep, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT blueprint_id, step_index, step_prompt_id, brief, created_at
		FROM blueprint_steps
		WHERE blueprint_id = ?
		ORDER BY step_index ASC
	`, blueprintID)
	if err != nil {
		return nil, fmt.Errorf("query blueprint steps: %w", err)
	}
	defer rows.Close()

	var out []domain.BlueprintStep
	for rows.Next() {
		var st domain.BlueprintStep
		if err := rows.Scan(&st.BlueprintID, &st.StepIndex, &st.StepPromptID, &st.Brief, &st.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

func (s *blueprintStore) CountStepReferences(ctx context.Context, orgID, stepPromptID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT bs.blueprint_id)
		FROM blueprint_steps bs
		JOIN blueprints b ON b.id = bs.blueprint_id
		WHERE bs.step_prompt_id = ? AND b.deleted_at IS NULL
	`, stepPromptID).Scan(&n)
	return n, err
}

func (s *blueprintStore) ReplaceSteps(ctx context.Context, orgID, blueprintID string, stepPromptIDs, briefs []string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	if len(briefs) != 0 && len(briefs) != len(stepPromptIDs) {
		return fmt.Errorf("briefs length %d must match stepPromptIDs length %d", len(briefs), len(stepPromptIDs))
	}
	return inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx, `DELETE FROM blueprint_steps WHERE blueprint_id = ?`, blueprintID); err != nil {
			return fmt.Errorf("clear existing steps: %w", err)
		}
		now := time.Now().UTC()
		for i, stepID := range stepPromptIDs {
			brief := ""
			if i < len(briefs) {
				brief = briefs[i]
			}
			// team_id is omitted: the schema column is NOT NULL DEFAULT
			// LocalDefaultTeamID, and SQLite is N=1 local mode where every
			// prompt + blueprint pins that same sentinel team, so the
			// same-team FKs are satisfied. The Postgres impl derives team_id
			// from the blueprint in-SQL because it is multi-team.
			if _, err := q.ExecContext(ctx, `
				INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id, brief, created_at)
				VALUES (?, ?, ?, ?, ?)
			`, blueprintID, i, stepID, brief, now); err != nil {
				return fmt.Errorf("insert step %d: %w", i, err)
			}
		}
		return nil
	})
}

// --- Runs ----------------------------------------------------------------

func (s *blueprintStore) CreateRun(ctx context.Context, orgID string, br domain.BlueprintRun) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	if br.ID == "" {
		br.ID = uuid.New().String()
	}
	if br.Status == "" {
		br.Status = domain.BlueprintRunStatusRunning
	}
	if br.TriggerType == "" {
		return "", errors.New("blueprint run trigger type required")
	}
	var triggerID any
	if br.TriggerID != "" {
		triggerID = br.TriggerID
	}
	var abortReason any
	if br.AbortReason != "" {
		abortReason = br.AbortReason
	}
	var completedAt any
	if br.CompletedAt != nil {
		completedAt = br.CompletedAt.UTC()
	}
	if _, err := s.q.ExecContext(ctx, `
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, trigger_id, status, worktree_path, abort_reason, completed_at, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`, br.ID, br.BlueprintID, br.TaskID, br.TriggerType, triggerID, br.Status, br.WorktreePath, abortReason, completedAt); err != nil {
		return "", fmt.Errorf("insert blueprint_run: %w", err)
	}
	return br.ID, nil
}

func (s *blueprintStore) GetRun(ctx context.Context, orgID, id string) (*domain.BlueprintRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var (
		br            domain.BlueprintRun
		triggerID     sql.NullString
		abortReason   sql.NullString
		abortedAtStep sql.NullInt64
		completedAt   sql.NullTime
	)
	err := s.q.QueryRowContext(ctx, `
		SELECT id, blueprint_id, task_id, trigger_type, trigger_id, status,
		       abort_reason, aborted_at_step, worktree_path, started_at, completed_at
		FROM blueprint_runs WHERE id = ?
	`, id).Scan(&br.ID, &br.BlueprintID, &br.TaskID, &br.TriggerType, &triggerID, &br.Status,
		&abortReason, &abortedAtStep, &br.WorktreePath, &br.StartedAt, &completedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if triggerID.Valid {
		br.TriggerID = triggerID.String
	}
	if abortReason.Valid {
		br.AbortReason = abortReason.String
	}
	if abortedAtStep.Valid {
		i := int(abortedAtStep.Int64)
		br.AbortedAtStep = &i
	}
	if completedAt.Valid {
		t := completedAt.Time
		br.CompletedAt = &t
	}
	return &br, nil
}

func (s *blueprintStore) GetRunForRun(ctx context.Context, orgID, runID string) (*domain.BlueprintRun, *int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, nil, err
	}
	var (
		blueprintRunID sql.NullString
		stepIndex      sql.NullInt64
	)
	err := s.q.QueryRowContext(ctx, `SELECT blueprint_run_id, blueprint_step_index FROM runs WHERE id = ?`, runID).
		Scan(&blueprintRunID, &stepIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if !blueprintRunID.Valid {
		return nil, nil, nil
	}
	br, err := s.GetRun(ctx, orgID, blueprintRunID.String)
	if err != nil || br == nil {
		return nil, nil, err
	}
	var idx *int
	if stepIndex.Valid {
		v := int(stepIndex.Int64)
		idx = &v
	}
	return br, idx, nil
}

func (s *blueprintStore) GetRunForRunSystem(ctx context.Context, orgID, runID string) (*domain.BlueprintRun, *int, error) {
	return s.GetRunForRun(ctx, orgID, runID)
}

func (s *blueprintStore) MarkRunStatus(ctx context.Context, orgID, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
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
		UPDATE blueprint_runs
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

func (s *blueprintStore) RunsForBlueprint(ctx context.Context, orgID, blueprintRunID string) ([]domain.AgentRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, task_id, prompt_id, status, model, started_at, completed_at,
		       total_cost_usd, duration_ms, num_turns, stop_reason, worktree_path,
		       result_summary, session_id, outcome, outcome_reason,
		       blueprint_run_id, blueprint_step_index
		FROM runs
		WHERE blueprint_run_id = ?
		ORDER BY blueprint_step_index ASC, started_at ASC
	`, blueprintRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.AgentRun
	for rows.Next() {
		var (
			r             domain.AgentRun
			completedAt   sql.NullTime
			costUSD       sql.NullFloat64
			durationMs    sql.NullInt64
			numTurns      sql.NullInt64
			stepIdx       sql.NullInt64
			promptID      sql.NullString
			model         sql.NullString
			stopReason    sql.NullString
			worktreeP     sql.NullString
			resultSum     sql.NullString
			sessionID     sql.NullString
			outcome       sql.NullString
			outcomeReason sql.NullString
			blueprintRun  sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.TaskID, &promptID, &r.Status, &model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns, &stopReason, &worktreeP, &resultSum, &sessionID,
			&outcome, &outcomeReason, &blueprintRun, &stepIdx); err != nil {
			return nil, err
		}
		r.PromptID = promptID.String
		r.Model = model.String
		r.StopReason = stopReason.String
		r.WorktreePath = worktreeP.String
		r.ResultSummary = resultSum.String
		r.SessionID = sessionID.String
		r.Outcome = outcome.String
		r.OutcomeReason = outcomeReason.String
		if blueprintRun.Valid {
			r.BlueprintRunID = blueprintRun.String
		}
		if stepIdx.Valid {
			v := int(stepIdx.Int64)
			r.BlueprintStepIndex = &v
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

func (s *blueprintStore) ActiveStepRunIDs(ctx context.Context, orgID, blueprintRunID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE blueprint_run_id = ?
		  AND status NOT IN ('completed','failed','cancelled','task_unsolvable',
		                     'pending_approval','taken_over','awaiting_input')
	`, blueprintRunID)
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

// --- Admin-pool variants (passthrough on SQLite) -------------------------

func (s *blueprintStore) ListStepsSystem(ctx context.Context, orgID, blueprintID string) ([]domain.BlueprintStep, error) {
	return s.ListSteps(ctx, orgID, blueprintID)
}

func (s *blueprintStore) MarkRunStatusSystem(ctx context.Context, orgID, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	return s.MarkRunStatus(ctx, orgID, id, status, abortReason, abortedAtStep)
}

func (s *blueprintStore) GetRunSystem(ctx context.Context, orgID, id string) (*domain.BlueprintRun, error) {
	return s.GetRun(ctx, orgID, id)
}

func (s *blueprintStore) ActiveStepRunIDsSystem(ctx context.Context, orgID, blueprintRunID string) ([]string, error) {
	return s.ActiveStepRunIDs(ctx, orgID, blueprintRunID)
}

func (s *blueprintStore) RunsForBlueprintSystem(ctx context.Context, orgID, blueprintRunID string) ([]domain.AgentRun, error) {
	return s.RunsForBlueprint(ctx, orgID, blueprintRunID)
}
