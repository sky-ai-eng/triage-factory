package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// blueprintStore is the Postgres impl of db.BlueprintStore. Holds two pools:
//
//   - app: app pool (tf_app, RLS-active). Every request-equivalent consumer
//     (blueprints handler, artifact-PR / reviews handlers, user-initiated
//     lifecycle entry points like CancelBlueprint and the
//     ResumeBlueprintAfter* paths) runs here.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). The blueprint
//     orchestrator goroutine — delegateBlueprint / runBlueprint /
//     terminateBlueprint — detaches from the kicking-off handler's context
//     the moment it spawns, so it has no JWT-claims in scope and routes
//     through admin via the `...System` variants. org_id stays in the WHERE
//     clause as defense in depth.
//
// CreateRun routes internally on BlueprintRun.TriggerType, mirroring the
// ConversationStore.Create pattern: event-triggered runs land on the admin pool
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

func (s *blueprintStore) List(ctx context.Context, orgID string, f db.BlueprintListFilter, opts db.ListOpts) ([]domain.Blueprint, int, error) {
	args := []any{orgID}
	where := ` WHERE org_id = $1 AND hidden = FALSE AND deleted_at IS NULL`
	if f.TeamID != "" {
		args = append(args, f.TeamID)
		where += fmt.Sprintf(" AND team_id = $%d", len(args))
	}
	if len(f.GatedEventTypes) > 0 {
		// Hide a blueprint iff it HAS triggers and NONE of them fires on an
		// ungated type. Spelled as the negation of "has triggers and has no
		// usable trigger" so a trigger-less blueprint falls through visible.
		args = append(args, f.GatedEventTypes)
		gated := fmt.Sprintf("$%d", len(args))
		where += `
		  AND NOT (
		    EXISTS (SELECT 1 FROM event_handlers h
		             WHERE h.org_id = blueprints.org_id AND h.blueprint_id = blueprints.id
		               AND h.kind = 'trigger')
		    AND NOT EXISTS (SELECT 1 FROM event_handlers h
		                     WHERE h.org_id = blueprints.org_id AND h.blueprint_id = blueprints.id
		                       AND h.kind = 'trigger' AND NOT (h.event_type = ANY(` + gated + `)))
		  )`
	}

	var total int
	if err := s.app.QueryRowContext(ctx, `SELECT COUNT(*) FROM blueprints`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT id, name, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints` + where + ` ORDER BY updated_at DESC, id`
	pageArgs := args
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		pageArgs = append(append([]any{}, args...), opts.Limit, opts.Offset)
	}
	rows, err := s.app.QueryContext(ctx, q, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.Blueprint
	for rows.Next() {
		b, err := scanBlueprintRowPG(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, b)
	}
	return out, total, rows.Err()
}

// Get is request-facing: it filters deleted_at IS NULL. GetSystem (admin pool)
// omits the filter so a soft-deleted blueprint still resolves for in-flight
// runs and past-run timelines.
func (s *blueprintStore) Get(ctx context.Context, orgID string, id string) (*domain.Blueprint, error) {
	return getBlueprint(ctx, s.app, orgID, id, false)
}

func (s *blueprintStore) GetSystem(ctx context.Context, orgID string, id string) (*domain.Blueprint, error) {
	return getBlueprint(ctx, s.admin, orgID, id, true)
}

func (s *blueprintStore) GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.Blueprint, error) {
	if teamID == "" {
		return nil, errors.New("postgres blueprints: GetBySystemSlug requires team_id")
	}
	b, err := scanBlueprintRowPG(s.app.QueryRowContext(ctx, `
		SELECT id, name, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints WHERE org_id = $1 AND team_id = $2 AND system_slug = $3 AND deleted_at IS NULL
	`, orgID, teamID, systemSlug).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func getBlueprint(ctx context.Context, q queryer, orgID, id string, includeDeleted bool) (*domain.Blueprint, error) {
	query := `
		SELECT id, name, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints WHERE org_id = $1 AND id = $2`
	if !includeDeleted {
		query += ` AND deleted_at IS NULL`
	}
	b, err := scanBlueprintRowPG(q.QueryRowContext(ctx, query, orgID, id).Scan)
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
	if err := scanFn(&b.ID, &b.Name, &b.Source, &b.UsageCount, &b.UserModified, &b.TeamID, &systemSlug, &b.CreatedAt, &b.UpdatedAt); err != nil {
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
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source, system_slug, usage_count, created_at, updated_at)
		VALUES ($1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3::uuid, $4, $5, $6, 0, now(), now())
	`, b.ID, orgID, teamID, b.Name, b.Source, systemSlug)
	return err
}

