package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// blueprintStore is the SQLite impl of db.BlueprintStore.
//
// SQLite has one connection, so the (app, admin) pool split the Postgres
// impl keeps distinct collapses to a single queryer here; the `...System`
// admin-pool variants are thin wrappers around their non-System counterparts.
type blueprintStore struct {
	q queryer
}

func newBlueprintStore(q queryer) db.BlueprintStore {
	return &blueprintStore{q: q}
}

var _ db.BlueprintStore = (*blueprintStore)(nil)

// --- Blueprint header CRUD -----------------------------------------------

// List ignores teamID: local mode is single-team.
func (s *blueprintStore) List(ctx context.Context, orgID string, f db.BlueprintListFilter, opts db.ListOpts) ([]domain.Blueprint, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	// f.TeamID is ignored — local mode is single-team.
	where := ` WHERE hidden = 0 AND deleted_at IS NULL`
	args := []any{}
	if len(f.GatedEventTypes) > 0 {
		// Same predicate as the Postgres impl, spelled with an IN list since
		// SQLite has no array bind: hide a blueprint iff it HAS triggers and
		// none of them fires on an ungated type.
		placeholders := make([]string, len(f.GatedEventTypes))
		for i, et := range f.GatedEventTypes {
			placeholders[i] = "?"
			args = append(args, et)
		}
		where += `
		  AND NOT (
		    EXISTS (SELECT 1 FROM event_handlers h
		             WHERE h.blueprint_id = blueprints.id AND h.kind = 'trigger')
		    AND NOT EXISTS (SELECT 1 FROM event_handlers h
		                     WHERE h.blueprint_id = blueprints.id AND h.kind = 'trigger'
		                       AND h.event_type NOT IN (` + strings.Join(placeholders, ",") + `))
		  )`
	}

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM blueprints`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.Blueprint{}, total, nil
	}

	query := `
		SELECT id, name, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at
		FROM blueprints` + where + ` ORDER BY updated_at DESC, id`
	pageArgs := args
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		pageArgs = append(append([]any{}, args...), opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []domain.Blueprint
	for rows.Next() {
		b, err := scanBlueprintRowSQLite(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, b)
	}
	return out, total, rows.Err()
}

// Get is request-facing: it filters deleted_at IS NULL. GetSystem omits the
// filter so a soft-deleted blueprint still resolves for in-flight runs and
// past-run timelines.
// sqliteBlueprintColumns is the canonical projection of a blueprints row, in
// the order scanBlueprintRowSQLite reads them. Every point read SELECTs it and
// every single-row write RETURNs it, so the write shape cannot drift from the
// read shape as columns are added.
const sqliteBlueprintColumns = `id, name, source, usage_count, user_modified, team_id, system_slug, created_at, updated_at`

// scanUpdatedBlueprintSQLite decodes an id-keyed UPDATE … RETURNING on
// blueprints. No row scanned means the id named no live blueprint, which is
// db.ErrNoSuchBlueprint.
func scanUpdatedBlueprintSQLite(row *sql.Row) (domain.Blueprint, error) {
	b, err := scanBlueprintRowSQLite(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Blueprint{}, db.ErrNoSuchBlueprint
	}
	return b, err
}

func (s *blueprintStore) Get(ctx context.Context, orgID string, id string) (*domain.Blueprint, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	b, err := scanBlueprintRowSQLite(s.q.QueryRowContext(ctx, `
		SELECT `+sqliteBlueprintColumns+`
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
		SELECT `+sqliteBlueprintColumns+`
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
		SELECT ` + sqliteBlueprintColumns + `
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

func (s *blueprintStore) Create(ctx context.Context, orgID, teamID string, b domain.Blueprint) (domain.Blueprint, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Blueprint{}, err
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
	return scanBlueprintRowSQLite(s.q.QueryRowContext(ctx, `
		INSERT INTO blueprints (id, name, source, usage_count, team_id, creator_user_id, system_slug, created_at, updated_at)
		VALUES (?, ?, ?, 0, ?, ?, ?, ?, ?)
		RETURNING `+sqliteBlueprintColumns,
		b.ID, b.Name, b.Source, runmode.LocalDefaultTeamID, creatorUserID, systemSlug, now, now,
	).Scan)
}

// Rename stamps user_modified=1 alongside the name change — the sync signal
// (see db.BlueprintStore's stamping contract) that this team's copy diverged
// from shipped content.
func (s *blueprintStore) Rename(ctx context.Context, orgID, id, name string) (domain.Blueprint, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Blueprint{}, err
	}
	return scanUpdatedBlueprintSQLite(s.q.QueryRowContext(ctx, `
		UPDATE blueprints SET name = ?, updated_at = ?, user_modified = 1
		WHERE org_id = ? AND id = ? AND deleted_at IS NULL
		RETURNING `+sqliteBlueprintColumns,
		name, time.Now().UTC(), orgID, id))
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

// ListAllSteps returns every step of the local team's request-facing blueprints
// in one read (the canvas's bulk steps fetch). The blueprints join mirrors
// List's gate (hidden = 0, deleted_at IS NULL); teamID is ignored in single-team
// local mode.
func (s *blueprintStore) ListAllSteps(ctx context.Context, orgID string, f db.BlueprintStepListFilter, opts db.ListOpts) ([]domain.BlueprintStep, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	// f.TeamID is ignored — local mode is single-team.
	where := `
		FROM blueprint_steps bs
		JOIN blueprints b ON b.id = bs.blueprint_id
		WHERE b.hidden = 0 AND b.deleted_at IS NULL`
	args := []any{}
	if len(f.BlueprintIDs) > 0 {
		placeholders := make([]string, len(f.BlueprintIDs))
		for i, id := range f.BlueprintIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where += ` AND bs.blueprint_id IN (` + strings.Join(placeholders, ",") + `)`
	}

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*)`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count blueprint steps: %w", err)
	}
	if opts.CountOnly {
		return []domain.BlueprintStep{}, total, nil
	}

	query := `
		SELECT bs.blueprint_id, bs.step_index, bs.step_prompt_id, bs.brief, bs.created_at` +
		where + ` ORDER BY bs.blueprint_id, bs.step_index`
	pageArgs := args
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		pageArgs = append(append([]any{}, args...), opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, pageArgs...)
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

func (s *blueprintStore) ReplaceSteps(ctx context.Context, orgID, blueprintID string, stepPromptIDs, briefs []string) (domain.Blueprint, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.Blueprint{}, err
	}
	if len(briefs) != 0 && len(briefs) != len(stepPromptIDs) {
		return domain.Blueprint{}, fmt.Errorf("briefs length %d must match stepPromptIDs length %d", len(briefs), len(stepPromptIDs))
	}
	var stamped domain.Blueprint
	if err := inTx(ctx, s.q, func(q queryer) error {
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
		// Stamp user_modified — the step list is part of the sync unit's
		// content (see db.BlueprintStore's stamping contract). The steps are a
		// set with no row to name; this stamp is the single-row write, and its
		// RETURNING is what the caller renders after the replace.
		b, err := scanUpdatedBlueprintSQLite(q.QueryRowContext(ctx,
			`UPDATE blueprints SET updated_at = ?, user_modified = 1 WHERE id = ?
			 RETURNING `+sqliteBlueprintColumns, now, blueprintID))
		if err != nil {
			return err
		}
		stamped = b
		return nil
	}); err != nil {
		return domain.Blueprint{}, err
	}
	return stamped, nil
}

// reparentBlueprintSteps moves the steps of fromBlueprint whose step_index is
// >= minIndex onto toBlueprint, shifting each step_index by indexShift. It is
// the shared mechanic behind MergeInto (append source onto host) and SplitAt
// (peel the tail onto a new blueprint). The target step_index values must not
// collide with any existing rows on toBlueprint — MergeInto guarantees this by
// shifting source steps past the host's length; SplitAt's target is a freshly
// created empty blueprint. step_prompt_id is unchanged, so the copy-only step
// uniqueness holds throughout (a prompt just moves to a different blueprint).
func reparentBlueprintSteps(ctx context.Context, q queryer, fromBlueprint, toBlueprint string, indexShift, minIndex int) error {
	_, err := q.ExecContext(ctx, `
		UPDATE blueprint_steps
		SET blueprint_id = ?, step_index = step_index + ?
		WHERE blueprint_id = ? AND step_index >= ?
	`, toBlueprint, indexShift, fromBlueprint, minIndex)
	return err
}

func (s *blueprintStore) MergeInto(ctx context.Context, orgID, hostID, sourceID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(q queryer) error {
		var hostLen int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM blueprint_steps WHERE blueprint_id = ?`, hostID,
		).Scan(&hostLen); err != nil {
			return fmt.Errorf("count host steps: %w", err)
		}
		// Append source's steps after the host's, densely (host_len + i).
		if err := reparentBlueprintSteps(ctx, q, sourceID, hostID, hostLen, 0); err != nil {
			return fmt.Errorf("reparent source steps: %w", err)
		}
		now := time.Now().UTC()
		// Soft-delete the now-empty source (mirrors Delete; inlined so it shares
		// this tx whether s.q is a *sql.Tx or a fresh inTx-opened one).
		res, err := q.ExecContext(ctx,
			`UPDATE blueprints SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, now, sourceID)
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
		if _, err := q.ExecContext(ctx,
			`UPDATE blueprints SET updated_at = ?, user_modified = 1 WHERE id = ?`, now, hostID); err != nil {
			return fmt.Errorf("bump host updated_at: %w", err)
		}
		return nil
	})
}

// insertTriggerlessBlueprint creates a fresh, trigger-less user blueprint
// (same sentinel team as everything in local mode). Shared by SplitAt and
// DeleteStep, which both peel steps onto a brand-new downstream blueprint.
// Born with user_modified=1: a split/delete product is never a shipped row
// (system_slug stays NULL, omitted below), so it starts already diverged per
// db.BlueprintStore's stamping contract.
func insertTriggerlessBlueprint(ctx context.Context, q queryer, id, name string, now time.Time) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO blueprints (id, name, source, usage_count, user_modified, team_id, creator_user_id, created_at, updated_at)
		VALUES (?, ?, 'user', 0, 1, ?, ?, ?, ?)
	`, id, name, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, now, now)
	return err
}

