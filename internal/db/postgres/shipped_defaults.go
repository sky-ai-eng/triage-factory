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

// shippedDefaultsStore is the Postgres impl of db.ShippedDefaultsStore.
//
// SeedShippedIntoTeam runs entirely on the admin pool (supabase_admin,
// BYPASSRLS) — it is claims-less bootstrap work. Phases 1+2 (prompts,
// blueprints+steps) run in one admin-pool transaction; phase 3 delegates to
// the composed EventHandlerStore.Seed, which manages its own admin-pool
// writes. Inside WithTx, admin points at the caller's *sql.Tx and inTx is
// true; SeedShippedIntoTeam refuses to run there.
type shippedDefaultsStore struct {
	admin         queryer
	inTx          bool
	eventHandlers db.EventHandlerStore
}

func newShippedDefaultsStore(admin queryer, eventHandlers db.EventHandlerStore) db.ShippedDefaultsStore {
	return &shippedDefaultsStore{admin: admin, eventHandlers: eventHandlers}
}

// newTxShippedDefaultsStore composes a tx-bound ShippedDefaultsStore for
// WithTx / NewForTx. SeedShippedIntoTeam refuses to run there (inTx=true).
func newTxShippedDefaultsStore(tx queryer, eventHandlers db.EventHandlerStore) db.ShippedDefaultsStore {
	return &shippedDefaultsStore{admin: tx, inTx: true, eventHandlers: eventHandlers}
}

var _ db.ShippedDefaultsStore = (*shippedDefaultsStore)(nil)

