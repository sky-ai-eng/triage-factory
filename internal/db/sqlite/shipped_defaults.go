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
)

// shippedDefaultsStore is the SQLite impl of db.ShippedDefaultsStore. SQLite
// has one connection: SeedShippedIntoTeam's phase 1+2 writes wrap in a tx via
// the inTx helper, then phase 3 delegates to the composed EventHandlerStore
// (bound to the same connection).
//
// inTx mirrors the Postgres impl's guard: constructed via
// newTxShippedDefaultsStore (the TxStores wiring in tx.go), SeedShippedIntoTeam
// refuses to run. SQLite has no genuine pool-escape hazard the way Postgres
// does — everything shares one connection — but enforcing the same "must run
// outside WithTx" contract on both dialects means a misuse fails loudly in
// SQLite/local-mode tests instead of silently working there and only
// surfacing as a Postgres/multi-mode production error.
type shippedDefaultsStore struct {
	q             queryer
	inTx          bool
	eventHandlers db.EventHandlerStore
}

func newShippedDefaultsStore(q queryer, eventHandlers db.EventHandlerStore) db.ShippedDefaultsStore {
	return &shippedDefaultsStore{q: q, eventHandlers: eventHandlers}
}

// newTxShippedDefaultsStore composes a tx-bound ShippedDefaultsStore for
// TxStores. SeedShippedIntoTeam refuses to run there — mirrors the
// Postgres newTxShippedDefaultsStore.
func newTxShippedDefaultsStore(tx queryer, eventHandlers db.EventHandlerStore) db.ShippedDefaultsStore {
	return &shippedDefaultsStore{q: tx, inTx: true, eventHandlers: eventHandlers}
}

var _ db.ShippedDefaultsStore = (*shippedDefaultsStore)(nil)