// softDeleteBlueprintTx stamps deleted_at (+ user_modified, since retiring a
// blueprint via DeleteStep's head case is itself a structural edit — its only
// caller) inside an open tx. Errors when the row is missing or already
// deleted, so a caller that thinks it is retiring a live blueprint finds out.
func softDeleteBlueprintTx(ctx context.Context, q queryer, id string, now time.Time) error {
	res, err := q.ExecContext(ctx,
		`UPDATE blueprints SET deleted_at = ?, user_modified = 1 WHERE id = ? AND deleted_at IS NULL`, now, id)
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

func (s *blueprintStore) SplitAt(ctx context.Context, orgID, id string, atIndex int, newBlueprintID, newName string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	err := inTx(ctx, s.q, func(q queryer) error {
		now := time.Now().UTC()
		// Create the new trigger-less downstream blueprint.
		if err := insertTriggerlessBlueprint(ctx, q, newBlueprintID, newName, now); err != nil {
			return fmt.Errorf("create downstream blueprint: %w", err)
		}
		// Peel steps [atIndex, N) onto the new blueprint, re-densified 0-based.
		if err := reparentBlueprintSteps(ctx, q, id, newBlueprintID, -atIndex, atIndex); err != nil {
			return fmt.Errorf("reparent tail steps: %w", err)
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE blueprints SET updated_at = ?, user_modified = 1 WHERE id = ?`, now, id); err != nil {
			return fmt.Errorf("bump upstream updated_at: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newBlueprintID, nil
}

// insertIsolationBlueprintTx creates the born-soft-deleted 1-step blueprint
// that holds the deleted step's row, named after that step's prompt (the name
// is never surfaced — the wrapper is soft-deleted on creation — but blueprints
// require a non-empty name). srcBlueprintID/stepIndex address the step the
// caller is about to reparent onto it.
func insertIsolationBlueprintTx(ctx context.Context, q queryer, id, srcBlueprintID string, stepIndex int, now time.Time) error {
	res, err := q.ExecContext(ctx, `
		INSERT INTO blueprints (id, name, source, usage_count, team_id, creator_user_id, created_at, updated_at, deleted_at)
		SELECT ?, p.name, 'user', 0, ?, ?, ?, ?, ?
		FROM blueprint_steps bs JOIN prompts p ON p.id = bs.step_prompt_id
		WHERE bs.blueprint_id = ? AND bs.step_index = ?
	`, id, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, now, now, now, srcBlueprintID, stepIndex)
	if err != nil {
		return err
	}
	// The SELECT resolves the deleted step's prompt; a 0-row insert means that
	// step is missing. The caller validates it exists, so this is a guard
	// against a cryptic FK error on the follow-up reparent rather than an
	// expected path.
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("isolation blueprint: step %d of %s not found", stepIndex, srcBlueprintID)
	}
	return nil
}

func (s *blueprintStore) DeleteStep(ctx context.Context, orgID, blueprintID string, stepIndex int, newName string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	var downstreamID string
	err := inTx(ctx, s.q, func(q queryer) error {
		var n int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM blueprint_steps WHERE blueprint_id = ?`, blueprintID,
		).Scan(&n); err != nil {
			return fmt.Errorf("count steps: %w", err)
		}
		if n < 2 {
			return fmt.Errorf("DeleteStep requires a multi-step blueprint, got %d step(s)", n)
		}
		if stepIndex < 0 || stepIndex >= n {
			return fmt.Errorf("step index %d out of range [0,%d)", stepIndex, n)
		}
		now := time.Now().UTC()
		switch stepIndex {
		case 0:
			// Head: peel [1,N) onto a fresh trigger-less downstream blueprint,
			// leaving the original holding only the deleted entry step; soft-delete
			// it (the caller detaches its trigger separately).
			downstreamID = uuid.New().String()
			if err := insertTriggerlessBlueprint(ctx, q, downstreamID, newName, now); err != nil {
				return fmt.Errorf("create downstream blueprint: %w", err)
			}
			if err := reparentBlueprintSteps(ctx, q, blueprintID, downstreamID, -1, 1); err != nil {
				return fmt.Errorf("reparent downstream steps: %w", err)
			}
			if err := softDeleteBlueprintTx(ctx, q, blueprintID, now); err != nil {
				return err
			}
		case n - 1:
			// Tail: isolate the last step onto a fresh soft-deleted 1-step
			// blueprint; the original keeps [0,N-1) with its trigger + id.
			isolationID := uuid.New().String()
			if err := insertIsolationBlueprintTx(ctx, q, isolationID, blueprintID, stepIndex, now); err != nil {
				return fmt.Errorf("create isolation blueprint: %w", err)
			}
			if err := reparentBlueprintSteps(ctx, q, blueprintID, isolationID, -stepIndex, stepIndex); err != nil {
				return fmt.Errorf("isolate deleted step: %w", err)
			}
			if _, err := q.ExecContext(ctx,
				`UPDATE blueprints SET updated_at = ?, user_modified = 1 WHERE id = ?`, now, blueprintID); err != nil {
				return fmt.Errorf("bump upstream updated_at: %w", err)
			}
		default:
			// Mid: the original keeps [0,stepIndex) with its trigger + id; peel
			// [stepIndex+1,N) onto a fresh trigger-less downstream blueprint, then
			// isolate the deleted step onto a fresh soft-deleted 1-step blueprint.
			downstreamID = uuid.New().String()
			if err := insertTriggerlessBlueprint(ctx, q, downstreamID, newName, now); err != nil {
				return fmt.Errorf("create downstream blueprint: %w", err)
			}
			if err := reparentBlueprintSteps(ctx, q, blueprintID, downstreamID, -(stepIndex + 1), stepIndex+1); err != nil {
				return fmt.Errorf("reparent downstream steps: %w", err)
			}
			isolationID := uuid.New().String()
			if err := insertIsolationBlueprintTx(ctx, q, isolationID, blueprintID, stepIndex, now); err != nil {
				return fmt.Errorf("create isolation blueprint: %w", err)
			}
			if err := reparentBlueprintSteps(ctx, q, blueprintID, isolationID, -stepIndex, stepIndex); err != nil {
				return fmt.Errorf("isolate deleted step: %w", err)
			}
			if _, err := q.ExecContext(ctx,
				`UPDATE blueprints SET updated_at = ?, user_modified = 1 WHERE id = ?`, now, blueprintID); err != nil {
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
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	ids := db.DedupPreserveOrder(promptIDs)
	if len(ids) == 0 {
		return nil, db.ErrDuplicateNoPrompts
	}
	var newIDs []string
	err := inTx(ctx, s.q, func(q queryer) error {
		// Resolve each prompt id → its step + prompt payload. Copy-only means a
		// live prompt is a step of at most one non-deleted blueprint, so a single
		// row resolves it.
		resolved := make([]db.DuplicationStep, 0, len(ids))
		for _, pid := range ids {
			var (
				st   db.DuplicationStep
				team string
			)
			err := q.QueryRowContext(ctx, `
				SELECT bs.blueprint_id, b.name, b.team_id, bs.step_index, bs.brief,
				       p.name, p.body, p.model, p.allowed_tools,
				       (SELECT COUNT(*) FROM blueprint_steps bs2 WHERE bs2.blueprint_id = bs.blueprint_id)
				FROM blueprint_steps bs
				JOIN blueprints b ON b.id = bs.blueprint_id
				JOIN prompts p ON p.id = bs.step_prompt_id
				WHERE bs.step_prompt_id = ? AND b.deleted_at IS NULL AND p.deleted_at IS NULL
			`, pid).Scan(&st.BlueprintID, &st.BlueprintName, &team, &st.StepIndex, &st.Brief,
				&st.PromptName, &st.PromptBody, &st.PromptModel, &st.PromptTools, &st.BlueprintTotal)
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrDuplicatePromptNotFound
			}
			if err != nil {
				return fmt.Errorf("resolve prompt %s: %w", pid, err)
			}
			if teamID != "" && team != teamID {
				return db.ErrDuplicateCrossTeam
			}
			resolved = append(resolved, st)
		}

		now := time.Now().UTC()
		for _, run := range db.PartitionDuplicationRuns(resolved) {
			bpID := uuid.New().String()
			// New blueprint: trigger-less, user source, local sentinel team.
			if _, err := q.ExecContext(ctx, `
				INSERT INTO blueprints (id, name, source, usage_count, team_id, creator_user_id, created_at, updated_at)
				VALUES (?, ?, 'user', 0, ?, ?, ?, ?)
			`, bpID, run.Name, runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, now, now); err != nil {
				return fmt.Errorf("create duplicated blueprint: %w", err)
			}
			for i, step := range run.Steps {
				// Deep-copy the prompt into a fresh, independent user row
				// (system_slug NULL — a copy of a system/imported prompt decouples).
				newPromptID := uuid.New().String()
				if _, err := q.ExecContext(ctx, `
					INSERT INTO prompts (id, name, body, source, allowed_tools, model, usage_count, team_id, creator_user_id, created_at, updated_at)
					VALUES (?, ?, ?, 'user', ?, ?, 0, ?, ?, ?, ?)
				`, newPromptID, step.PromptName, step.PromptBody, step.PromptTools, step.PromptModel,
					runmode.LocalDefaultTeamID, runmode.LocalDefaultUserID, now, now); err != nil {
					return fmt.Errorf("copy prompt: %w", err)
				}
				if _, err := q.ExecContext(ctx, `
					INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id, brief, created_at)
					VALUES (?, ?, ?, ?, ?)
				`, bpID, i, newPromptID, step.Brief, now); err != nil {
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

func (s *blueprintStore) CreateRun(ctx context.Context, orgID string, br domain.BlueprintRun) (domain.BlueprintRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.BlueprintRun{}, err
	}
	if br.ID == "" {
		br.ID = uuid.New().String()
	}
	if br.Status == "" {
		br.Status = domain.BlueprintRunStatusRunning
	}
	if br.TriggerType == "" {
		return domain.BlueprintRun{}, errors.New("blueprint run trigger type required")
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
	stepPlan, err := domain.MarshalStepPlan(br.StepPlan)
	if err != nil {
		return domain.BlueprintRun{}, fmt.Errorf("marshal step plan: %w", err)
	}
	// creator_user_id is paired with trigger_type by the
	// blueprint_runs_creator_matches_trigger_type CHECK: NULL for event-fired
	// runs (no human author), the sentinel local user for manual runs. Mirrors
	// the conversations insert and the Postgres createRunManual/createRunEventTriggered
	// split (which are one method here — SQLite is N=1, single connection).
	var creatorUserID any
	if br.TriggerType != domain.BlueprintTriggerEvent {
		creatorUserID = runmode.LocalDefaultUserID
	}
	// RETURNING carries the started_at the statement stamped and the id this
	// method minted when br had none.
	stored, err := scanBlueprintRunSQLite(s.q.QueryRowContext(ctx, `
		INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, trigger_id, triggering_event_id, actor_agent_id, status, step_plan, worktree_path, abort_reason, completed_at, creator_user_id, started_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		RETURNING `+blueprintRunColumns,
		br.ID, br.BlueprintID, br.TaskID, br.TriggerType, triggerID, nullIfEmpty(br.TriggeringEventID), nullIfEmpty(br.ActorAgentID), br.Status, stepPlan, br.WorktreePath, abortReason, completedAt, creatorUserID).Scan)
	if err != nil {
		return domain.BlueprintRun{}, fmt.Errorf("insert blueprint_run: %w", err)
	}
	return stored, nil
}

// CreateRunIfNotFiredSystem is the event-path fenced insert: ON CONFLICT against
// the blueprint_runs_event_trigger_fence partial unique index makes a replayed
// (triggering_event_id, trigger_id) a clean no-op (inserted=false). SQLite has a
// single connection, so there is no admin/app split; the contract matches the
// Postgres impl.
//
// The blueprint run row and the task's agent claim commit together — see
// db.AgentClaimStamp for why they are inseparable. A stamp refusal is not an
// error and leaves the blueprint run committed.
func (s *blueprintStore) CreateRunIfNotFiredSystem(ctx context.Context, orgID string, br domain.BlueprintRun, claim db.AgentClaimStamp) (bool, bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, false, err
	}
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
	err = inTx(ctx, s.q, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			INSERT INTO blueprint_runs (id, blueprint_id, task_id, trigger_type, trigger_id, triggering_event_id, actor_agent_id, status, step_plan, worktree_path, creator_user_id, started_at)
			VALUES (?, ?, ?, 'event', ?, ?, ?, ?, ?, ?, NULL, CURRENT_TIMESTAMP)
			ON CONFLICT (triggering_event_id, trigger_id) WHERE triggering_event_id IS NOT NULL DO NOTHING
		`, br.ID, br.BlueprintID, br.TaskID, br.TriggerID, br.TriggeringEventID, nullIfEmpty(br.ActorAgentID), br.Status, stepPlan, br.WorktreePath)
		if err != nil {
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
		claimed, err = stampAgentClaimIfUnclaimed(ctx, q, br.TaskID, claim.AgentID, claim.ActingTeamID)
		return err
	})
	if err != nil {
		return false, false, err
	}
	return inserted, claimed, nil
}

func (s *blueprintStore) SetRunWorktreePathSystem(ctx context.Context, orgID, id, worktreePath string) (domain.BlueprintRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.BlueprintRun{}, err
	}
	return scanUpdatedBlueprintRunSQLite(s.q.QueryRowContext(ctx,
		`UPDATE blueprint_runs SET worktree_path = ? WHERE id = ? RETURNING `+blueprintRunColumns,
		worktreePath, id))
}

func (s *blueprintStore) ActiveRunForTaskSystem(ctx context.Context, orgID, taskID string) (*domain.BlueprintRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	var id string
	err := s.q.QueryRowContext(ctx, `
		SELECT id FROM blueprint_runs
		WHERE task_id = ? AND status = 'running'
		ORDER BY started_at DESC LIMIT 1
	`, taskID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return s.GetRun(ctx, orgID, id)
}

func (s *blueprintStore) GetRun(ctx context.Context, orgID, id string) (*domain.BlueprintRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	br, err := scanBlueprintRunSQLite(s.q.QueryRowContext(ctx, blueprintRunSelect+` WHERE id = ?`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &br, nil
}

// blueprintRunColumns is the canonical projection of a blueprint_runs row, in
// the order scanBlueprintRunSQLite reads them. Both reads SELECT it and both
// single-row run writes RETURN it, so the write shape cannot drift from the
// read shape.
const blueprintRunColumns = `id, blueprint_id, task_id, trigger_type, trigger_id, actor_agent_id, status,
		       current_step_index, cancel_requested, step_plan,
		       abort_reason, aborted_at_step, worktree_path, started_at, completed_at`

// blueprintRunSelect is the projection GetRun and ListRuns share, so the two
// reads of one row cannot drift on column order.
const blueprintRunSelect = `
		SELECT ` + blueprintRunColumns + `
		FROM blueprint_runs`

// scanBlueprintRunSQLite reads one blueprint_runs row in blueprintRunColumns
// order. It takes the Scan func rather than a row so *sql.Row and *sql.Rows
// share it and the reads and the writes' RETURNING cannot drift.
func scanBlueprintRunSQLite(scan func(...any) error) (domain.BlueprintRun, error) {
	var (
		br              domain.BlueprintRun
		triggerID       sql.NullString
		actorAgentID    sql.NullString
		abortReason     sql.NullString
		abortedAtStep   sql.NullInt64
		completedAt     sql.NullTime
		cancelRequested int
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
	br.CancelRequested = cancelRequested != 0
	br.TriggerID = triggerID.String
	br.ActorAgentID = actorAgentID.String
	br.AbortReason = abortReason.String
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

// scanUpdatedBlueprintRunSQLite decodes an id-keyed UPDATE … RETURNING on
// blueprint_runs. No row scanned means the id named no run, which is
// db.ErrNoSuchBlueprintRun.
func scanUpdatedBlueprintRunSQLite(row *sql.Row) (domain.BlueprintRun, error) {
	br, err := scanBlueprintRunSQLite(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.BlueprintRun{}, db.ErrNoSuchBlueprintRun
	}
	return br, err
}

func (s *blueprintStore) ListRuns(ctx context.Context, orgID string, f db.BlueprintRunListFilter, opts db.ListOpts) ([]domain.BlueprintRun, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	where := ` WHERE 1 = 1`
	args := []any{}
	if f.BlueprintID != "" {
		where += ` AND blueprint_id = ?`
		args = append(args, f.BlueprintID)
	}
	if len(f.Statuses) > 0 {
		placeholders := make([]string, len(f.Statuses))
		for i, st := range f.Statuses {
			placeholders[i] = "?"
			args = append(args, st)
		}
		where += ` AND status IN (` + strings.Join(placeholders, ",") + `)`
	}

	var total int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM blueprint_runs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.BlueprintRun{}, total, nil
	}

	query := blueprintRunSelect + where + ` ORDER BY started_at DESC, id`
	pageArgs := args
	if opts.Limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		pageArgs = append(append([]any{}, args...), opts.Limit, opts.Offset)
	}
	rows, err := s.q.QueryContext(ctx, query, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.BlueprintRun{}
	for rows.Next() {
		br, err := scanBlueprintRunSQLite(rows.Scan)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, br)
	}
	return out, total, rows.Err()
}

func (s *blueprintStore) GetRunForConversation(ctx context.Context, orgID, stepConversationID string) (*domain.BlueprintRun, *int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, nil, err
	}
	var (
		blueprintRunID sql.NullString
		stepIndex      sql.NullInt64
	)
	err := s.q.QueryRowContext(ctx, `SELECT blueprint_run_id, blueprint_step_index FROM conversations WHERE id = ?`, stepConversationID).
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

func (s *blueprintStore) GetRunForConversationSystem(ctx context.Context, orgID, stepConversationID string) (*domain.BlueprintRun, *int, error) {
	return s.GetRunForConversation(ctx, orgID, stepConversationID)
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
	now := time.Now().UTC()
	// Flip the blueprint_run terminal AND cancel any still-active child
	// conversation in one transaction, so a terminal parent can never be
	// observed (or committed) alongside a live child. inTx composes with the
	// caller's tx when MarkRunStatus runs inside SyntheticClaimsWithTx (manual
	// path) and opens a fresh one on the bare admin/system handle — either way
	// the two writes are all-or-nothing. Without a non-terminal child a cancel
	// that raced the dispatcher's claim/setup window (or a parked `open` step
	// the sequence-cancel path skips) would strand a child 'running', keeping
	// the dispatcher on phantom work and pinning its feature branch in a
	// worktree, requeuing forever.
	var changed bool
	err := inTx(ctx, s.q, func(q queryer) error {
		res, err := q.ExecContext(ctx, `
			UPDATE blueprint_runs
			SET status = ?, abort_reason = ?, aborted_at_step = ?, completed_at = ?
			WHERE id = ? AND status = 'running'
		`, string(status), reasonArg, stepArg, now, id)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		changed = n > 0
		// Only on a real transition (changed) to a terminal target; a no-op flip
		// or a non-terminal target leaves children alone. In the common terminate
		// paths the triggering step is already terminal, so this is a guard that
		// fires only on the race.
		if changed && status.Terminal() {
			return parkOrphanedChildConversations(ctx, q, id)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// parkOrphanedChildConversations parks every still-mid-flight child
// conversation of blueprintRunID `open` and releases those children's active
// claims. Called by MarkRunStatus's atomic flip;
// ConversationQueueStore.ReconcileOrphanedConversations applies the same
// predicate in its own boot sweep (it can't share this body — different store,
// different scope).
//
// A park, not a terminal: the child neither failed nor concluded, it was
// stopped when its parent ended. Read the `open` as "stopped without
// concluding", NOT as "resumable" — the parent is terminal, so the claim gate
// refuses it. Scoped to status IS NULL: a child that already parked itself
// keeps its own parked_at and park_reason, and one that already reached a
// terminal is left alone.
func parkOrphanedChildConversations(ctx context.Context, q queryer, blueprintRunID string) error {
	// Claims first: the subquery's mid-flight predicate matches exactly the
	// rows the park below is about to retire, and both statements share the
	// caller's transaction, so the pair lands atomically either way.
	if _, err := q.ExecContext(ctx, `
		UPDATE claims SET released_at = ?, outcome = 'cancelled'
		WHERE released_at IS NULL
		  AND conversation_id IN (
		      SELECT id FROM conversations
		      WHERE blueprint_run_id = ? AND status IS NULL)
	`, time.Now().UTC(), blueprintRunID); err != nil {
		return err
	}
	_, err := q.ExecContext(ctx, `
		UPDATE conversations
		SET status = 'open',
		    parked_at = COALESCE(parked_at, ?),
		    park_reason = COALESCE(park_reason, 'blueprint_terminal'),
		    result_summary = COALESCE(NULLIF(result_summary, ''), ?)
		WHERE blueprint_run_id = ? AND status IS NULL
	`, time.Now().UTC(), "Stopped: owning blueprint run reached a terminal state", blueprintRunID)
	return err
}

func (s *blueprintStore) ReopenRunForResume(ctx context.Context, orgID, id string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	// CAS aborted → running, clearing the stale abort metadata. The resumed step
	// re-finalizes the blueprint through the normal post-resume disposition.
	res, err := s.q.ExecContext(ctx, `
		UPDATE blueprint_runs
		SET status = 'running', abort_reason = NULL, aborted_at_step = NULL, completed_at = NULL
		WHERE id = ? AND status = 'aborted'
	`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *blueprintStore) SetRunCurrentStepSystem(ctx context.Context, orgID, id string, stepIndex int) (domain.BlueprintRun, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.BlueprintRun{}, err
	}
	return scanUpdatedBlueprintRunSQLite(s.q.QueryRowContext(ctx,
		`UPDATE blueprint_runs SET current_step_index = ? WHERE id = ? RETURNING `+blueprintRunColumns,
		stepIndex, id))
}

func (s *blueprintStore) RequestRunCancelSystem(ctx context.Context, orgID, id string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE blueprint_runs SET cancel_requested = 1
		WHERE id = ? AND status = 'running' AND cancel_requested = 0
	`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *blueprintStore) ConversationsForBlueprint(ctx context.Context, orgID, blueprintRunID string) ([]domain.Conversation, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// Status is the derived display ladder — the same contract as every
	// other ConversationStore projection.
	rows, err := s.q.QueryContext(ctx, `
		SELECT r.id, r.task_id, r.prompt_id,
		       `+sqliteDisplayStatusSQL+`,
		       r.model, r.started_at, r.completed_at,
		       (SELECT SUM(m.cost_usd) FROM messages m WHERE m.conversation_id = r.id),
		       (SELECT SUM(cl.duration_ms) FROM claims cl WHERE cl.conversation_id = r.id),
		       (SELECT SUM(cl.num_turns) FROM claims cl WHERE cl.conversation_id = r.id),
		       r.park_reason, r.worktree_path,
		       r.result_summary, r.sdk_session_id, r.outcome, r.outcome_reason,
		       r.blueprint_run_id, r.blueprint_step_index
		FROM conversations r
		WHERE r.blueprint_run_id = ?
		ORDER BY r.blueprint_step_index ASC, r.started_at ASC
	`, blueprintRunID)
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

func (s *blueprintStore) ActiveStepConversationIDs(ctx context.Context, orgID, blueprintRunID string) ([]string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id FROM conversations
		WHERE blueprint_run_id = ?
		  AND (status IS NULL
		       OR status NOT IN (`+conversationTerminalStatusesSQL+`,'open'))
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

func (s *blueprintStore) StepPlanLengths(ctx context.Context, orgID string, blueprintRunIDs []string) (map[string]int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(blueprintRunIDs))
	if len(blueprintRunIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(blueprintRunIDs))
	for i, id := range blueprintRunIDs {
		args[i] = id
	}
	ph := strings.TrimRight(strings.Repeat("?, ", len(args)), ", ")
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, json_array_length(step_plan)
		FROM blueprint_runs WHERE id IN (`+ph+`)
	`, args...)
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

func (s *blueprintStore) ActiveStepConversationIDsSystem(ctx context.Context, orgID, blueprintRunID string) ([]string, error) {
	return s.ActiveStepConversationIDs(ctx, orgID, blueprintRunID)
}

func (s *blueprintStore) ConversationsForBlueprintSystem(ctx context.Context, orgID, blueprintRunID string) ([]domain.Conversation, error) {
	return s.ConversationsForBlueprint(ctx, orgID, blueprintRunID)
}