func (s *shippedDefaultsStore) SeedShippedIntoTeam(ctx context.Context, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	if s.inTx {
		return errors.New("postgres shipped_defaults: SeedShippedIntoTeam must not be called inside WithTx; call stores.ShippedDefaults.SeedShippedIntoTeam directly")
	}
	if teamID == "" {
		return errors.New("postgres shipped_defaults: SeedShippedIntoTeam requires team_id")
	}
	conn, ok := s.admin.(*sql.DB)
	if !ok {
		return fmt.Errorf("postgres shipped_defaults: SeedShippedIntoTeam requires a *sql.DB admin handle, got %T", s.admin)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin admin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	// Phase 1: prompts. ON CONFLICT DO NOTHING makes re-seeding idempotent;
	// the follow-up SELECT resolves the (existing or freshly inserted) id
	// so blueprint steps + triggers can wire to it. Always source='system',
	// creator_user_id NULL — the shipped Go slices carry no other source.
	promptIDsBySlug := make(map[string]string, len(shippedPrompts))
	for _, p := range shippedPrompts {
		if p.SystemSlug == "" {
			return fmt.Errorf("shipped_defaults seed: shipped prompt %q has empty system_slug", p.Name)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO prompts (id, org_id, team_id, system_slug, creator_user_id, name, body, source, allowed_tools, model, usage_count, user_modified, created_at, updated_at)
			VALUES ($1, $2, $3::uuid, $4, NULL, $5, $6, 'system', $7, $8, 0, FALSE, $9, $9)
			ON CONFLICT (org_id, team_id, system_slug) DO NOTHING
		`, uuid.New().String(), orgID, teamID, p.SystemSlug, p.Name, p.Body, p.AllowedTools, p.Model, now); err != nil {
			return fmt.Errorf("seed prompt %s: %w", p.SystemSlug, err)
		}
		var id string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM prompts WHERE org_id = $1 AND team_id = $2 AND system_slug = $3`, orgID, teamID, p.SystemSlug,
		).Scan(&id); err != nil {
			return fmt.Errorf("resolve prompt %s: %w", p.SystemSlug, err)
		}
		promptIDsBySlug[p.SystemSlug] = id
	}

	// Phase 2: blueprints + steps. Steps are written only when the
	// blueprint is freshly inserted (RowsAffected > 0) so a re-seed never
	// clobbers a team's edited step list.
	blueprintIDsBySlug := make(map[string]string, len(shippedBlueprints))
	for _, b := range shippedBlueprints {
		if b.SystemSlug == "" {
			return fmt.Errorf("shipped_defaults seed: shipped blueprint %q has empty system_slug", b.Name)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO blueprints (id, org_id, team_id, system_slug, creator_user_id, name, source, usage_count, user_modified, created_at, updated_at)
			VALUES ($1, $2, $3::uuid, $4, NULL, $5, 'system', 0, FALSE, $6, $6)
			ON CONFLICT (org_id, team_id, system_slug) DO NOTHING
		`, uuid.New().String(), orgID, teamID, b.SystemSlug, b.Name, now)
		if err != nil {
			return fmt.Errorf("seed blueprint %s: %w", b.SystemSlug, err)
		}
		var bpID string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM blueprints WHERE org_id = $1 AND team_id = $2 AND system_slug = $3`, orgID, teamID, b.SystemSlug,
		).Scan(&bpID); err != nil {
			return fmt.Errorf("resolve blueprint %s: %w", b.SystemSlug, err)
		}
		blueprintIDsBySlug[b.SystemSlug] = bpID
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue // already existed — leave its steps untouched
		}
		for i, slug := range b.StepPromptSlugs {
			promptID, ok := promptIDsBySlug[slug]
			if !ok || promptID == "" {
				return fmt.Errorf("seed blueprint %s: step prompt slug %q not found (seed prompts before blueprints)", b.SystemSlug, slug)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
				VALUES ($1, $2::uuid, $3, $4, $5, '', $6)
			`, orgID, teamID, bpID, i, promptID, now); err != nil {
				return fmt.Errorf("seed blueprint %s step %d: %w", b.SystemSlug, i, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit shipped_defaults seed: %w", err)
	}

	// Phase 3: handlers. EventHandlerStore.Seed already does exactly this —
	// rules enabled, triggers disabled (shipped convention), each trigger's
	// blueprint slug resolved through blueprintIDsBySlug — so reuse it
	// rather than duplicate its SQL.
	if err := s.eventHandlers.Seed(ctx, orgID, teamID, blueprintIDsBySlug); err != nil {
		return fmt.Errorf("shipped_defaults seed: %w", err)
	}
	return nil
}

// SyncShippedIntoTeam brings the team's unmodified shipped-default copies up to
// the current shipped content. See db.ShippedDefaultsStore for the normative
// rules. It runs the one-time grandfather backfill (gated on
// teams.shipped_defaults_backfilled_at), then syncs each shipped blueprint's
// unit in its own admin-pool transaction.
func (s *shippedDefaultsStore) SyncShippedIntoTeam(ctx context.Context, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	if s.inTx {
		return errors.New("postgres shipped_defaults: SyncShippedIntoTeam must not be called inside WithTx")
	}
	if teamID == "" {
		return errors.New("postgres shipped_defaults: SyncShippedIntoTeam requires team_id")
	}
	conn, ok := s.admin.(*sql.DB)
	if !ok {
		return fmt.Errorf("postgres shipped_defaults: SyncShippedIntoTeam requires a *sql.DB admin handle, got %T", s.admin)
	}
	shippedBySlug := db.ShippedPromptsBySlug(shippedPrompts)
	if err := db.ValidateShippedConsistency(shippedBlueprints, shippedBySlug); err != nil {
		return fmt.Errorf("shipped defaults sync: %w", err)
	}
	if err := s.backfillShippedDefaults(ctx, conn, orgID, teamID, shippedBlueprints); err != nil {
		return fmt.Errorf("shipped defaults sync backfill: %w", err)
	}
	for _, b := range shippedBlueprints {
		if b.SystemSlug == "" {
			return fmt.Errorf("shipped defaults sync: shipped blueprint %q has empty system_slug", b.Name)
		}
		if err := s.syncUnit(ctx, conn, orgID, teamID, b, shippedBySlug); err != nil {
			return fmt.Errorf("shipped defaults sync: blueprint %s: %w", b.SystemSlug, err)
		}
	}

	// Handlers sync last: a trigger's blueprint binding re-resolves by shipped
	// slug against the blueprint state the loop above just produced (fresh
	// inserts included), so it must run after every blueprint unit has synced.
	blueprintIDsBySlug, err := resolveBlueprintIDsBySlugPG(ctx, conn, orgID, teamID, shippedBlueprints)
	if err != nil {
		return fmt.Errorf("shipped defaults sync: resolve blueprint ids: %w", err)
	}
	if err := s.eventHandlers.Sync(ctx, orgID, teamID, db.ShippedEventHandlers, blueprintIDsBySlug); err != nil {
		return fmt.Errorf("shipped defaults sync: handlers: %w", err)
	}
	return nil
}

// resolveBlueprintIDsBySlugPG reads each shipped blueprint's current
// non-deleted team-copy id, for EventHandlerStore.Sync to resolve a shipped
// trigger's blueprint slug against. A slug with no non-deleted row (missing,
// or soft-deleted by the user) is omitted — Sync then skips any trigger that
// targets it rather than wiring a dangling or resurrected reference.
func resolveBlueprintIDsBySlugPG(ctx context.Context, conn *sql.DB, orgID, teamID string, shippedBlueprints []domain.SeedBlueprint) (map[string]string, error) {
	ids := make(map[string]string, len(shippedBlueprints))
	for _, b := range shippedBlueprints {
		var id string
		switch err := conn.QueryRowContext(ctx,
			`SELECT id FROM blueprints WHERE org_id = $1 AND team_id = $2 AND system_slug = $3 AND deleted_at IS NULL`,
			orgID, teamID, b.SystemSlug,
		).Scan(&id); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return nil, fmt.Errorf("resolve blueprint %s: %w", b.SystemSlug, err)
		}
		ids[b.SystemSlug] = id
	}
	return ids, nil
}

// backfillShippedDefaults runs the one-time grandfather pass on the admin pool.
func (s *shippedDefaultsStore) backfillShippedDefaults(ctx context.Context, conn *sql.DB, orgID, teamID string, shippedBlueprints []domain.SeedBlueprint) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin admin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var backfilledAt sql.NullTime
	switch err := tx.QueryRowContext(ctx,
		`SELECT shipped_defaults_backfilled_at FROM teams WHERE org_id = $1 AND id = $2`, orgID, teamID,
	).Scan(&backfilledAt); {
	case errors.Is(err, sql.ErrNoRows):
		return nil // team vanished mid-sweep; nothing to backfill
	case err != nil:
		return fmt.Errorf("read backfill marker: %w", err)
	}
	if backfilledAt.Valid {
		return nil // already backfilled
	}
	now := time.Now().UTC()
	for _, b := range shippedBlueprints {
		if b.SystemSlug == "" {
			continue
		}
		var bpID string
		switch err := tx.QueryRowContext(ctx,
			`SELECT id FROM blueprints WHERE org_id = $1 AND team_id = $2 AND system_slug = $3 AND deleted_at IS NULL`,
			orgID, teamID, b.SystemSlug,
		).Scan(&bpID); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return fmt.Errorf("read blueprint %s: %w", b.SystemSlug, err)
		}
		slugs, err := pgLoadStepSlugs(ctx, tx, orgID, bpID)
		if err != nil {
			return err
		}
		if db.SlugsDiverged(slugs, b.StepPromptSlugs) {
			// The blueprints set_updated_at BEFORE-UPDATE trigger bumps updated_at
			// on this stamp — unlike SQLite, which deliberately preserves it. That
			// cosmetic divergence is accepted: multi-mode has no users yet and the
			// grandfather pass runs once. Suppressing the trigger here would need
			// schema surgery for no present benefit.
			if _, err := tx.ExecContext(ctx,
				`UPDATE blueprints SET user_modified = TRUE WHERE org_id = $1 AND id = $2`, orgID, bpID,
			); err != nil {
				return fmt.Errorf("stamp user_modified on %s: %w", b.SystemSlug, err)
			}
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE teams SET shipped_defaults_backfilled_at = $1 WHERE org_id = $2 AND id = $3`, now, orgID, teamID,
	); err != nil {
		return fmt.Errorf("set backfill marker: %w", err)
	}
	return tx.Commit()
}

// syncUnit loads one shipped blueprint's team-side state, plans the sync, and
// executes it in one admin-pool transaction.
func (s *shippedDefaultsStore) syncUnit(ctx context.Context, conn *sql.DB, orgID, teamID string, b domain.SeedBlueprint, shippedBySlug map[string]domain.Prompt) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin admin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	st, err := pgLoadTeamUnitState(ctx, tx, orgID, teamID, b)
	if err != nil {
		return err
	}
	plan := db.PlanUnitSync(b, shippedBySlug, st)
	switch plan.Action {
	case db.UnitSkip, db.UnitEqual:
		return nil // nothing written; rollback is a no-op
	case db.UnitInsert, db.UnitApply:
		if err := pgExecuteUnitPlan(ctx, tx, orgID, teamID, b, st, plan); err != nil {
			return err
		}
		return tx.Commit()
	}
	return nil
}

// pgLoadStepSlugs returns a blueprint's ordered step prompt system_slugs.
func pgLoadStepSlugs(ctx context.Context, q queryer, orgID, blueprintID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT COALESCE(p.system_slug, '')
		FROM blueprint_steps bs
		JOIN prompts p ON p.id = bs.step_prompt_id AND p.org_id = bs.org_id
		WHERE bs.org_id = $1 AND bs.blueprint_id = $2
		ORDER BY bs.step_index
	`, orgID, blueprintID)
	if err != nil {
		return nil, fmt.Errorf("read step slugs: %w", err)
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// pgLoadTeamUnitState reads the team-side state of one shipped blueprint's unit.
func pgLoadTeamUnitState(ctx context.Context, q queryer, orgID, teamID string, b domain.SeedBlueprint) (db.TeamUnitState, error) {
	st := db.TeamUnitState{PromptsBySlug: map[string]db.TeamPromptRow{}}

	var (
		bpID   string
		bpName string
		bpMod  bool
		bpDel  bool
	)
	switch err := q.QueryRowContext(ctx, `
		SELECT id, name, user_modified, (deleted_at IS NOT NULL)
		FROM blueprints WHERE org_id = $1 AND team_id = $2 AND system_slug = $3
	`, orgID, teamID, b.SystemSlug).Scan(&bpID, &bpName, &bpMod, &bpDel); {
	case errors.Is(err, sql.ErrNoRows):
		st.BlueprintExists = false
	case err != nil:
		return st, fmt.Errorf("read blueprint header: %w", err)
	default:
		st.BlueprintExists = true
		st.BlueprintID = bpID
		st.BlueprintName = bpName
		st.BlueprintModified = bpMod
		st.BlueprintDeleted = bpDel
	}

	if st.BlueprintExists {
		rows, err := q.QueryContext(ctx, `
			SELECT COALESCE(p.system_slug, ''), bs.brief, p.id, p.name, p.body, p.model, p.allowed_tools,
			       p.user_modified, (p.deleted_at IS NOT NULL)
			FROM blueprint_steps bs
			JOIN prompts p ON p.id = bs.step_prompt_id AND p.org_id = bs.org_id
			WHERE bs.org_id = $1 AND bs.blueprint_id = $2
			ORDER BY bs.step_index
		`, orgID, st.BlueprintID)
		if err != nil {
			return st, fmt.Errorf("read steps: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				slug, brief, pid, name, body, model, tools string
				mod, del                                   bool
			)
			if err := rows.Scan(&slug, &brief, &pid, &name, &body, &model, &tools, &mod, &del); err != nil {
				return st, err
			}
			st.StepSlugs = append(st.StepSlugs, slug)
			st.StepBriefs = append(st.StepBriefs, brief)
			st.StepPromptIDs = append(st.StepPromptIDs, pid)
			st.CurrentStep = append(st.CurrentStep, db.TeamPromptRow{
				Exists: true, Deleted: del, UserModified: mod,
				ID: pid, Name: name, Body: body, Model: model, AllowedTools: tools,
			})
		}
		if err := rows.Err(); err != nil {
			return st, err
		}
	}

	for _, slug := range b.StepPromptSlugs {
		if _, done := st.PromptsBySlug[slug]; done {
			continue
		}
		row, err := pgProbePromptBySlug(ctx, q, orgID, teamID, slug)
		if err != nil {
			return st, err
		}
		st.PromptsBySlug[slug] = row
	}
	return st, nil
}

// pgProbePromptBySlug reads the team's prompt row for one (org, team, slug),
// including a soft-deleted occupant.
func pgProbePromptBySlug(ctx context.Context, q queryer, orgID, teamID, slug string) (db.TeamPromptRow, error) {
	var (
		id, name, body, model, tools string
		mod, del                     bool
	)
	switch err := q.QueryRowContext(ctx, `
		SELECT id, name, body, model, allowed_tools, user_modified, (deleted_at IS NOT NULL)
		FROM prompts WHERE org_id = $1 AND team_id = $2 AND system_slug = $3
	`, orgID, teamID, slug).Scan(&id, &name, &body, &model, &tools, &mod, &del); {
	case errors.Is(err, sql.ErrNoRows):
		return db.TeamPromptRow{Exists: false}, nil
	case err != nil:
		return db.TeamPromptRow{}, fmt.Errorf("probe prompt %s: %w", slug, err)
	}
	return db.TeamPromptRow{
		Exists: true, Deleted: del, UserModified: mod,
		ID: id, Name: name, Body: body, Model: model, AllowedTools: tools,
	}, nil
}

// pgExecuteUnitPlan writes a UnitInsert or UnitApply plan on the given tx.
func pgExecuteUnitPlan(ctx context.Context, q queryer, orgID, teamID string, b domain.SeedBlueprint, st db.TeamUnitState, plan db.UnitSyncPlan) error {
	now := time.Now().UTC()
	blueprintID := st.BlueprintID

	if plan.Action == db.UnitInsert {
		blueprintID = uuid.New().String()
		if _, err := q.ExecContext(ctx, `
			INSERT INTO blueprints (id, org_id, team_id, system_slug, creator_user_id, name, source, usage_count, user_modified, created_at, updated_at)
			VALUES ($1, $2, $3::uuid, $4, NULL, $5, 'system', 0, FALSE, $6, $6)
		`, blueprintID, orgID, teamID, b.SystemSlug, plan.HeaderName, now); err != nil {
			return fmt.Errorf("insert blueprint header: %w", err)
		}
	}

	resolved := make(map[string]string, len(plan.StepTargets))
	for slug, row := range st.PromptsBySlug {
		if row.Exists && !row.Deleted {
			resolved[slug] = row.ID
		}
	}
	for _, w := range plan.PromptWrites {
		if w.Insert {
			id := uuid.New().String()
			if _, err := q.ExecContext(ctx, `
				INSERT INTO prompts (id, org_id, team_id, system_slug, creator_user_id, name, body, source, allowed_tools, model, usage_count, user_modified, created_at, updated_at)
				VALUES ($1, $2, $3::uuid, $4, NULL, $5, $6, 'system', $7, $8, 0, FALSE, $9, $9)
			`, id, orgID, teamID, w.Slug, w.Name, w.Body, w.AllowedTools, w.Model, now); err != nil {
				return fmt.Errorf("insert step prompt %s: %w", w.Slug, err)
			}
			resolved[w.Slug] = id
		} else {
			if _, err := q.ExecContext(ctx, `
				UPDATE prompts SET name = $1, body = $2, model = $3, allowed_tools = $4, updated_at = $5
				WHERE org_id = $6 AND id = $7
			`, w.Name, w.Body, w.Model, w.AllowedTools, now, orgID, w.PromptID); err != nil {
				return fmt.Errorf("update step prompt %s: %w", w.Slug, err)
			}
		}
	}

	if plan.Action == db.UnitInsert || plan.ReplaceSteps {
		if _, err := q.ExecContext(ctx,
			`DELETE FROM blueprint_steps WHERE org_id = $1 AND blueprint_id = $2`, orgID, blueprintID,
		); err != nil {
			return fmt.Errorf("clear steps: %w", err)
		}
		for i, slug := range plan.StepTargets {
			pid, ok := resolved[slug]
			if !ok || pid == "" {
				return fmt.Errorf("resolve step prompt for slug %q: not found", slug)
			}
			if _, err := q.ExecContext(ctx, `
				INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
				VALUES ($1, $2::uuid, $3, $4, $5, '', $6)
			`, orgID, teamID, blueprintID, i, pid, now); err != nil {
				return fmt.Errorf("insert step %d: %w", i, err)
			}
		}
	}

	for _, pid := range plan.DropPromptIDs {
		if _, err := q.ExecContext(ctx,
			`UPDATE prompts SET deleted_at = $1 WHERE org_id = $2 AND id = $3`, now, orgID, pid,
		); err != nil {
			return fmt.Errorf("soft-delete dropped prompt %s: %w", pid, err)
		}
	}

	if plan.Action == db.UnitApply && (plan.RenameHeader || plan.ReplaceSteps) {
		if _, err := q.ExecContext(ctx,
			`UPDATE blueprints SET name = $1, updated_at = $2 WHERE org_id = $3 AND id = $4`,
			plan.HeaderName, now, orgID, blueprintID,
		); err != nil {
			return fmt.Errorf("update blueprint header: %w", err)
		}
	}
	return nil
}
