package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// blueprintStore is the Postgres impl of db.BlueprintStore. Holds two pools:
//
//   - app: app pool (tf_app, RLS-active). Every request-equivalent consumer
//     (blueprints handler, pending_prs / reviews handlers, user-initiated
//     lifecycle entry points like CancelBlueprint and the
//     ResumeBlueprintAfter* paths) runs here.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). The blueprint
//     orchestrator goroutine — delegateBlueprint / runBlueprint /
//     terminateBlueprint — detaches from the kicking-off handler's context
//     the moment it spawns, so it has no JWT-claims in scope and routes
//     through admin via the `...System` variants. The header seeder also
//     runs here (claims-less system rows). org_id stays in the WHERE clause
//     as defense in depth.
//
// CreateRun routes internally on BlueprintRun.TriggerType, mirroring the
// AgentRunStore.Create pattern: event-triggered runs land on the admin pool
// with NULL creator_user_id, manual runs on the app pool with COALESCE
// fallback. There is no separate CreateRunSystem.
type blueprintStore struct {
	app   queryer
	admin queryer
}

func newBlueprintStore(app, admin queryer) db.BlueprintStore {
	return &blueprintStore{app: app, admin: admin}
}

var _ db.BlueprintStore = (*blueprintStore)(nil)

// --- Blueprint header CRUD -----------------------------------------------

func (s *blueprintStore) SeedOrUpdate(ctx context.Context, orgID, teamID string, b domain.Blueprint) (string, error) {
	if teamID == "" {
		return "", errors.New("postgres blueprints: SeedOrUpdate requires team_id (blueprints are team-scoped)")
	}
	if b.Source == "" {
		b.Source = "system"
	}
	if b.Source != "system" {
		return "", fmt.Errorf("postgres blueprints: SeedOrUpdate only accepts Source=\"system\" (got %q)", b.Source)
	}
	if b.SystemSlug == "" {
		return "", fmt.Errorf("postgres blueprints: SeedOrUpdate requires a non-empty SystemSlug")
	}
	now := time.Now().UTC()
	newID := uuid.New().String()
	// ON CONFLICT DO NOTHING (no versions sidecar — pushing step-list updates
	// to shipped blueprints is a future concern). System rows are team-owned
	// with no human author (blueprints_system_has_no_creator pins
	// source='system' ↔ creator_user_id NULL).
	if _, err := s.admin.ExecContext(ctx, `
		INSERT INTO blueprints (id, org_id, team_id, system_slug, creator_user_id, name, source, usage_count, user_modified, created_at, updated_at)
		VALUES ($1, $2, $3::uuid, $4, NULL, $5, $6, 0, FALSE, $7, $7)
		ON CONFLICT (org_id, team_id, system_slug) DO NOTHING
	`, newID, orgID, teamID, b.SystemSlug, b.Name, b.Source, now); err != nil {
		return "", fmt.Errorf("insert blueprint: %w", err)
	}
	var id string
	if err := s.admin.QueryRowContext(ctx,
		`SELECT id FROM blueprints WHERE org_id = $1 AND team_id = $2::uuid AND system_slug = $3`,
		orgID, teamID, b.SystemSlug,
	).Scan(&id); err != nil {
		return "", fmt.Errorf("resolve blueprint id: %w", err)
	}
	return id, nil
}