func (s *shippedDefaultsStore) SeedShippedIntoTeam(ctx context.Context, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	if s.inTx {
		return errors.New("sqlite shipped_defaults: SeedShippedIntoTeam must not be called inside WithTx; call stores.ShippedDefaults.SeedShippedIntoTeam directly")
	}
	if teamID == "" {
		return errors.New("sqlite shipped_defaults: SeedShippedIntoTeam requires team_id")
	}
	now := time.Now().UTC()
	blueprintIDsBySlug := make(map[string]string, len(shippedBlueprints))

	err := inTx(ctx, s.q, func(q queryer) error {
		// Phase 1: prompts. INSERT OR IGNORE makes re-seeding idempotent;
		// the follow-up SELECT resolves the (existing or fresh) id for the
		// blueprint step wiring. Always source='system', creator_user_id
		// NULL — the shipped Go slices carry no other source.
		promptIDsBySlug := make(map[string]string, len(shippedPrompts))
		for _, p := range shippedPrompts {
			if p.SystemSlug == "" {
				return fmt.Errorf("shipped_defaults seed: shipped prompt %q has empty system_slug", p.Name)
			}
			if _, err := q.ExecContext(ctx, `
				INSERT OR IGNORE INTO prompts (id, org_id, team_id, system_slug, creator_user_id, name, body, source, allowed_tools, model, usage_count, user_modified, created_at, updated_at)
				VALUES (?, ?, ?, ?, NULL, ?, ?, 'system', ?, ?, 0, 0, ?, ?)
			`, uuid.New().String(), orgID, teamID, p.SystemSlug, p.Name, p.Body, p.AllowedTools, p.Model, now, now); err != nil {
				return fmt.Errorf("seed prompt %s: %w", p.SystemSlug, err)
			}
			var id string
			if err := q.QueryRowContext(ctx,
				`SELECT id FROM prompts WHERE org_id = ? AND team_id = ? AND system_slug = ?`, orgID, teamID, p.SystemSlug,
			).Scan(&id); err != nil {
				return fmt.Errorf("resolve prompt %s: %w", p.SystemSlug, err)
			}
			promptIDsBySlug[p.SystemSlug] = id
		}

		// Phase 2: blueprints + steps. Steps are written only when the
		// blueprint is freshly inserted so a re-seed never clobbers a
		// team's edited step list.
		for _, b := range shippedBlueprints {
			if b.SystemSlug == "" {
				return fmt.Errorf("shipped_defaults seed: shipped blueprint %q has empty system_slug", b.Name)
			}
			res, err := q.ExecContext(ctx, `
				INSERT OR IGNORE INTO blueprints (id, org_id, team_id, system_slug, creator_user_id, name, source, usage_count, user_modified, created_at, updated_at)
				VALUES (?, ?, ?, ?, NULL, ?, 'system', 0, 0, ?, ?)
			`, uuid.New().String(), orgID, teamID, b.SystemSlug, b.Name, now, now)
			if err != nil {
				return fmt.Errorf("seed blueprint %s: %w", b.SystemSlug, err)
			}
			var bpID string
			if err := q.QueryRowContext(ctx,
				`SELECT id FROM blueprints WHERE org_id = ? AND team_id = ? AND system_slug = ?`, orgID, teamID, b.SystemSlug,
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
				if _, err := q.ExecContext(ctx, `
					INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id, team_id, brief, created_at)
					VALUES (?, ?, ?, ?, '', ?)
				`, bpID, i, promptID, teamID, now); err != nil {
					return fmt.Errorf("seed blueprint %s step %d: %w", b.SystemSlug, i, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
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
// unit in its own transaction.
func (s *shippedDefaultsStore) SyncShippedIntoTeam(ctx context.Context, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	if s.inTx {
		return errors.New("sqlite shipped_defaults: SyncShippedIntoTeam must not be called inside WithTx")
	}
	if teamID == "" {
		return errors.New("sqlite shipped_defaults: SyncShippedIntoTeam requires team_id")
	}
	shippedBySlug := db.ShippedPromptsBySlug(shippedPrompts)
	if err := db.ValidateShippedConsistency(shippedBlueprints, shippedBySlug); err != nil {
		return fmt.Errorf("shipped defaults sync: %w", err)
	}
	if err := s.backfillShippedDefaults(ctx, orgID, teamID, shippedBlueprints); err != nil {
		return fmt.Errorf("shipped defaults sync backfill: %w", err)
	}
	for _, b := range shippedBlueprints {
		if b.SystemSlug == "" {
			return fmt.Errorf("shipped defaults sync: shipped blueprint %q has empty system_slug", b.Name)
		}
		if err := s.syncUnit(ctx, orgID, teamID, b, shippedBySlug); err != nil {
			return fmt.Errorf("shipped defaults sync: blueprint %s: %w", b.SystemSlug, err)
		}
	}

	// Handlers sync last: a trigger's blueprint binding re-resolves by shipped
	// slug against the blueprint state the loop above just produced (fresh
	// inserts included), so it must run after every blueprint unit has synced.
	blueprintIDsBySlug, err := s.resolveBlueprintIDsBySlug(ctx, orgID, teamID, shippedBlueprints)
	if err != nil {
		return fmt.Errorf("shipped defaults sync: resolve blueprint ids: %w", err)
	}
	if err := s.eventHandlers.Sync(ctx, orgID, teamID, db.ShippedEventHandlers, blueprintIDsBySlug); err != nil {
		return fmt.Errorf("shipped defaults sync: handlers: %w", err)
	}
	return nil
}

// resolveBlueprintIDsBySlug reads each shipped blueprint's current
// non-deleted team-copy id, for EventHandlerStore.Sync to resolve a shipped
// trigger's blueprint slug against. A slug with no non-deleted row (missing,
// or soft-deleted by the user) is omitted — Sync then skips any trigger that
// targets it rather than wiring a dangling or resurrected reference.
func (s *shippedDefaultsStore) resolveBlueprintIDsBySlug(ctx context.Context, orgID, teamID string, shippedBlueprints []domain.SeedBlueprint) (map[string]string, error) {
	ids := make(map[string]string, len(shippedBlueprints))
	for _, b := range shippedBlueprints {
		var id string
		switch err := s.q.QueryRowContext(ctx,
			`SELECT id FROM blueprints WHERE org_id = ? AND team_id = ? AND system_slug = ? AND deleted_at IS NULL`,
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

// backfillShippedDefaults runs the one-time grandfather pass: if the team's
// marker is unset, stamp user_modified on any system-slugged, non-deleted
// blueprint whose ordered step structure already diverges from shipped, then
// set the marker so it never runs again.
func (s *shippedDefaultsStore) backfillShippedDefaults(ctx context.Context, orgID, teamID string, shippedBlueprints []domain.SeedBlueprint) error {
	return inTx(ctx, s.q, func(q queryer) error {
		var backfilledAt sql.NullTime
		switch err := q.QueryRowContext(ctx,
			`SELECT shipped_defaults_backfilled_at FROM teams WHERE org_id = ? AND id = ?`, orgID, teamID,
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
			switch err := q.QueryRowContext(ctx,
				`SELECT id FROM blueprints WHERE org_id = ? AND team_id = ? AND system_slug = ? AND deleted_at IS NULL`,
				orgID, teamID, b.SystemSlug,
			).Scan(&bpID); {
			case errors.Is(err, sql.ErrNoRows):
				continue
			case err != nil:
				return fmt.Errorf("read blueprint %s: %w", b.SystemSlug, err)
			}
			slugs, err := loadStepSlugs(ctx, q, bpID)
			if err != nil {
				return err
			}
			if db.SlugsDiverged(slugs, b.StepPromptSlugs) {
				// Deliberately leaves updated_at untouched: the grandfather stamp
				// is a silent correction that shouldn't reorder the (updated_at
				// DESC) blueprint list for local users. Postgres bumps updated_at
				// here via its unconditional set_updated_at trigger, an accepted
				// cosmetic divergence — multi-mode has no users yet and the pass
				// is one-time.
				if _, err := q.ExecContext(ctx,
					`UPDATE blueprints SET user_modified = 1 WHERE org_id = ? AND id = ?`, orgID, bpID,
				); err != nil {
					return fmt.Errorf("stamp user_modified on %s: %w", b.SystemSlug, err)
				}
			}
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE teams SET shipped_defaults_backfilled_at = ? WHERE org_id = ? AND id = ?`, now, orgID, teamID,
		); err != nil {
			return fmt.Errorf("set backfill marker: %w", err)
		}
		return nil
	})
}

// syncUnit loads one shipped blueprint's team-side state, plans the sync, and
// executes it — all in one transaction so a partially-applied unit can't commit.
func (s *shippedDefaultsStore) syncUnit(ctx context.Context, orgID, teamID string, b domain.SeedBlueprint, shippedBySlug map[string]domain.Prompt) error {
	return inTx(ctx, s.q, func(q queryer) error {
		st, err := loadTeamUnitState(ctx, q, orgID, teamID, b)
		if err != nil {
			return err
		}
		plan := db.PlanUnitSync(b, shippedBySlug, st)
		switch plan.Action {
		case db.UnitSkip, db.UnitEqual:
			return nil
		case db.UnitInsert, db.UnitApply:
			return executeUnitPlan(ctx, q, orgID, teamID, b, st, plan)
		}
		return nil
	})
}

// loadStepSlugs returns a blueprint's ordered step prompt system_slugs ("" for a
// step whose prompt carries no system_slug).
func loadStepSlugs(ctx context.Context, q queryer, blueprintID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT COALESCE(p.system_slug, '')
		FROM blueprint_steps bs
		JOIN prompts p ON p.id = bs.step_prompt_id
		WHERE bs.blueprint_id = ?
		ORDER BY bs.step_index
	`, blueprintID)
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

// loadTeamUnitState reads the team-side state of one shipped blueprint's sync
// unit: the header (including a soft-deleted occupant), the current ordered step
// list with each step prompt's content + flags, and a probe of every shipped
// step slug into PromptsBySlug.
func loadTeamUnitState(ctx context.Context, q queryer, orgID, teamID string, b domain.SeedBlueprint) (db.TeamUnitState, error) {
	st := db.TeamUnitState{PromptsBySlug: map[string]db.TeamPromptRow{}}

	var (
		bpID   string
		bpName string
		bpMod  int
		bpDel  int
	)
	switch err := q.QueryRowContext(ctx, `
		SELECT id, name, user_modified, CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END
		FROM blueprints WHERE org_id = ? AND team_id = ? AND system_slug = ?
	`, orgID, teamID, b.SystemSlug).Scan(&bpID, &bpName, &bpMod, &bpDel); {
	case errors.Is(err, sql.ErrNoRows):
		st.BlueprintExists = false
	case err != nil:
		return st, fmt.Errorf("read blueprint header: %w", err)
	default:
		st.BlueprintExists = true
		st.BlueprintID = bpID
		st.BlueprintName = bpName
		st.BlueprintModified = bpMod != 0
		st.BlueprintDeleted = bpDel != 0
	}

	if st.BlueprintExists {
		rows, err := q.QueryContext(ctx, `
			SELECT COALESCE(p.system_slug, ''), bs.brief, p.id, p.name, p.body, p.model, p.allowed_tools,
			       p.user_modified, CASE WHEN p.deleted_at IS NULL THEN 0 ELSE 1 END
			FROM blueprint_steps bs
			JOIN prompts p ON p.id = bs.step_prompt_id
			WHERE bs.blueprint_id = ?
			ORDER BY bs.step_index
		`, st.BlueprintID)
		if err != nil {
			return st, fmt.Errorf("read steps: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				slug, brief, pid, name, body, model, tools string
				mod, del                                   int
			)
			if err := rows.Scan(&slug, &brief, &pid, &name, &body, &model, &tools, &mod, &del); err != nil {
				return st, err
			}
			st.StepSlugs = append(st.StepSlugs, slug)
			st.StepBriefs = append(st.StepBriefs, brief)
			st.StepPromptIDs = append(st.StepPromptIDs, pid)
			st.CurrentStep = append(st.CurrentStep, db.TeamPromptRow{
				Exists: true, Deleted: del != 0, UserModified: mod != 0,
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
		row, err := probePromptBySlug(ctx, q, orgID, teamID, slug)
		if err != nil {
			return st, err
		}
		st.PromptsBySlug[slug] = row
	}
	return st, nil
}

// probePromptBySlug reads the team's prompt row for one (org, team, system_slug),
// including a soft-deleted occupant (Deleted=true). Exists=false when no row.
func probePromptBySlug(ctx context.Context, q queryer, orgID, teamID, slug string) (db.TeamPromptRow, error) {
	var (
		id, name, body, model, tools string
		mod, del                     int
	)
	switch err := q.QueryRowContext(ctx, `
		SELECT id, name, body, model, allowed_tools, user_modified,
		       CASE WHEN deleted_at IS NULL THEN 0 ELSE 1 END
		FROM prompts WHERE org_id = ? AND team_id = ? AND system_slug = ?
	`, orgID, teamID, slug).Scan(&id, &name, &body, &model, &tools, &mod, &del); {
	case errors.Is(err, sql.ErrNoRows):
		return db.TeamPromptRow{Exists: false}, nil
	case err != nil:
		return db.TeamPromptRow{}, fmt.Errorf("probe prompt %s: %w", slug, err)
	}
	return db.TeamPromptRow{
		Exists: true, Deleted: del != 0, UserModified: mod != 0,
		ID: id, Name: name, Body: body, Model: model, AllowedTools: tools,
	}, nil
}

// executeUnitPlan writes a UnitInsert or UnitApply plan. Order matters: insert
// the header (insert only), then insert/update step prompts, then rewrite the
// step list, then soft-delete dropped prompts, then bump the header (apply only).
func executeUnitPlan(ctx context.Context, q queryer, orgID, teamID string, b domain.SeedBlueprint, st db.TeamUnitState, plan db.UnitSyncPlan) error {
	now := time.Now().UTC()
	blueprintID := st.BlueprintID

	if plan.Action == db.UnitInsert {
		blueprintID = uuid.New().String()
		if _, err := q.ExecContext(ctx, `
			INSERT INTO blueprints (id, org_id, team_id, system_slug, name, source, usage_count, user_modified, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'system', 0, 0, ?, ?)
		`, blueprintID, orgID, teamID, b.SystemSlug, plan.HeaderName, now, now); err != nil {
			return fmt.Errorf("insert blueprint header: %w", err)
		}
	}

	// resolved maps a shipped step slug to the prompt id the step wires to —
	// seeded from existing live rows, then extended by fresh inserts.
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
				INSERT INTO prompts (id, org_id, team_id, system_slug, name, body, source, allowed_tools, model, usage_count, user_modified, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, 'system', ?, ?, 0, 0, ?, ?)
			`, id, orgID, teamID, w.Slug, w.Name, w.Body, w.AllowedTools, w.Model, now, now); err != nil {
				return fmt.Errorf("insert step prompt %s: %w", w.Slug, err)
			}
			resolved[w.Slug] = id
		} else {
			if _, err := q.ExecContext(ctx, `
				UPDATE prompts SET name = ?, body = ?, model = ?, allowed_tools = ?, updated_at = ?
				WHERE org_id = ? AND id = ?
			`, w.Name, w.Body, w.Model, w.AllowedTools, now, orgID, w.PromptID); err != nil {
				return fmt.Errorf("update step prompt %s: %w", w.Slug, err)
			}
		}
	}

	if plan.Action == db.UnitInsert || plan.ReplaceSteps {
		if _, err := q.ExecContext(ctx, `DELETE FROM blueprint_steps WHERE blueprint_id = ?`, blueprintID); err != nil {
			return fmt.Errorf("clear steps: %w", err)
		}
		for i, slug := range plan.StepTargets {
			pid, ok := resolved[slug]
			if !ok || pid == "" {
				return fmt.Errorf("resolve step prompt for slug %q: not found", slug)
			}
			if _, err := q.ExecContext(ctx, `
				INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id, team_id, brief, created_at)
				VALUES (?, ?, ?, ?, '', ?)
			`, blueprintID, i, pid, teamID, now); err != nil {
				return fmt.Errorf("insert step %d: %w", i, err)
			}
		}
	}

	for _, pid := range plan.DropPromptIDs {
		if _, err := q.ExecContext(ctx,
			`UPDATE prompts SET deleted_at = ? WHERE org_id = ? AND id = ?`, now, orgID, pid,
		); err != nil {
			return fmt.Errorf("soft-delete dropped prompt %s: %w", pid, err)
		}
	}

	if plan.Action == db.UnitApply && (plan.RenameHeader || plan.ReplaceSteps) {
		if _, err := q.ExecContext(ctx,
			`UPDATE blueprints SET name = ?, updated_at = ? WHERE org_id = ? AND id = ?`,
			plan.HeaderName, now, orgID, blueprintID,
		); err != nil {
			return fmt.Errorf("update blueprint header: %w", err)
		}
	}
	return nil
}