// Rename stamps user_modified=TRUE alongside the name change — the sync
// signal (see db.BlueprintStore's stamping contract) that this team's copy
// diverged from shipped content.
func (s *blueprintStore) Rename(ctx context.Context, orgID, id, name string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE blueprints SET name = $1, updated_at = now(), user_modified = TRUE
		WHERE org_id = $2 AND id = $3 AND deleted_at IS NULL
	`, name, orgID, id)
	return err
}

// Delete soft-deletes a blueprint (stamps deleted_at). Its blueprint_steps stay
// so the copy-only unique index keeps the wrapped prompt pinned; the prompt is
// soft-deleted alongside by the delete-pairing.
func (s *blueprintStore) Delete(ctx context.Context, orgID string, id string) error {
	res, err := s.app.ExecContext(ctx, `UPDATE blueprints SET deleted_at = now() WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`, orgID, id)
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
// copy-only unique index (org_id, step_prompt_id) guarantees at most one row.
// Only considers non-deleted blueprints — a step row in a soft-deleted
// blueprint does not count as ownership (the step persists as audit trail;
// the prompt is free to be claimed by a new blueprint).
func (s *blueprintStore) StepPromptOwner(ctx context.Context, orgID string, promptID string) (string, bool, error) {
	var blueprintID string
	err := s.app.QueryRowContext(ctx, `
		SELECT bs.blueprint_id
		FROM blueprint_steps bs
		JOIN blueprints b ON b.id = bs.blueprint_id AND b.org_id = bs.org_id
		WHERE bs.org_id = $1 AND bs.step_prompt_id = $2 AND b.deleted_at IS NULL
		LIMIT 1
	`, orgID, promptID).Scan(&blueprintID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return blueprintID, true, nil
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

// ListAllSteps returns every step of teamID's request-facing blueprints in one
// read. The blueprints join mirrors List's gate (hidden = FALSE, deleted_at IS
// NULL, optional team_id) so the canvas's bulk steps fetch lines up exactly with
// the blueprint list it renders from.
func (s *blueprintStore) ListAllSteps(ctx context.Context, orgID string, f db.BlueprintStepListFilter, opts db.ListOpts) ([]domain.BlueprintStep, int, error) {
	args := []any{orgID}
	where := `
		FROM blueprint_steps bs
		JOIN blueprints b ON b.id = bs.blueprint_id AND b.org_id = bs.org_id
		WHERE bs.org_id = $1 AND b.hidden = FALSE AND b.deleted_at IS NULL`
	if f.TeamID != "" {
		args = append(args, f.TeamID)
		where += fmt.Sprintf(" AND b.team_id = $%d", len(args))
	}
	if len(f.BlueprintIDs) > 0 {
		args = append(args, f.BlueprintIDs)
		where += fmt.Sprintf(" AND bs.blueprint_id = ANY($%d)", len(args))
	}

	var total int
	if err := s.app.QueryRowContext(ctx, `SELECT COUNT(*)`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count blueprint steps: %w", err)
	}

	q := `
		SELECT bs.blueprint_id, bs.step_index, bs.step_prompt_id, bs.brief, bs.created_at` +
		where + ` ORDER BY bs.blueprint_id, bs.step_index`
	pageArgs := args
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		pageArgs = append(append([]any{}, args...), opts.Limit, opts.Offset)
	}
	rows, err := s.app.QueryContext(ctx, q, pageArgs...)
	if err != nil {
		return nil, 0, fmt.Errorf("query all blueprint steps: %w", err)
	}
	defer rows.Close()
	var out []domain.BlueprintStep
	for rows.Next() {
		var st domain.BlueprintStep
		if err := rows.Scan(&st.BlueprintID, &st.StepIndex, &st.StepPromptID, &st.Brief, &st.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, st)
	}
	return out, total, rows.Err()
}

func (s *blueprintStore) CountStepReferences(ctx context.Context, orgID, stepPromptID string) (int, error) {
	var n int
	err := s.app.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT bs.blueprint_id)
		FROM blueprint_steps bs
		JOIN blueprints b ON b.id = bs.blueprint_id AND b.org_id = bs.org_id
		WHERE bs.org_id = $1 AND bs.step_prompt_id = $2 AND b.deleted_at IS NULL
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
		// Stamp user_modified — the step list is part of the sync unit's
		// content (see db.BlueprintStore's stamping contract).
		if _, err := tx.ExecContext(ctx,
			`UPDATE blueprints SET updated_at = now(), user_modified = TRUE WHERE org_id = $1 AND id = $2`,
			orgID, blueprintID); err != nil {
			return fmt.Errorf("stamp user_modified: %w", err)
		}
		return nil
	})
}

// reparentBlueprintStepsPG moves the steps of fromBlueprint whose step_index
// is >= minIndex onto toBlueprint, shifting each step_index by indexShift. It
// is the shared mechanic behind MergeInto and SplitAt. org_id is threaded
// through the WHERE for defense in depth alongside RLS. team_id is left
// unchanged: both blueprints share a team (same-team validated by the caller),
// so the (blueprint_id, team_id) composite FK still resolves to the new owner.
// step_prompt_id is untouched, so the copy-only step uniqueness holds.
func reparentBlueprintStepsPG(ctx context.Context, q queryer, orgID, fromBlueprint, toBlueprint string, indexShift, minIndex int) error {
	_, err := q.ExecContext(ctx, `
		UPDATE blueprint_steps
		SET blueprint_id = $1, step_index = step_index + $2
		WHERE org_id = $3 AND blueprint_id = $4 AND step_index >= $5
	`, toBlueprint, indexShift, orgID, fromBlueprint, minIndex)
	return err
}

func (s *blueprintStore) MergeInto(ctx context.Context, orgID, hostID, sourceID string) error {
	return s.runInTx(ctx, func(tx queryer) error {
		var hostLen int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM blueprint_steps WHERE org_id = $1 AND blueprint_id = $2`,
			orgID, hostID).Scan(&hostLen); err != nil {
			return fmt.Errorf("count host steps: %w", err)
		}
		// Append source's steps after the host's, densely (host_len + i).
		if err := reparentBlueprintStepsPG(ctx, tx, orgID, sourceID, hostID, hostLen, 0); err != nil {
			return fmt.Errorf("reparent source steps: %w", err)
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE blueprints SET deleted_at = now() WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`,
			orgID, sourceID)
		if err != nil {
			return fmt.Errorf("soft-delete source: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("blueprint %s not found or already deleted", sourceID)
		}
		// Bump host.updated_at (the set_updated_at trigger also stamps it, but
		// the row must be touched for the trigger to fire) and stamp
		// user_modified true (the source is retired, not stamped).
		if _, err := tx.ExecContext(ctx,
			`UPDATE blueprints SET updated_at = now(), user_modified = TRUE WHERE org_id = $1 AND id = $2`, orgID, hostID); err != nil {
			return fmt.Errorf("bump host updated_at: %w", err)
		}
		return nil
	})
}

// insertTriggerlessBlueprintPG creates a fresh, trigger-less user blueprint,
// deriving its team (and the same-team FK target) from srcBlueprintID so the
// reparented steps' (blueprint_id, team_id) FK still resolves. Mirrors Create's
// COALESCE(current_user_id, org owner) creator default. Shared by SplitAt and
// DeleteStep. Born with user_modified=TRUE: a split/delete product is never a
// shipped row (system_slug stays NULL, omitted below), so it starts already
// diverged per db.BlueprintStore's stamping contract.
func insertTriggerlessBlueprintPG(ctx context.Context, q queryer, orgID, id, name, srcBlueprintID string) error {
	res, err := q.ExecContext(ctx, `
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source, usage_count, user_modified, created_at, updated_at)
		SELECT $1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			b.team_id, $3, 'user', 0, TRUE, now(), now()
		FROM blueprints b WHERE b.id = $4 AND b.org_id = $2
	`, id, orgID, name, srcBlueprintID)
	if err != nil {
		return err
	}
	// The SELECT derives team_id from the source blueprint; a 0-row insert means
	// srcBlueprintID is absent (or out of org). Guard so a missing source surfaces
	// here rather than as a cryptic FK error on the follow-up reparent.
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("triggerless blueprint: source %s not found", srcBlueprintID)
	}
	return nil
}

// softDeleteBlueprintTxPG stamps deleted_at (+ user_modified, since retiring a
// blueprint via DeleteStep's head case is itself a structural edit — its only
// caller) inside an open tx. Errors when the row is missing or already deleted.
func softDeleteBlueprintTxPG(ctx context.Context, q queryer, orgID, id string) error {
	res, err := q.ExecContext(ctx,
		`UPDATE blueprints SET deleted_at = now(), user_modified = TRUE WHERE org_id = $1 AND id = $2 AND deleted_at IS NULL`, orgID, id)
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

// insertIsolationBlueprintPG creates the born-soft-deleted 1-step blueprint that
// holds the deleted step's row, named after that step's prompt (the name is
// never surfaced — the wrapper is soft-deleted on creation). Team is derived
// from srcBlueprintID so the reparented step's same-team FK resolves.
func insertIsolationBlueprintPG(ctx context.Context, q queryer, orgID, id, srcBlueprintID string, stepIndex int) error {
	res, err := q.ExecContext(ctx, `
		INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source, usage_count, created_at, updated_at, deleted_at)
		SELECT $1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			b.team_id, p.name, 'user', 0, now(), now(), now()
		FROM blueprints b
		JOIN blueprint_steps bs ON bs.blueprint_id = b.id AND bs.org_id = b.org_id
		JOIN prompts p ON p.id = bs.step_prompt_id AND p.org_id = bs.org_id
		WHERE b.id = $3 AND b.org_id = $2 AND bs.step_index = $4
	`, id, orgID, srcBlueprintID, stepIndex)
	if err != nil {
		return err
	}
	// The SELECT resolves the deleted step's prompt; a 0-row insert means that
	// step is missing. The caller validates it exists, so this guards against a
	// cryptic FK error on the follow-up reparent rather than an expected path.
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("isolation blueprint: step %d of %s not found", stepIndex, srcBlueprintID)
	}
	return nil
}

func (s *blueprintStore) SplitAt(ctx context.Context, orgID, id string, atIndex int, newBlueprintID, newName string) (string, error) {
	err := s.runInTx(ctx, func(tx queryer) error {
		// Create the new trigger-less downstream blueprint (team derived from the
		// blueprint being split so both halves share a team).
		if err := insertTriggerlessBlueprintPG(ctx, tx, orgID, newBlueprintID, newName, id); err != nil {
			return fmt.Errorf("create downstream blueprint: %w", err)
		}
		// Peel steps [atIndex, N) onto the new blueprint, re-densified 0-based.
		if err := reparentBlueprintStepsPG(ctx, tx, orgID, id, newBlueprintID, -atIndex, atIndex); err != nil {
			return fmt.Errorf("reparent tail steps: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE blueprints SET updated_at = now(), user_modified = TRUE WHERE org_id = $1 AND id = $2`, orgID, id); err != nil {
			return fmt.Errorf("bump upstream updated_at: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newBlueprintID, nil
}

func (s *blueprintStore) DeleteStep(ctx context.Context, orgID, blueprintID string, stepIndex int, newName string) (string, error) {
	var downstreamID string
	err := s.runInTx(ctx, func(tx queryer) error {
		var n int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM blueprint_steps WHERE org_id = $1 AND blueprint_id = $2`,
			orgID, blueprintID).Scan(&n); err != nil {
			return fmt.Errorf("count steps: %w", err)
		}
		if n < 2 {
			return fmt.Errorf("DeleteStep requires a multi-step blueprint, got %d step(s)", n)
		}
		if stepIndex < 0 || stepIndex >= n {
			return fmt.Errorf("step index %d out of range [0,%d)", stepIndex, n)
		}
		switch stepIndex {
		case 0:
			// Head: peel [1,N) onto a fresh trigger-less downstream blueprint,
			// leaving the original holding only the deleted entry step; soft-delete
			// it (the caller detaches its trigger separately).
			downstreamID = uuid.New().String()
			if err := insertTriggerlessBlueprintPG(ctx, tx, orgID, downstreamID, newName, blueprintID); err != nil {
				return fmt.Errorf("create downstream blueprint: %w", err)
			}
			if err := reparentBlueprintStepsPG(ctx, tx, orgID, blueprintID, downstreamID, -1, 1); err != nil {
				return fmt.Errorf("reparent downstream steps: %w", err)
			}
			if err := softDeleteBlueprintTxPG(ctx, tx, orgID, blueprintID); err != nil {
				return err
			}
		case n - 1:
			// Tail: isolate the last step onto a fresh soft-deleted 1-step
			// blueprint; the original keeps [0,N-1) with its trigger + id.
			isolationID := uuid.New().String()
			if err := insertIsolationBlueprintPG(ctx, tx, orgID, isolationID, blueprintID, stepIndex); err != nil {
				return fmt.Errorf("create isolation blueprint: %w", err)
			}
			if err := reparentBlueprintStepsPG(ctx, tx, orgID, blueprintID, isolationID, -stepIndex, stepIndex); err != nil {
				return fmt.Errorf("isolate deleted step: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE blueprints SET updated_at = now(), user_modified = TRUE WHERE org_id = $1 AND id = $2`, orgID, blueprintID); err != nil {
				return fmt.Errorf("bump upstream updated_at: %w", err)
			}
		default:
			// Mid: the original keeps [0,stepIndex) with its trigger + id; peel
			// [stepIndex+1,N) onto a fresh trigger-less downstream blueprint, then
			// isolate the deleted step onto a fresh soft-deleted 1-step blueprint.
			downstreamID = uuid.New().String()
			if err := insertTriggerlessBlueprintPG(ctx, tx, orgID, downstreamID, newName, blueprintID); err != nil {
				return fmt.Errorf("create downstream blueprint: %w", err)
			}
			if err := reparentBlueprintStepsPG(ctx, tx, orgID, blueprintID, downstreamID, -(stepIndex + 1), stepIndex+1); err != nil {
				return fmt.Errorf("reparent downstream steps: %w", err)
			}
			isolationID := uuid.New().String()
			if err := insertIsolationBlueprintPG(ctx, tx, orgID, isolationID, blueprintID, stepIndex); err != nil {
				return fmt.Errorf("create isolation blueprint: %w", err)
			}
			if err := reparentBlueprintStepsPG(ctx, tx, orgID, blueprintID, isolationID, -stepIndex, stepIndex); err != nil {
				return fmt.Errorf("isolate deleted step: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE blueprints SET updated_at = now(), user_modified = TRUE WHERE org_id = $1 AND id = $2`, orgID, blueprintID); err != nil {
				return fmt.Errorf("bump upstream updated_at: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return downstreamID, nil
}

func (s *blueprintStore) DuplicatePrompts(ctx context.Context, orgID, teamID string, promptIDs []string) ([]string, error) {
	if teamID == "" {
		return nil, fmt.Errorf("postgres blueprints DuplicatePrompts: team_id required (handler must thread the resolved acting team)")
	}
	ids := db.DedupPreserveOrder(promptIDs)
	if len(ids) == 0 {
		return nil, db.ErrDuplicateNoPrompts
	}
	var newIDs []string
	err := s.runInTx(ctx, func(tx queryer) error {
		// Resolve each prompt id → its step + prompt payload. org_id is threaded
		// through every join for defense in depth alongside RLS; copy-only means
		// a live prompt is a step of at most one non-deleted blueprint.
		resolved := make([]db.DuplicationStep, 0, len(ids))
		for _, pid := range ids {
			var (
				st   db.DuplicationStep
				team string
			)
			err := tx.QueryRowContext(ctx, `
				SELECT bs.blueprint_id, b.name, b.team_id::text, bs.step_index, bs.brief,
				       p.name, p.body, p.model, p.allowed_tools,
				       (SELECT COUNT(*) FROM blueprint_steps bs2
				        WHERE bs2.org_id = $1 AND bs2.blueprint_id = bs.blueprint_id)
				FROM blueprint_steps bs
				JOIN blueprints b ON b.id = bs.blueprint_id AND b.org_id = bs.org_id
				JOIN prompts p ON p.id = bs.step_prompt_id AND p.org_id = bs.org_id
				WHERE bs.org_id = $1 AND bs.step_prompt_id = $2 AND b.deleted_at IS NULL AND p.deleted_at IS NULL
			`, orgID, pid).Scan(&st.BlueprintID, &st.BlueprintName, &team, &st.StepIndex, &st.Brief,
				&st.PromptName, &st.PromptBody, &st.PromptModel, &st.PromptTools, &st.BlueprintTotal)
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrDuplicatePromptNotFound
			}
			if err != nil {
				return fmt.Errorf("resolve prompt %s: %w", pid, err)
			}
			if team != teamID {
				return db.ErrDuplicateCrossTeam
			}
			resolved = append(resolved, st)
		}

		for _, run := range db.PartitionDuplicationRuns(resolved) {
			bpID := uuid.New().String()
			// New blueprint: trigger-less, user source, attributed to the acting
			// team (validated == every source's team). Creator mirrors Create's
			// COALESCE(current_user_id, org owner).
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO blueprints (id, org_id, creator_user_id, team_id, name, source, usage_count, created_at, updated_at)
				VALUES ($1, $2,
					COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
					$3::uuid, $4, 'user', 0, now(), now())
			`, bpID, orgID, teamID, run.Name); err != nil {
				return fmt.Errorf("create duplicated blueprint: %w", err)
			}
			for i, step := range run.Steps {
				// Deep-copy the prompt into a fresh, independent user row
				// (system_slug NULL — a copy of a system/imported prompt decouples).
				newPromptID := uuid.New().String()
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO prompts (id, org_id, creator_user_id, team_id, name, body, source, allowed_tools, model, usage_count, created_at, updated_at)
					VALUES ($1, $2,
						COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
						$3::uuid, $4, $5, 'user', $6, $7, 0, now(), now())
				`, newPromptID, orgID, teamID, step.PromptName, step.PromptBody, step.PromptTools, step.PromptModel); err != nil {
					return fmt.Errorf("copy prompt: %w", err)
				}
				// team_id on the step matches the new blueprint + prompt so the
				// same-team composite FKs resolve.
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
					VALUES ($1, $2::uuid, $3, $4, $5, $6, now())
				`, orgID, teamID, bpID, i, newPromptID, step.Brief); err != nil {
					return fmt.Errorf("insert duplicated step %d: %w", i, err)
				}
			}
			newIDs = append(newIDs, bpID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return newIDs, nil
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
	stepPlan, err := domain.MarshalStepPlan(br.StepPlan)
	if err != nil {
		return "", fmt.Errorf("marshal step plan: %w", err)
	}
	if _, err := s.admin.ExecContext(ctx, `
		INSERT INTO blueprint_runs
			(id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, trigger_id, triggering_event_id,
			 actor_agent_id, status, worktree_path, abort_reason, completed_at, started_at, step_plan, entity_id)
		VALUES (
			$1, $2, NULL,
			$3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12, now(), $13,
			(SELECT entity_id FROM tasks WHERE id = $4 AND org_id = $2)
		)
	`, br.ID, orgID, br.BlueprintID, br.TaskID, br.TriggerType, triggerID, nullIfEmpty(br.TriggeringEventID), nullIfEmpty(br.ActorAgentID), br.Status, br.WorktreePath, abortReason, completedAt, stepPlan); err != nil {
		// Same one-active-per-task translation as the fenced insert:
		// production event runs route through CreateRunIfNotFiredSystem
		// today, but any caller reaching this path (tests seeding event
		// runs, a future direct CreateRun user) deserves the documented
		// sentinel, not a raw unique_violation.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == blueprintRunsOneActivePerTaskConstraint {
			return "", db.ErrTaskBusyActiveAutoRun
		}
		return "", fmt.Errorf("insert blueprint_run (event): %w", err)
	}
	return br.ID, nil
}

func (s *blueprintStore) createRunManual(ctx context.Context, orgID string, br domain.BlueprintRun) (string, error) {
	triggerID, abortReason, completedAt := blueprintRunArgs(br)
	stepPlan, err := domain.MarshalStepPlan(br.StepPlan)
	if err != nil {
		return "", fmt.Errorf("marshal step plan: %w", err)
	}
	if _, err := s.app.ExecContext(ctx, `
		INSERT INTO blueprint_runs
			(id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, trigger_id, triggering_event_id,
			 actor_agent_id, status, worktree_path, abort_reason, completed_at, started_at, step_plan)
		VALUES (
			$1, $2,
			COALESCE(tf.current_user_id(), (SELECT owner_user_id FROM orgs WHERE id = $2)),
			$3, $4, $5, $6, $7,
			$8, $9, $10, $11, $12, now(), $13
		)
	`, br.ID, orgID, br.BlueprintID, br.TaskID, br.TriggerType, triggerID, nullIfEmpty(br.TriggeringEventID), nullIfEmpty(br.ActorAgentID), br.Status, br.WorktreePath, abortReason, completedAt, stepPlan); err != nil {
		return "", fmt.Errorf("insert blueprint_run (manual): %w", err)
	}
	return br.ID, nil
}

// blueprintRunsOneActivePerTaskConstraint is the partial unique index name
// backing "at most one active (trigger_type='event', status='running')
// blueprint_run per task" — the DB-enforced twin of the router's in-process
// task gate (HasActiveAutoRunForTaskSystem), which is check-then-act: two
// processes (or a leader-failover overlap within one) could both pass the
// check and each mint an active auto run on the same task via different
// triggers. See the migration for the full index definition.
const blueprintRunsOneActivePerTaskConstraint = "blueprint_runs_one_active_auto_run_per_task"

// CreateRunIfNotFiredSystem is the event-path fenced insert (admin pool).
// Two independent unique constraints can turn this insert into a clean
// no-op instead of a duplicate:
//
//   - blueprint_runs_event_trigger_fence (ON CONFLICT, inference-targeted):
//     a replayed (triggering_event_id, trigger_id) — the at-least-once event
//     queue redelivering an event whose first auto-delegation already fired.
//   - blueprint_runs_one_active_auto_run_per_task (caught below): a
//     DIFFERENT (event, trigger) pair racing to fire on the SAME task
//     while another auto run is still active on it. A single INSERT's ON
//     CONFLICT can only target one arbiter index, so this second case
//     isn't inference-eligible — it surfaces as a raw unique_violation,
//     translated to db.ErrTaskBusyActiveAutoRun. NOT the inserted=false
//     contract: a replay is permanently satisfied, task-busy is a
//     deferral — the caller must queue the intent, not drop it.
//
// The run row and the task's agent claim commit together — see
// db.AgentClaimStamp for why they are inseparable. A stamp refusal is not an
// error and leaves the run committed; a stamp *failure* rolls the run back,
// so the firing path retries the pair rather than committing half of it.
func (s *blueprintStore) CreateRunIfNotFiredSystem(ctx context.Context, orgID string, br domain.BlueprintRun, claim db.AgentClaimStamp) (bool, bool, error) {
	if br.TriggeringEventID == "" || br.TriggerID == "" {
		return false, false, db.ErrBlueprintRunFenceRequiresEventAndTrigger
	}
	if br.ID == "" {
		br.ID = uuid.New().String()
	}
	if br.Status == "" {
		br.Status = domain.BlueprintRunStatusRunning
	}
	stepPlan, err := domain.MarshalStepPlan(br.StepPlan)
	if err != nil {
		return false, false, fmt.Errorf("marshal step plan: %w", err)
	}
	inserted, claimed := false, false
	err = inTx(ctx, s.admin, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			INSERT INTO blueprint_runs
				(id, org_id, creator_user_id, blueprint_id, task_id, trigger_type, trigger_id, triggering_event_id,
				 actor_agent_id, status, worktree_path, started_at, step_plan, entity_id)
			VALUES (
				$1, $2, NULL,
				$3, $4, 'event', $5, $6,
				$7, $8, $9, now(), $10,
				(SELECT entity_id FROM tasks WHERE id = $4 AND org_id = $2)
			)
			ON CONFLICT (triggering_event_id, trigger_id) WHERE triggering_event_id IS NOT NULL DO NOTHING
		`, br.ID, orgID, br.BlueprintID, br.TaskID, br.TriggerID, br.TriggeringEventID, nullIfEmpty(br.ActorAgentID), br.Status, br.WorktreePath, stepPlan)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == blueprintRunsOneActivePerTaskConstraint {
				return db.ErrTaskBusyActiveAutoRun
			}
			return fmt.Errorf("insert blueprint_run (fenced): %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		inserted = n > 0
		// The fence closed: the original firing already stamped whatever claim
		// this event was going to make, so a replay must not re-stamp one the
		// user may have deliberately cleared since.
		if !inserted || claim.AgentID == "" {
			return nil
		}
		claimed, err = stampAgentClaimIfUnclaimed(ctx, q, orgID, br.TaskID, claim.AgentID, claim.ActingTeamID)
		return err
	})
	if err != nil {
		return false, false, err
	}
	return inserted, claimed, nil
}

func (s *blueprintStore) SetRunWorktreePathSystem(ctx context.Context, orgID, id, worktreePath string) error {
	_, err := s.admin.ExecContext(ctx,
		`UPDATE blueprint_runs SET worktree_path = $3 WHERE org_id = $1 AND id = $2`, orgID, id, worktreePath)
	return err
}

func (s *blueprintStore) ActiveRunForTaskSystem(ctx context.Context, orgID, taskID string) (*domain.BlueprintRun, error) {
	if !isValidUUID(taskID) {
		return nil, nil
	}
	var id string
	err := s.admin.QueryRowContext(ctx, `
		SELECT id FROM blueprint_runs
		WHERE org_id = $1 AND task_id = $2 AND status = 'running'
		ORDER BY started_at DESC LIMIT 1
	`, orgID, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return getBlueprintRun(ctx, s.admin, orgID, id)
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
	br, err := scanBlueprintRun(q.QueryRowContext(ctx, `
		SELECT id, blueprint_id, task_id, trigger_type, trigger_id, actor_agent_id::text, status,
		       current_step_index, cancel_requested, step_plan,
		       abort_reason, aborted_at_step, worktree_path, started_at, completed_at
		FROM blueprint_runs WHERE org_id = $1 AND id = $2
	`, orgID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &br, nil
}

// scanBlueprintRun reads one blueprint_runs row in the column order both the
// single read and ListRuns select. It takes the Scan func rather than a row so
// *sql.Row and *sql.Rows share it and the two reads cannot drift.
func scanBlueprintRun(scan func(...any) error) (domain.BlueprintRun, error) {
	var (
		br              domain.BlueprintRun
		triggerID       sql.NullString
		actorAgentID    sql.NullString
		abortReason     sql.NullString
		abortedAtStep   sql.NullInt64
		completedAt     sql.NullTime
		cancelRequested bool
		stepPlanRaw     string
	)
	if err := scan(&br.ID, &br.BlueprintID, &br.TaskID, &br.TriggerType, &triggerID, &actorAgentID, &br.Status,
		&br.CurrentStepIndex, &cancelRequested, &stepPlanRaw,
		&abortReason, &abortedAtStep, &br.WorktreePath, &br.StartedAt, &completedAt); err != nil {
		return br, err
	}
	var err error
	if br.StepPlan, err = domain.UnmarshalStepPlan(stepPlanRaw); err != nil {
		return br, fmt.Errorf("unmarshal step plan for blueprint_run %s: %w", br.ID, err)
	}
	br.CancelRequested = cancelRequested
	if triggerID.Valid {
		br.TriggerID = triggerID.String
	}
	if actorAgentID.Valid {
		br.ActorAgentID = actorAgentID.String
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
	return br, nil
}

func (s *blueprintStore) ListRuns(ctx context.Context, orgID string, f db.BlueprintRunListFilter, opts db.ListOpts) ([]domain.BlueprintRun, int, error) {
	args := []any{orgID}
	where := ` WHERE org_id = $1`
	if f.BlueprintID != "" {
		args = append(args, f.BlueprintID)
		where += fmt.Sprintf(" AND blueprint_id = $%d", len(args))
	}
	if len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where += fmt.Sprintf(" AND status = ANY($%d)", len(args))
	}

	var total int
	if err := s.app.QueryRowContext(ctx, `SELECT COUNT(*) FROM blueprint_runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `
		SELECT id, blueprint_id, task_id, trigger_type, trigger_id, actor_agent_id::text, status,
		       current_step_index, cancel_requested, step_plan,
		       abort_reason, aborted_at_step, worktree_path, started_at, completed_at
		FROM blueprint_runs` + where + ` ORDER BY started_at DESC, id`
	pageArgs := args
	if opts.Limit > 0 {
		q += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
		pageArgs = append(append([]any{}, args...), opts.Limit, opts.Offset)
	}
	rows, err := s.app.QueryContext(ctx, q, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.BlueprintRun{}
	for rows.Next() {
		br, err := scanBlueprintRun(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, br)
	}
	return out, total, rows.Err()
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

// readRunBlueprintPointer reads conversations.blueprint_run_id +
// blueprint_step_index for a single run.
func readRunBlueprintPointer(ctx context.Context, q queryer, orgID, runID string) (string, *int, error) {
	var (
		blueprintRunID sql.NullString
		stepIndex      sql.NullInt64
	)
	err := q.QueryRowContext(ctx,
		`SELECT blueprint_run_id, blueprint_step_index FROM conversations WHERE org_id = $1 AND id = $2`,
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
	return markBlueprintRunStatus(ctx, s.app, s.admin, orgID, id, status, abortReason, abortedAtStep)
}

func (s *blueprintStore) MarkRunStatusSystem(ctx context.Context, orgID, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
	return markBlueprintRunStatus(ctx, s.admin, nil, orgID, id, status, abortReason, abortedAtStep)
}

// adjacentClaims routes the parked children's claim release: nil means q
// holds claims-write privilege and the release rides the child-cancel
// statement itself; non-nil means q is the RLS app handle (SELECT-only on
// claims), so the release lands on that admin-backed queryer AFTER the tx
// work — the same adjacent, janitor-healed split every app-pool terminal
// write uses for its claim release.
func markBlueprintRunStatus(ctx context.Context, q, adjacentClaims queryer, orgID, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (bool, error) {
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
	// Flip the blueprint_run terminal AND cancel any still-active child run in
	// one transaction, so the two writes commit together — a terminal parent is
	// never committed alongside a live child. inTx composes with the caller's tx
	// when markBlueprintRunStatus runs inside a SyntheticClaims tx (manual/app
	// path) and opens a fresh one on the bare admin handle (event/system path).
	//
	// RLS scope: the admin pool bypasses RLS; on the app pool the child-cancel
	// runs under the caller's claims, but a blueprint's child steps share its
	// team, so conversations_update admits them — the cancel covers every
	// child the parent flip's actor owns. The narrow residual is a child the
	// caller somehow can't see under RLS: it stays non-terminal (not an error
	// — RLS filters the row, it doesn't fail), and the admin-pool boot
	// reconcile is the backstop that heals it. Without this whole guard a
	// cancel that raced the dispatcher's claim/setup window — or a parked
	// `open` step the sequence-cancel path skips — would strand a child
	// 'running', keeping the dispatcher on phantom work and pinning its
	// feature branch in a worktree, requeuing forever.
	var changed bool
	var parkedChildIDs []string
	err := inTx(ctx, q, func(tx queryer) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE blueprint_runs
			SET status = $1, abort_reason = $2, aborted_at_step = $3, completed_at = now()
			WHERE org_id = $4 AND id = $5
			  AND status = 'running'
		`, string(status), reasonArg, stepArg, orgID, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed = n > 0
		// Only on a real transition to a terminal target.
		if changed && status.Terminal() {
			if adjacentClaims == nil {
				return parkOrphanedChildRunsWithClaims(ctx, tx, orgID, id)
			}
			parkedChildIDs, err = parkOrphanedChildRuns(ctx, tx, orgID, id)
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if adjacentClaims != nil && len(parkedChildIDs) > 0 {
		// Adjacent, not atomic: when the app-side work is composed into a
		// caller's still-open tx this release can land against a cancel that
		// later rolls back (an in-flight child with no active claim), and a
		// crash can leave parked children with dangling claims — both are
		// the janitor-healed shapes the app-pool claim split already accepts.
		return changed, releaseParkedChildClaims(ctx, adjacentClaims, orgID, parkedChildIDs)
	}
	return changed, nil
}

// parkOrphanedChildRunsWithClaims parks every still-mid-flight child run of
// blueprintRunID `open` and releases those children's active claims in the
// same statement — q must hold claims-write privilege. Called by
// markBlueprintRunStatus's atomic flip; RunQueueStore.ReconcileOrphanedRuns
// applies the same predicate in its own boot sweep (it can't share this body —
// different store, different scope).
//
// A park, not a terminal: the child neither failed nor concluded, it was
// stopped when its parent ended. Read the `open` as "stopped without
// concluding", NOT as "resumable" — the parent is terminal, so the claim gate
// refuses it. Scoped to status IS NULL: a child that already parked itself
// keeps its own parked_at and park_reason, and one that already reached a
// terminal is left alone.
func parkOrphanedChildRunsWithClaims(ctx context.Context, q queryer, orgID, blueprintRunID string) error {
	_, err := q.ExecContext(ctx, `
		WITH parked AS (
			UPDATE conversations
			SET status = 'open',
			    parked_at = COALESCE(parked_at, now()),
			    park_reason = COALESCE(park_reason, 'blueprint_terminal'),
			    result_summary = COALESCE(NULLIF(result_summary, ''), $3)
			WHERE org_id = $1 AND blueprint_run_id = $2 AND status IS NULL
			RETURNING id
		)
		UPDATE claims SET released_at = now(), outcome = 'cancelled'
		FROM parked
		WHERE claims.conversation_id = parked.id AND claims.released_at IS NULL
	`, orgID, blueprintRunID, "Stopped: owning blueprint run reached a terminal state")
	return err
}

// parkOrphanedChildRuns is the app-pool (claims-SELECT-only) variant of the
// same child park: it returns the parked ids so the caller can release their
// claims on an admin-backed queryer.
func parkOrphanedChildRuns(ctx context.Context, q queryer, orgID, blueprintRunID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		UPDATE conversations
		SET status = 'open',
		    parked_at = COALESCE(parked_at, now()),
		    park_reason = COALESCE(park_reason, 'blueprint_terminal'),
		    result_summary = COALESCE(NULLIF(result_summary, ''), $3)
		WHERE org_id = $1 AND blueprint_run_id = $2 AND status IS NULL
		RETURNING id
	`, orgID, blueprintRunID, "Stopped: owning blueprint run reached a terminal state")
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

func releaseParkedChildClaims(ctx context.Context, q queryer, orgID string, conversationIDs []string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE claims SET released_at = now(), outcome = 'cancelled'
		WHERE org_id = $1 AND conversation_id = ANY($2) AND released_at IS NULL
	`, orgID, conversationIDs)
	return err
}

func (s *blueprintStore) ReopenRunForResume(ctx context.Context, orgID, id string) (bool, error) {
	if !isValidUUID(id) {
		return false, nil
	}
	// CAS aborted → running, clearing the stale abort metadata. The resumed step
	// re-finalizes the blueprint through the normal post-resume disposition.
	// App pool — runs inside the resume's synthetic-claims tx alongside the run
	// flip, so the two commit atomically.
	res, err := s.app.ExecContext(ctx, `
		UPDATE blueprint_runs
		SET status = 'running', abort_reason = NULL, aborted_at_step = NULL, completed_at = NULL
		WHERE org_id = $1 AND id = $2 AND status = 'aborted'
	`, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *blueprintStore) SetRunCurrentStepSystem(ctx context.Context, orgID, id string, stepIndex int) error {
	if !isValidUUID(id) {
		return nil
	}
	_, err := s.admin.ExecContext(ctx,
		`UPDATE blueprint_runs SET current_step_index = $1 WHERE org_id = $2 AND id = $3`,
		stepIndex, orgID, id)
	return err
}

func (s *blueprintStore) RequestRunCancelSystem(ctx context.Context, orgID, id string) (bool, error) {
	if !isValidUUID(id) {
		return false, nil
	}
	res, err := s.admin.ExecContext(ctx, `
		UPDATE blueprint_runs SET cancel_requested = true
		WHERE org_id = $1 AND id = $2 AND status = 'running' AND cancel_requested = false
	`, orgID, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *blueprintStore) RunsForBlueprint(ctx context.Context, orgID, blueprintRunID string) ([]domain.Conversation, error) {
	return runsForBlueprint(ctx, s.app, orgID, blueprintRunID)
}

func (s *blueprintStore) RunsForBlueprintSystem(ctx context.Context, orgID, blueprintRunID string) ([]domain.Conversation, error) {
	return runsForBlueprint(ctx, s.admin, orgID, blueprintRunID)
}

func (s *blueprintStore) ActiveStepRunIDs(ctx context.Context, orgID, blueprintRunID string) ([]string, error) {
	return blueprintActiveStepRunIDs(ctx, s.app, orgID, blueprintRunID)
}

func (s *blueprintStore) ActiveStepRunIDsSystem(ctx context.Context, orgID, blueprintRunID string) ([]string, error) {
	return blueprintActiveStepRunIDs(ctx, s.admin, orgID, blueprintRunID)
}

func (s *blueprintStore) StepPlanLengths(ctx context.Context, orgID string, blueprintRunIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(blueprintRunIDs))
	if len(blueprintRunIDs) == 0 {
		return out, nil
	}
	// App pool (RLS-active), matching the handler that asks: manual blueprint
	// runs are creator-scoped, so another user's manual run is absent from the
	// result rather than counted. The caller degrades to the unqualified
	// projection there, exactly as the chain rail already does for a run whose
	// blueprint row it cannot read.
	rows, err := s.app.QueryContext(ctx, `
		SELECT id::text, json_array_length(step_plan::json)
		FROM blueprint_runs
		WHERE org_id = $1 AND id = ANY($2)
	`, orgID, pgUUIDArray(blueprintRunIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func blueprintActiveStepRunIDs(ctx context.Context, q queryer, orgID, blueprintRunID string) ([]string, error) {
	if !isValidUUID(blueprintRunID) {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `
		SELECT id FROM conversations
		WHERE org_id = $1 AND blueprint_run_id = $2
		  AND (status IS NULL
		       OR status NOT IN (`+runTerminalStatusesSQL+`,'open'))
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

func runsForBlueprint(ctx context.Context, q queryer, orgID, blueprintRunID string) ([]domain.Conversation, error) {
	if !isValidUUID(blueprintRunID) {
		return nil, nil
	}
	// Status is the derived display ladder — the same contract as every
	// other ConversationStore projection.
	rows, err := q.QueryContext(ctx, `
		SELECT r.id, r.task_id, r.prompt_id,
		       `+pgDisplayStatusSQL+`,
		       r.model, r.started_at, r.completed_at,
		       (SELECT SUM(m.cost_usd) FROM messages m WHERE m.conversation_id = r.id AND m.org_id = r.org_id),
		       (SELECT SUM(cl.duration_ms)::bigint FROM claims cl WHERE cl.conversation_id = r.id),
		       (SELECT SUM(cl.num_turns)::bigint FROM claims cl WHERE cl.conversation_id = r.id),
		       r.park_reason, r.worktree_path,
		       r.result_summary, r.sdk_session_id, r.outcome, r.outcome_reason,
		       r.blueprint_run_id, r.blueprint_step_index
		FROM conversations r
		WHERE r.org_id = $1 AND r.blueprint_run_id = $2
		ORDER BY r.blueprint_step_index ASC, r.started_at ASC
	`, orgID, blueprintRunID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.Conversation
	for rows.Next() {
		var (
			r             domain.Conversation
			completedAt   sql.NullTime
			costUSD       sql.NullFloat64
			durationMs    sql.NullInt64
			numTurns      sql.NullInt64
			stepIdx       sql.NullInt64
			promptID      sql.NullString
			model         sql.NullString
			parkReason    sql.NullString
			worktreeP     sql.NullString
			resultSum     sql.NullString
			sessionID     sql.NullString
			outcome       sql.NullString
			outcomeReason sql.NullString
			blueprintRun  sql.NullString
		)
		if err := rows.Scan(&r.ID, &r.TaskID, &promptID, &r.Status, &model, &r.StartedAt, &completedAt,
			&costUSD, &durationMs, &numTurns, &parkReason, &worktreeP, &resultSum, &sessionID,
			&outcome, &outcomeReason, &blueprintRun, &stepIdx); err != nil {
			return nil, err
		}
		r.PromptID = promptID.String
		r.Model = model.String
		r.ParkReason = domain.ParkReason(parkReason.String)
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