func (s *blueprintStore) List(ctx context.Context, orgID string, teamID string) ([]domain.Blueprint, error) {
	args := []any{orgID}
	q := `SELECT id, name, description, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints WHERE org_id = $1 AND hidden = FALSE`
	if teamID != "" {
		args = append(args, teamID)
		q += fmt.Sprintf(" AND team_id = $%d", len(args))
	}
	q += ` ORDER BY updated_at DESC`
	rows, err := s.app.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Blueprint
	for rows.Next() {
		b, err := scanBlueprintRowPG(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *blueprintStore) Get(ctx context.Context, orgID string, id string) (*domain.Blueprint, error) {
	return getBlueprint(ctx, s.app, orgID, id)
}

func (s *blueprintStore) GetSystem(ctx context.Context, orgID string, id string) (*domain.Blueprint, error) {
	return getBlueprint(ctx, s.admin, orgID, id)
}

func (s *blueprintStore) GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.Blueprint, error) {
	if teamID == "" {
		return nil, errors.New("postgres blueprints: GetBySystemSlug requires team_id")
	}
	b, err := scanBlueprintRowPG(s.app.QueryRowContext(ctx, `
		SELECT id, name, description, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints WHERE org_id = $1 AND team_id = $2 AND system_slug = $3
	`, orgID, teamID, systemSlug).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func getBlueprint(ctx context.Context, q queryer, orgID, id string) (*domain.Blueprint, error) {
	b, err := scanBlueprintRowPG(q.QueryRowContext(ctx, `
		SELECT id, name, description, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints WHERE org_id = $1 AND id = $2
	`, orgID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func scanBlueprintRowPG(scanFn func(dst ...any) error) (domain.Blueprint, error) {
	var b domain.Blueprint
	var systemSlug sql.NullString
	if err := scanFn(&b.ID, &b.Name, &b.Description, &b.Source, &b.UsageCount, &b.UserModified, &b.TeamID, &systemSlug, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return b, err
	}
	if systemSlug.Valid {
		b.SystemSlug = systemSlug.String
	}
	return b, nil
}

func (s *blueprintStore) Create(ctx context.Context, orgID, teamID string, b domain.Blueprint) error {
	if teamID == "" {
		return fmt.Errorf("postgres blueprints Create: team_id required (handler must thread the resolved acting team)")
	}
	if b.Source == "" {
		b.Source = "user"
	}
	var systemSlug any
	if b.SystemSlug != "" {
		systemSlug = b.SystemSlug
	}
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, description, source, system_slug, usage_count, created_at, updated_at)
		VALUES ($1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3::uuid, $4, $5, $6, $7, 0, now(), now())
	`, b.ID, orgID, teamID, b.Name, b.Description, b.Source, systemSlug)
	return err
}

// Update sets the blueprint's name + description (the metadata-popup fields).
// updated_at is maintained by the set_updated_at trigger; org_id stays in the
// WHERE as defense in depth (RLS blueprints_update gates the rest).
func (s *blueprintStore) Update(ctx context.Context, orgID, id, name, description string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE blueprints SET name = $1, description = $2
		WHERE org_id = $3 AND id = $4
	`, name, description, orgID, id)
	return err
}

// Delete hard-deletes a blueprint. Its steps and any triggers referencing it
// cascade (FK ON DELETE CASCADE); team copies are independent rows, untouched.
// RLS blueprints_delete gates the row to the caller's team.
func (s *blueprintStore) Delete(ctx context.Context, orgID, id string) error {
	_, err := s.app.ExecContext(ctx, `DELETE FROM blueprints WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *blueprintStore) IncrementUsage(ctx context.Context, orgID string, id string) error {
	_, err := s.app.ExecContext(ctx, `UPDATE blueprints SET usage_count = usage_count + 1 WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *blueprintStore) IncrementUsageSystem(ctx context.Context, orgID string, id string) error {
	_, err := s.admin.ExecContext(ctx, `UPDATE blueprints SET usage_count = usage_count + 1 WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

// --- Steps ---------------------------------------------------------------

func (s *blueprintStore) ListSteps(ctx context.Context, orgID, blueprintID string) ([]domain.BlueprintStep, error) {
	return listBlueprintSteps(ctx, s.app, orgID, blueprintID)
}

func (s *blueprintStore) ListStepsSystem(ctx context.Context, orgID, blueprintID string) ([]domain.BlueprintStep, error) {
	return listBlueprintSteps(ctx, s.admin, orgID, blueprintID)
}

func listBlueprintSteps(ctx context.Context, q queryer, orgID, blueprintID string) ([]domain.BlueprintStep, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT blueprint_id, step_index, step_prompt_id, brief, created_at
		FROM blueprint_steps
		WHERE org_id = $1 AND blueprint_id = $2
		ORDER BY step_index ASC
	`, orgID, blueprintID)
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
	var n int
	err := s.app.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT blueprint_id)
		FROM blueprint_steps
		WHERE org_id = $1 AND step_prompt_id = $2
	`, orgID, stepPromptID).Scan(&n)
	return n, err
}

func (s *blueprintStore) ReplaceSteps(ctx context.Context, orgID, blueprintID string, stepPromptIDs, briefs []string) error {
	if len(briefs) != 0 && len(briefs) != len(stepPromptIDs) {
		return fmt.Errorf("briefs length %d must match stepPromptIDs length %d", len(briefs), len(stepPromptIDs))
	}
	return s.runInTx(ctx, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM blueprint_steps WHERE org_id = $1 AND blueprint_id = $2`,
			orgID, blueprintID); err != nil {
			return fmt.Errorf("clear existing steps: %w", err)
		}
		now := time.Now().UTC()
		for i, stepID := range stepPromptIDs {
			brief := ""
			if i < len(briefs) {
				brief = briefs[i]
			}
			// team_id is derived from the blueprint in-SQL so the same-team
			// composite FKs ((blueprint_id|step_prompt_id, team_id) →
			// {blueprints|prompts}(id, team_id)) enforce that every step
			// belongs to the blueprint's own team — a cross-team step is
			// refused at the DB layer.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
				VALUES ($1, (SELECT team_id FROM blueprints WHERE id = $2 AND org_id = $1), $2, $3, $4, $5, $6)
			`, orgID, blueprintID, i, stepID, brief, now); err != nil {
				return fmt.Errorf("insert step %d: %w", i, err)
			}
		}
		return nil
	})
}

// --- Runs ----------------------------------------------------------------

func (s *blueprintStore) CreateRun(ctx context.Context, orgID string, br domain.BlueprintRun) (string, error) {
	if br.ID == "" {
		br.ID = uuid.New().String()
	}
	if br.Status == "" {
		br.Status = domain.BlueprintRunStatusRunning
	}
	if br.TriggerType == "" {
		return "", errors.New("blueprint run trigger type required")
	}
	if br.TriggerType == domain.BlueprintTriggerEvent {
		return s.createRunEventTriggered(ctx, orgID, br)
	}
	return s.createRunManual(ctx, orgID, br)
}

func (s *blueprintStore) createRunEventTriggered(ctx context.Context, orgID string, br domain.BlueprintRun) (string, error) {
	triggerID, abortReason, completedAt := blueprintRunArgs(br)
	if _, err := s.admin.ExecContext(ctx, `
		INSERT INTO blueprint_runs
			(id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, trigger_id,
			 status, worktree_path, abort_reason, completed_at, started_at)
		VALUES (
			$1, $2, NULL,
			$3, $4, $5, $6,
			$7, $8, $9, $10, now()
		)
	`, br.ID, orgID, br.BlueprintID, br.TaskID, br.TriggerType, triggerID, br.Status, br.WorktreePath, abortReason, completedAt); err != nil {
		return "", fmt.Errorf("insert blueprint_run (event): %w", err)
	}
	return br.ID, nil
}

func (s *blueprintStore) createRunManual(ctx context.Context, orgID string, br domain.BlueprintRun) (string, error) {
	triggerID, abortReason, completedAt := blueprintRunArgs(br)
	if _, err := s.app.ExecContext(ctx, `
		INSERT INTO blueprint_runs
			(id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, trigger_id,
			 status, worktree_path, abort_reason, completed_at, started_at)
		VALUES (
			$1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3, $4, $5, $6,
			$7, $8, $9, $10, now()
		)
	`, br.ID, orgID, br.BlueprintID, br.TaskID, br.TriggerType, triggerID, br.Status, br.WorktreePath, abortReason, completedAt); err != nil {
		return "", fmt.Errorf("insert blueprint_run (manual): %w", err)
	}
	return br.ID, nil
}

func blueprintRunArgs(br domain.BlueprintRun) (triggerID, abortReason, completedAt any) {
	if br.TriggerID != "" {
		triggerID = br.TriggerID
	}
	if br.AbortReason != "" {
		abortReason = br.AbortReason
	}
	if br.CompletedAt != nil {
		completedAt = br.CompletedAt.UTC()
	}
	return
}

func (s *blueprintStore) GetRun(ctx context.Context, orgID, id string) (*domain.BlueprintRun, error) {
	return getBlueprintRun(ctx, s.app, orgID, id)
}

func (s *blueprintStore) GetRunSystem(ctx context.Context, orgID, id string) (*domain.BlueprintRun, error) {
	return getBlueprintRun(ctx, s.admin, orgID, id)
}

func getBlueprintRun(ctx context.Context, q queryer, orgID, id string) (*domain.BlueprintRun, error) {
	if !isValidUUID(id) {
		return nil, nil
	}
	var (
		br            domain.BlueprintRun
		triggerID     sql.NullString
		abortReason   sql.NullString
		abortedAtStep sql.NullInt64
		completedAt   sql.NullTime
	)
	err := q.QueryRowContext(ctx, `
		SELECT id, blueprint_id, task_id, trigger_type, trigger_id, status,
		       abort_reason, aborted_at_step, worktree_path, started_at, completed_at
		FROM blueprint_runs WHERE org_id = $1 AND id = $2
	`, orgID, id).Scan(&br.ID, &br.BlueprintID, &br.TaskID, &br.TriggerType, &triggerID, &br.Status,
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
	return getRunForBlueprintRun(ctx, s.app, s.GetRun, orgID, runID)
}

func (s *blueprintStore) GetRunForRunSystem(ctx context.Context, orgID, runID string) (*domain.BlueprintRun, *int, error) {
	return getRunForBlueprintRun(ctx, s.admin, s.GetRunSystem, orgID, runID)
}

func getRunForBlueprintRun(ctx context.Context, q queryer, getRun func(context.Context, string, string) (*domain.BlueprintRun, error), orgID, runID string) (*domain.BlueprintRun, *int, error) {
	if !isValidUUID(runID) {
		return nil, nil, nil
	}
	blueprintRunID, stepIndex, err := readRunBlueprintPointer(ctx, q, orgID, runID)
	if err != nil || blueprintRunID == "" {
		return nil, nil, err
	}
	br, err := getRun(ctx, orgID, blueprintRunID)
	if err != nil || br == nil {
		return nil, nil, err
	}
	return br, stepIndex, nil
}

// readRunBlueprintPointer reads runs.blueprint_run_id + blueprint_step_index
// for a single run.
func readRunBlueprintPointer(ctx context.Context, q queryer, orgID, runID string) (string, *int, error) {
	var (
		blueprintRunID sql.NullString
		stepIndex      sql.NullInt64
	)
	err := q.QueryRowContext(ctx,
		`SELECT blueprint_run_id, blueprint_step_index FROM runs WHERE org_id = $1 AND id = $2`,
		orgID, runID).Scan(&blueprintRunID, &stepIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if !blueprintRunID.Valid {
		return "", nil, nil
	}
	var idx *int
	if stepIndex.Valid {
		v := int(stepIndex.Int64)
		idx = &v
	}
	return blueprintRunID.String, idx, nil
}

func (s *blueprintStore) MarkRunStatus(ctx context.Context, orgID, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	return markBlueprintRunStatus(ctx, s.app, orgID, id, status, abortReason, abortedAtStep)
}

func (s *blueprintStore) MarkRunStatusSystem(ctx context.Context, orgID, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	return markBlueprintRunStatus(ctx, s.admin, orgID, id, status, abortReason, abortedAtStep)
}

func markBlueprintRunStatus(ctx context.Context, q queryer, orgID, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
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
		UPDATE blueprint_runs
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

func (s *blueprintStore) RunsForBlueprint(ctx context.Context, orgID, blueprintRunID string) ([]domain.AgentRun, error) {
	return runsForBlueprint(ctx, s.app, orgID, blueprintRunID)
}

func (s *blueprintStore) RunsForBlueprintSystem(ctx context.Context, orgID, blueprintRunID string) ([]domain.AgentRun, error) {
	return runsForBlueprint(ctx, s.admin, orgID, blueprintRunID)
}

func (s *blueprintStore) ActiveStepRunIDs(ctx context.Context, orgID, blueprintRunID string) ([]string, error) {
	return blueprintActiveStepRunIDs(ctx, s.app, orgID, blueprintRunID)
}

func (s *blueprintStore) ActiveStepRunIDsSystem(ctx context.Context, orgID, blueprintRunID string) ([]string, error) {
	return blueprintActiveStepRunIDs(ctx, s.admin, orgID, blueprintRunID)
}

func blueprintActiveStepRunIDs(ctx context.Context, q queryer, orgID, blueprintRunID string) ([]string, error) {
	if !isValidUUID(blueprintRunID) {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM runs
		WHERE org_id = $1 AND blueprint_run_id = $2
		  AND status NOT IN ('completed','failed','cancelled','task_unsolvable',
		                     'pending_approval','taken_over','awaiting_input')
	`, orgID, blueprintRunID)
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

func runsForBlueprint(ctx context.Context, q queryer, orgID, blueprintRunID string) ([]domain.AgentRun, error) {
	if !isValidUUID(blueprintRunID) {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id, task_id, prompt_id, status, model, started_at, completed_at,
		       total_cost_usd, duration_ms, num_turns, stop_reason, worktree_path,
		       result_summary, session_id, outcome, outcome_reason,
		       blueprint_run_id, blueprint_step_index
		FROM runs
		WHERE org_id = $1 AND blueprint_run_id = $2
		ORDER BY blueprint_step_index ASC, started_at ASC
	`, orgID, blueprintRunID)
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

// runInTx is the multi-statement helper for ReplaceSteps. Composes with the
// caller's *sql.Tx inside WithTx; otherwise opens a fresh tx on the app pool.
func (s *blueprintStore) runInTx(ctx context.Context, fn func(queryer) error) error {
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
		return errors.New("postgres blueprints: unexpected queryer type")
	}
}
