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

// orgTemplateStore is the SQLite impl of db.OrgTemplateStore.
//
// SQLite has one connection (no pool split): the seeder/materializer wrap their
// multi-statement work in a tx via the inTx helper, every other method runs on
// the single handle. The org template is a multi-mode concept — local mode
// never seeds or reads it — but the store is fully functional so the db-package
// bootstrap tests can exercise BootstrapNewOrg/NewTeam without Postgres.
// Methods filter by org_id structurally (like the event_handlers store) rather
// than asserting the local sentinel, so a test can drive any org id.
type orgTemplateStore struct {
	q queryer
}

func newOrgTemplateStore(q queryer) db.OrgTemplateStore {
	return &orgTemplateStore{q: q}
}

var _ db.OrgTemplateStore = (*orgTemplateStore)(nil)

const orgTemplatePromptColumns = `id, name, body, source, allowed_tools, model, system_slug, created_at, updated_at`

const orgTemplateHandlerColumns = `id, kind, event_type, scope_predicate_json, enabled, source, system_slug,
	name, default_priority, sort_order,
	blueprint_id, breaker_threshold, min_autonomy_suitability,
	created_at, updated_at`

const orgTemplateBlueprintColumns = `id, name, source, system_slug, created_at, updated_at`

// --- SeedFromShipped ------------------------------------------------

func (s *orgTemplateStore) SeedFromShipped(ctx context.Context, orgID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	now := time.Now().UTC()
	return inTx(ctx, s.q, func(q queryer) error {
		// Phase 1: prompts. INSERT OR IGNORE preserves admin edits across re-runs;
		// the follow-up SELECT resolves the (existing or fresh) id for the step +
		// trigger wiring.
		promptIDsBySlug := make(map[string]string, len(shippedPrompts))
		for _, p := range shippedPrompts {
			if p.SystemSlug == "" {
				return fmt.Errorf("org_template seed: shipped prompt %q has empty system_slug", p.Name)
			}
			if _, err := q.ExecContext(ctx, `
				INSERT OR IGNORE INTO org_template_prompts (id, org_id, system_slug, name, body, source, allowed_tools, model, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, 'system', ?, ?, ?, ?)
			`, uuid.New().String(), orgID, p.SystemSlug, p.Name, p.Body, p.AllowedTools, p.Model, now, now); err != nil {
				return fmt.Errorf("seed org_template prompt %s: %w", p.SystemSlug, err)
			}
			var id string
			if err := q.QueryRowContext(ctx,
				`SELECT id FROM org_template_prompts WHERE org_id = ? AND system_slug = ?`, orgID, p.SystemSlug,
			).Scan(&id); err != nil {
				return fmt.Errorf("resolve org_template prompt %s: %w", p.SystemSlug, err)
			}
			promptIDsBySlug[p.SystemSlug] = id
		}

		// Phase 2: blueprints + steps. Each shipped blueprint becomes a template
		// blueprint (source='system'); its steps re-point at the phase-1 prompts.
		// Steps are written only when the blueprint is freshly inserted so a
		// re-seed never clobbers an admin's edited step list.
		blueprintIDsBySlug := make(map[string]string, len(shippedBlueprints))
		for _, b := range shippedBlueprints {
			if b.SystemSlug == "" {
				return fmt.Errorf("org_template seed: shipped blueprint %q has empty system_slug", b.Name)
			}
			res, err := q.ExecContext(ctx, `
				INSERT OR IGNORE INTO org_template_blueprints (id, org_id, system_slug, name, source, created_at, updated_at)
				VALUES (?, ?, ?, ?, 'system', ?, ?)
			`, uuid.New().String(), orgID, b.SystemSlug, b.Name, now, now)
			if err != nil {
				return fmt.Errorf("seed org_template blueprint %s: %w", b.SystemSlug, err)
			}
			var bpID string
			if err := q.QueryRowContext(ctx,
				`SELECT id FROM org_template_blueprints WHERE org_id = ? AND system_slug = ?`, orgID, b.SystemSlug,
			).Scan(&bpID); err != nil {
				return fmt.Errorf("resolve org_template blueprint %s: %w", b.SystemSlug, err)
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
					return fmt.Errorf("seed org_template blueprint %s: step prompt slug %q not found (seed prompts before blueprints)", b.SystemSlug, slug)
				}
				if _, err := q.ExecContext(ctx, `
					INSERT INTO org_template_blueprint_steps (org_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
					VALUES (?, ?, ?, ?, '', ?)
				`, orgID, bpID, i, promptID, now); err != nil {
					return fmt.Errorf("seed org_template blueprint %s step %d: %w", b.SystemSlug, i, err)
				}
			}
		}

		// Phase 3: handlers. Rules seed enabled, triggers disabled (shipped
		// convention); a trigger's blueprint slug resolves to the phase-2
		// template blueprint id.
		for _, h := range db.ShippedEventHandlers {
			var pred any
			if h.Predicate != "" {
				pred = h.Predicate
			}
			switch h.Kind {
			case domain.EventHandlerKindRule:
				if _, err := q.ExecContext(ctx, `
					INSERT OR IGNORE INTO org_template_handlers
						(id, org_id, system_slug, kind, event_type, scope_predicate_json, enabled, source,
						 name, default_priority, sort_order, created_at, updated_at)
					VALUES (?, ?, ?, 'rule', ?, ?, 1, 'system', ?, ?, ?, ?, ?)
				`, uuid.New().String(), orgID, h.ID, h.EventType, pred, h.Name, h.DefaultPriority, h.SortOrder, now, now); err != nil {
					return fmt.Errorf("seed org_template rule %s: %w", h.ID, err)
				}
			case domain.EventHandlerKindTrigger:
				blueprintID, ok := blueprintIDsBySlug[h.BlueprintID]
				if !ok || blueprintID == "" {
					return fmt.Errorf("seed org_template trigger %s: blueprint slug %q not found (seed blueprints before handlers)", h.ID, h.BlueprintID)
				}
				if _, err := q.ExecContext(ctx, `
					INSERT OR IGNORE INTO org_template_handlers
						(id, org_id, system_slug, kind, event_type, scope_predicate_json, enabled, source,
						 blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
					VALUES (?, ?, ?, 'trigger', ?, ?, 0, 'system', ?, ?, ?, ?, ?)
				`, uuid.New().String(), orgID, h.ID, h.EventType, pred, blueprintID, h.BreakerThreshold, h.MinAutonomySuitability, now, now); err != nil {
					return fmt.Errorf("seed org_template trigger %s: %w", h.ID, err)
				}
			default:
				return fmt.Errorf("seed org_template handler %s: unknown kind %q", h.ID, h.Kind)
			}
		}
		return nil
	})
}

// --- MaterializeIntoTeam --------------------------------------------

func (s *orgTemplateStore) MaterializeIntoTeam(ctx context.Context, orgID, teamID string) error {
	if teamID == "" {
		return errors.New("sqlite org_template: MaterializeIntoTeam requires team_id")
	}
	now := time.Now().UTC()
	return inTx(ctx, s.q, func(q queryer) error {
		// Phase 1: prompts. SELECT-then-INSERT keeps it idempotent and yields
		// the team copy id for the trigger re-point map.
		rows, err := q.QueryContext(ctx, `
			SELECT id, system_slug, name, body, source, allowed_tools, model
			FROM org_template_prompts WHERE org_id = ?
		`, orgID)
		if err != nil {
			return fmt.Errorf("org_template materialize: list prompts: %w", err)
		}
		type tmplPrompt struct {
			id, slug, name, body, source, allowedTools, model string
		}
		var tprompts []tmplPrompt
		for rows.Next() {
			var p tmplPrompt
			if err := rows.Scan(&p.id, &p.slug, &p.name, &p.body, &p.source, &p.allowedTools, &p.model); err != nil {
				_ = rows.Close()
				return fmt.Errorf("org_template materialize: scan prompt: %w", err)
			}
			tprompts = append(tprompts, p)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()

		teamPromptIDByTemplateID := make(map[string]string, len(tprompts))
		for _, p := range tprompts {
			var existing string
			err := q.QueryRowContext(ctx,
				`SELECT id FROM prompts WHERE org_id = ? AND team_id = ? AND system_slug = ?`, orgID, teamID, p.slug,
			).Scan(&existing)
			if err == nil {
				teamPromptIDByTemplateID[p.id] = existing
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("org_template materialize: probe team prompt %s: %w", p.slug, err)
			}
			var creator any
			if p.source != "system" {
				creator = runmode.LocalDefaultUserID
			}
			newID := uuid.New().String()
			if _, err := q.ExecContext(ctx, `
				INSERT INTO prompts (id, org_id, team_id, system_slug, creator_user_id, name, body, source, allowed_tools, model, usage_count, user_modified, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
			`, newID, orgID, teamID, p.slug, creator, p.name, p.body, p.source, p.allowedTools, p.model, now, now); err != nil {
				return fmt.Errorf("org_template materialize: insert prompt %s: %w", p.slug, err)
			}
			teamPromptIDByTemplateID[p.id] = newID
			if p.source == "system" {
				hash := shippedContentHash(domain.Prompt{Name: p.name, Body: p.body, Source: p.source})
				if err := upsertSystemPromptVersionSQLite(ctx, q, newID, hash, now); err != nil {
					return err
				}
			}
		}

		// Phase 2: deep-copy template blueprints (+ their steps) into the team's
		// blueprints / blueprint_steps. Buffer the template rows first (SQLite is
		// single-connection: an open cursor can't coexist with the INSERTs below).
		type tmplBlueprint struct {
			id, slug, name, source string
		}
		brows, err := q.QueryContext(ctx, `
			SELECT id, system_slug, name, source FROM org_template_blueprints WHERE org_id = ?
		`, orgID)
		if err != nil {
			return fmt.Errorf("org_template materialize: list blueprints: %w", err)
		}
		var tblueprints []tmplBlueprint
		for brows.Next() {
			var b tmplBlueprint
			if err := brows.Scan(&b.id, &b.slug, &b.name, &b.source); err != nil {
				_ = brows.Close()
				return fmt.Errorf("org_template materialize: scan blueprint: %w", err)
			}
			tblueprints = append(tblueprints, b)
		}
		if err := brows.Err(); err != nil {
			_ = brows.Close()
			return err
		}
		_ = brows.Close()

		// Buffer template steps keyed by template blueprint id, in step order.
		type tmplStep struct {
			stepPromptID, brief string
		}
		srows, err := q.QueryContext(ctx, `
			SELECT blueprint_id, step_prompt_id, brief
			FROM org_template_blueprint_steps WHERE org_id = ?
			ORDER BY blueprint_id, step_index ASC
		`, orgID)
		if err != nil {
			return fmt.Errorf("org_template materialize: list blueprint steps: %w", err)
		}
		stepsByBlueprint := make(map[string][]tmplStep)
		for srows.Next() {
			var bid string
			var st tmplStep
			if err := srows.Scan(&bid, &st.stepPromptID, &st.brief); err != nil {
				_ = srows.Close()
				return fmt.Errorf("org_template materialize: scan blueprint step: %w", err)
			}
			stepsByBlueprint[bid] = append(stepsByBlueprint[bid], st)
		}
		if err := srows.Err(); err != nil {
			_ = srows.Close()
			return err
		}
		_ = srows.Close()

		// Probe by (org, team, slug) first; an existing team blueprint keeps its
		// steps untouched (idempotent re-run). A fresh copy gets the full step
		// list re-pointed at the team's prompt copies.
		teamBlueprintIDByTemplateID := make(map[string]string, len(tblueprints))
		for _, b := range tblueprints {
			var existing string
			err := q.QueryRowContext(ctx,
				`SELECT id FROM blueprints WHERE org_id = ? AND team_id = ? AND system_slug = ?`, orgID, teamID, b.slug,
			).Scan(&existing)
			if err == nil {
				teamBlueprintIDByTemplateID[b.id] = existing
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("org_template materialize: probe team blueprint %s: %w", b.slug, err)
			}
			var creator any
			if b.source != "system" {
				creator = runmode.LocalDefaultUserID
			}
			newID := uuid.New().String()
			if _, err := q.ExecContext(ctx, `
				INSERT INTO blueprints (id, org_id, team_id, system_slug, creator_user_id, name, source, usage_count, user_modified, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
			`, newID, orgID, teamID, b.slug, creator, b.name, b.source, now, now); err != nil {
				return fmt.Errorf("org_template materialize: insert blueprint %s: %w", b.slug, err)
			}
			for i, st := range stepsByBlueprint[b.id] {
				teamPromptID, ok := teamPromptIDByTemplateID[st.stepPromptID]
				if !ok || teamPromptID == "" {
					return fmt.Errorf("org_template materialize: blueprint %s step %d references a template prompt with no team copy", b.slug, i)
				}
				if _, err := q.ExecContext(ctx, `
					INSERT INTO blueprint_steps (blueprint_id, step_index, step_prompt_id, team_id, brief, created_at)
					VALUES (?, ?, ?, ?, ?, ?)
				`, newID, i, teamPromptID, teamID, st.brief, now); err != nil {
					return fmt.Errorf("org_template materialize: insert blueprint step %s/%d: %w", b.slug, i, err)
				}
			}
			teamBlueprintIDByTemplateID[b.id] = newID
		}

		// Phase 3: handlers. INSERT OR IGNORE for idempotency; re-point a
		// trigger's template blueprint to the team's blueprint copy and preserve
		// enabled + source verbatim.
		hrows, err := q.QueryContext(ctx, `
			SELECT system_slug, kind, event_type, scope_predicate_json, enabled, source,
			       name, default_priority, sort_order, blueprint_id, breaker_threshold, min_autonomy_suitability
			FROM org_template_handlers WHERE org_id = ?
		`, orgID)
		if err != nil {
			return fmt.Errorf("org_template materialize: list handlers: %w", err)
		}
		type tmplHandler struct {
			slug, kind, eventType, source string
			pred, name, blueprintID       sql.NullString
			enabled                       bool
			defPriority, minAutonomy      sql.NullFloat64
			sortOrder, breaker            sql.NullInt64
		}
		var thandlers []tmplHandler
		for hrows.Next() {
			var h tmplHandler
			if err := hrows.Scan(&h.slug, &h.kind, &h.eventType, &h.pred, &h.enabled, &h.source,
				&h.name, &h.defPriority, &h.sortOrder, &h.blueprintID, &h.breaker, &h.minAutonomy); err != nil {
				_ = hrows.Close()
				return fmt.Errorf("org_template materialize: scan handler: %w", err)
			}
			thandlers = append(thandlers, h)
		}
		if err := hrows.Err(); err != nil {
			_ = hrows.Close()
			return err
		}
		_ = hrows.Close()

		for _, h := range thandlers {
			var creator any
			if h.source != "system" {
				creator = runmode.LocalDefaultUserID
			}
			var predArg any
			if h.pred.Valid {
				predArg = h.pred.String
			}
			newID := uuid.New().String()
			switch h.kind {
			case domain.EventHandlerKindRule:
				if _, err := q.ExecContext(ctx, `
					INSERT OR IGNORE INTO event_handlers
						(id, org_id, team_id, creator_user_id, kind, event_type, system_slug,
						 scope_predicate_json, enabled, source, name, default_priority, sort_order, created_at, updated_at)
					VALUES (?, ?, ?, ?, 'rule', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				`, newID, orgID, teamID, creator, h.eventType, h.slug, predArg, h.enabled, h.source,
					h.name.String, h.defPriority.Float64, h.sortOrder.Int64, now, now); err != nil {
					return fmt.Errorf("org_template materialize: insert rule %s: %w", h.slug, err)
				}
			case domain.EventHandlerKindTrigger:
				teamBlueprintID, ok := teamBlueprintIDByTemplateID[h.blueprintID.String]
				if !ok || teamBlueprintID == "" {
					return fmt.Errorf("org_template materialize: trigger %s references template blueprint %q with no team copy", h.slug, h.blueprintID.String)
				}
				if _, err := q.ExecContext(ctx, `
					INSERT OR IGNORE INTO event_handlers
						(id, org_id, team_id, creator_user_id, kind, event_type, system_slug,
						 scope_predicate_json, enabled, source, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
					VALUES (?, ?, ?, ?, 'trigger', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				`, newID, orgID, teamID, creator, h.eventType, h.slug, predArg, h.enabled, h.source,
					teamBlueprintID, h.breaker.Int64, h.minAutonomy.Float64, now, now); err != nil {
					return fmt.Errorf("org_template materialize: insert trigger %s: %w", h.slug, err)
				}
			default:
				return fmt.Errorf("org_template materialize: handler %s unknown kind %q", h.slug, h.kind)
			}
		}
		return nil
	})
}

// --- prompts CRUD ---------------------------------------------------

func (s *orgTemplateStore) ListPrompts(ctx context.Context, orgID string) ([]domain.Prompt, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+orgTemplatePromptColumns+`
		FROM org_template_prompts WHERE org_id = ? ORDER BY updated_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Prompt
	for rows.Next() {
		p, err := scanOrgTemplatePrompt(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *orgTemplateStore) GetPrompt(ctx context.Context, orgID, id string) (*domain.Prompt, error) {
	p, err := scanOrgTemplatePrompt(s.q.QueryRowContext(ctx, `
		SELECT `+orgTemplatePromptColumns+`
		FROM org_template_prompts WHERE org_id = ? AND id = ?
	`, orgID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *orgTemplateStore) CreatePrompt(ctx context.Context, orgID string, p domain.Prompt) error {
	if p.SystemSlug == "" {
		return errors.New("sqlite org_template CreatePrompt: system_slug required")
	}
	source := p.Source
	if source == "" {
		source = "user"
	}
	now := time.Now().UTC()
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_template_prompts (id, org_id, system_slug, name, body, source, allowed_tools, model, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.ID, orgID, p.SystemSlug, p.Name, p.Body, source, p.AllowedTools, p.Model, now, now)
	return err
}

func (s *orgTemplateStore) UpdatePrompt(ctx context.Context, orgID, id, name, body, model string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_template_prompts SET name = ?, body = ?, model = ?, updated_at = ?
		WHERE org_id = ? AND id = ?
	`, name, body, model, time.Now().UTC(), orgID, id)
	return err
}

func (s *orgTemplateStore) DeletePrompt(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM org_template_prompts WHERE org_id = ? AND id = ?`, orgID, id)
	return err
}

func scanOrgTemplatePrompt(scanFn func(dst ...any) error) (domain.Prompt, error) {
	var p domain.Prompt
	var slug sql.NullString
	if err := scanFn(&p.ID, &p.Name, &p.Body, &p.Source, &p.AllowedTools, &p.Model, &slug, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return p, err
	}
	if slug.Valid {
		p.SystemSlug = slug.String
	}
	return p, nil
}

// --- blueprints CRUD ------------------------------------------------

func (s *orgTemplateStore) ListBlueprints(ctx context.Context, orgID string) ([]domain.Blueprint, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT `+orgTemplateBlueprintColumns+`
		FROM org_template_blueprints WHERE org_id = ? ORDER BY updated_at DESC
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Blueprint
	for rows.Next() {
		b, err := scanOrgTemplateBlueprint(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *orgTemplateStore) GetBlueprint(ctx context.Context, orgID, id string) (*domain.Blueprint, error) {
	b, err := scanOrgTemplateBlueprint(s.q.QueryRowContext(ctx, `
		SELECT `+orgTemplateBlueprintColumns+`
		FROM org_template_blueprints WHERE org_id = ? AND id = ?
	`, orgID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *orgTemplateStore) CreateBlueprint(ctx context.Context, orgID string, b domain.Blueprint) error {
	if b.SystemSlug == "" {
		return errors.New("sqlite org_template CreateBlueprint: system_slug required")
	}
	source := b.Source
	if source == "" {
		source = "user"
	}
	now := time.Now().UTC()
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_template_blueprints (id, org_id, system_slug, name, source, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, b.ID, orgID, b.SystemSlug, b.Name, source, now, now)
	return err
}

func (s *orgTemplateStore) UpdateBlueprint(ctx context.Context, orgID, id, name string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_template_blueprints SET name = ?, updated_at = ?
		WHERE org_id = ? AND id = ?
	`, name, time.Now().UTC(), orgID, id)
	return err
}

func (s *orgTemplateStore) DeleteBlueprint(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM org_template_blueprints WHERE org_id = ? AND id = ?`, orgID, id)
	return err
}

func (s *orgTemplateStore) ListBlueprintSteps(ctx context.Context, orgID, blueprintID string) ([]domain.BlueprintStep, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT blueprint_id, step_index, step_prompt_id, brief, created_at
		FROM org_template_blueprint_steps
		WHERE org_id = ? AND blueprint_id = ?
		ORDER BY step_index ASC
	`, orgID, blueprintID)
	if err != nil {
		return nil, fmt.Errorf("query org_template blueprint steps: %w", err)
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

func (s *orgTemplateStore) ListAllBlueprintSteps(ctx context.Context, orgID string) ([]domain.BlueprintStep, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT blueprint_id, step_index, step_prompt_id, brief, created_at
		FROM org_template_blueprint_steps
		WHERE org_id = ?
		ORDER BY blueprint_id, step_index ASC
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("query all org_template blueprint steps: %w", err)
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

func (s *orgTemplateStore) ReplaceBlueprintSteps(ctx context.Context, orgID, blueprintID string, stepPromptIDs, briefs []string) error {
	if len(briefs) != 0 && len(briefs) != len(stepPromptIDs) {
		return fmt.Errorf("briefs length %d must match stepPromptIDs length %d", len(briefs), len(stepPromptIDs))
	}
	return inTx(ctx, s.q, func(q queryer) error {
		if _, err := q.ExecContext(ctx,
			`DELETE FROM org_template_blueprint_steps WHERE org_id = ? AND blueprint_id = ?`,
			orgID, blueprintID); err != nil {
			return fmt.Errorf("clear existing template steps: %w", err)
		}
		now := time.Now().UTC()
		for i, stepID := range stepPromptIDs {
			brief := ""
			if i < len(briefs) {
				brief = briefs[i]
			}
			if _, err := q.ExecContext(ctx, `
				INSERT INTO org_template_blueprint_steps (org_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
				VALUES (?, ?, ?, ?, ?, ?)
			`, orgID, blueprintID, i, stepID, brief, now); err != nil {
				return fmt.Errorf("insert template step %d: %w", i, err)
			}
		}
		return nil
	})
}

func (s *orgTemplateStore) MergeBlueprints(ctx context.Context, orgID, hostID, sourceID string) error {
	return inTx(ctx, s.q, func(q queryer) error {
		var hostLen int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM org_template_blueprint_steps WHERE org_id = ? AND blueprint_id = ?`,
			orgID, hostID).Scan(&hostLen); err != nil {
			return fmt.Errorf("count host template steps: %w", err)
		}
		// Append source's steps after the host's, densely (host_len + i) — a
		// range disjoint from the host's, so no PK collision; step_prompt_id is
		// untouched so the copy-only unique index holds.
		if _, err := q.ExecContext(ctx, `
			UPDATE org_template_blueprint_steps
			SET blueprint_id = ?, step_index = step_index + ?
			WHERE org_id = ? AND blueprint_id = ?
		`, hostID, hostLen, orgID, sourceID); err != nil {
			return fmt.Errorf("reparent source template steps: %w", err)
		}
		// Hard-delete the now-stepless source (template mirror — no soft-delete).
		res, err := q.ExecContext(ctx,
			`DELETE FROM org_template_blueprints WHERE org_id = ? AND id = ?`, orgID, sourceID)
		if err != nil {
			return fmt.Errorf("delete source template blueprint: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("template blueprint %s not found", sourceID)
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE org_template_blueprints SET updated_at = ? WHERE org_id = ? AND id = ?`,
			time.Now().UTC(), orgID, hostID); err != nil {
			return fmt.Errorf("bump host updated_at: %w", err)
		}
		return nil
	})
}

func (s *orgTemplateStore) SplitBlueprint(ctx context.Context, orgID, id string, atIndex int, newBlueprintID, newSlug, newName string) (string, error) {
	err := inTx(ctx, s.q, func(q queryer) error {
		now := time.Now().UTC()
		if _, err := q.ExecContext(ctx, `
			INSERT INTO org_template_blueprints (id, org_id, system_slug, name, source, created_at, updated_at)
			VALUES (?, ?, ?, ?, 'user', ?, ?)
		`, newBlueprintID, orgID, newSlug, newName, now, now); err != nil {
			return fmt.Errorf("create downstream template blueprint: %w", err)
		}
		// Peel steps [atIndex, N) onto the new blueprint, re-densified 0-based.
		if _, err := q.ExecContext(ctx, `
			UPDATE org_template_blueprint_steps
			SET blueprint_id = ?, step_index = step_index - ?
			WHERE org_id = ? AND blueprint_id = ? AND step_index >= ?
		`, newBlueprintID, atIndex, orgID, id, atIndex); err != nil {
			return fmt.Errorf("reparent tail template steps: %w", err)
		}
		if _, err := q.ExecContext(ctx,
			`UPDATE org_template_blueprints SET updated_at = ? WHERE org_id = ? AND id = ?`, now, orgID, id); err != nil {
			return fmt.Errorf("bump upstream updated_at: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return newBlueprintID, nil
}

func (s *orgTemplateStore) CountBlueprintStepReferences(ctx context.Context, orgID, stepPromptID string) (int, error) {
	var n int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT blueprint_id)
		FROM org_template_blueprint_steps
		WHERE org_id = ? AND step_prompt_id = ?
	`, orgID, stepPromptID).Scan(&n)
	return n, err
}

func (s *orgTemplateStore) BlueprintStepPromptOwner(ctx context.Context, orgID, stepPromptID string) (string, bool, error) {
	var blueprintID string
	err := s.q.QueryRowContext(ctx,
		`SELECT blueprint_id FROM org_template_blueprint_steps WHERE org_id = ? AND step_prompt_id = ? LIMIT 1`,
		orgID, stepPromptID,
	).Scan(&blueprintID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return blueprintID, true, nil
}

func (s *orgTemplateStore) DuplicateBlueprintPrompts(ctx context.Context, orgID string, promptIDs []string) ([]string, error) {
	ids := db.DedupPreserveOrder(promptIDs)
	if len(ids) == 0 {
		return nil, db.ErrDuplicateNoPrompts
	}
	var newIDs []string
	err := inTx(ctx, s.q, func(q queryer) error {
		resolved := make([]db.DuplicationStep, 0, len(ids))
		for _, pid := range ids {
			var st db.DuplicationStep
			err := q.QueryRowContext(ctx, `
				SELECT bs.blueprint_id, b.name, bs.step_index, bs.brief,
				       p.name, p.body, p.model, p.allowed_tools,
				       (SELECT COUNT(*) FROM org_template_blueprint_steps bs2
				        WHERE bs2.org_id = ? AND bs2.blueprint_id = bs.blueprint_id)
				FROM org_template_blueprint_steps bs
				JOIN org_template_blueprints b ON b.id = bs.blueprint_id AND b.org_id = bs.org_id
				JOIN org_template_prompts p ON p.id = bs.step_prompt_id AND p.org_id = bs.org_id
				WHERE bs.org_id = ? AND bs.step_prompt_id = ?
			`, orgID, orgID, pid).Scan(&st.BlueprintID, &st.BlueprintName, &st.StepIndex, &st.Brief,
				&st.PromptName, &st.PromptBody, &st.PromptModel, &st.PromptTools, &st.BlueprintTotal)
			if errors.Is(err, sql.ErrNoRows) {
				return db.ErrDuplicatePromptNotFound
			}
			if err != nil {
				return fmt.Errorf("resolve template prompt %s: %w", pid, err)
			}
			resolved = append(resolved, st)
		}

		now := time.Now().UTC()
		for _, run := range db.PartitionDuplicationRuns(resolved) {
			bpID := uuid.New().String()
			if _, err := q.ExecContext(ctx, `
				INSERT INTO org_template_blueprints (id, org_id, system_slug, name, source, created_at, updated_at)
				VALUES (?, ?, ?, ?, 'user', ?, ?)
			`, bpID, orgID, "tmpl-"+uuid.New().String(), run.Name, now, now); err != nil {
				return fmt.Errorf("create duplicated template blueprint: %w", err)
			}
			for i, step := range run.Steps {
				newPromptID := uuid.New().String()
				if _, err := q.ExecContext(ctx, `
					INSERT INTO org_template_prompts (id, org_id, system_slug, name, body, source, allowed_tools, model, created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, 'user', ?, ?, ?, ?)
				`, newPromptID, orgID, "tmpl-"+uuid.New().String(), step.PromptName, step.PromptBody,
					step.PromptTools, step.PromptModel, now, now); err != nil {
					return fmt.Errorf("copy template prompt: %w", err)
				}
				if _, err := q.ExecContext(ctx, `
					INSERT INTO org_template_blueprint_steps (org_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
					VALUES (?, ?, ?, ?, ?, ?)
				`, orgID, bpID, i, newPromptID, step.Brief, now); err != nil {
					return fmt.Errorf("insert duplicated template step %d: %w", i, err)
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

func scanOrgTemplateBlueprint(scanFn func(dst ...any) error) (domain.Blueprint, error) {
	var b domain.Blueprint
	var slug sql.NullString
	if err := scanFn(&b.ID, &b.Name, &b.Source, &slug, &b.CreatedAt, &b.UpdatedAt); err != nil {
		return b, err
	}
	if slug.Valid {
		b.SystemSlug = slug.String
	}
	return b, nil
}

// --- handlers CRUD --------------------------------------------------

func (s *orgTemplateStore) ListHandlers(ctx context.Context, orgID, kind string) ([]domain.EventHandler, error) {
	q := `SELECT ` + orgTemplateHandlerColumns + ` FROM org_template_handlers WHERE org_id = ?`
	args := []any{orgID}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += `
		ORDER BY kind ASC,
		         CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
		         CASE WHEN kind = 'rule' THEN name ELSE '' END ASC,
		         created_at DESC`
	rows, err := s.q.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.EventHandler
	for rows.Next() {
		h, err := scanOrgTemplateHandler(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *orgTemplateStore) GetHandler(ctx context.Context, orgID, id string) (*domain.EventHandler, error) {
	h, err := scanOrgTemplateHandler(s.q.QueryRowContext(ctx, `
		SELECT `+orgTemplateHandlerColumns+` FROM org_template_handlers WHERE org_id = ? AND id = ?
	`, orgID, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &h, nil
}

func (s *orgTemplateStore) CreateHandler(ctx context.Context, orgID string, h domain.EventHandler) error {
	if h.SystemSlug == "" {
		return errors.New("sqlite org_template CreateHandler: system_slug required")
	}
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	now := time.Now().UTC()
	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.q.ExecContext(ctx, `
			INSERT INTO org_template_handlers
				(id, org_id, system_slug, kind, event_type, scope_predicate_json, enabled, source,
				 name, default_priority, sort_order, created_at, updated_at)
			VALUES (?, ?, ?, 'rule', ?, ?, ?, 'user', ?, ?, ?, ?, ?)
		`, h.ID, orgID, h.SystemSlug, h.EventType, pred, h.Enabled, h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder), now, now)
		return err
	case domain.EventHandlerKindTrigger:
		_, err := s.q.ExecContext(ctx, `
			INSERT INTO org_template_handlers
				(id, org_id, system_slug, kind, event_type, scope_predicate_json, enabled, source,
				 blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
			VALUES (?, ?, ?, 'trigger', ?, ?, ?, 'user', ?, ?, ?, ?, ?)
		`, h.ID, orgID, h.SystemSlug, h.EventType, pred, h.Enabled, h.BlueprintID, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability), now, now)
		return err
	}
	return fmt.Errorf("sqlite org_template CreateHandler: unknown kind %q", h.Kind)
}

func (s *orgTemplateStore) UpdateHandler(ctx context.Context, orgID string, h domain.EventHandler) (bool, error) {
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return false, err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	now := time.Now().UTC()
	// The WHERE pins the row's kind, so a concurrent promote (rule→trigger)
	// between the caller's read and this write makes the UPDATE match 0 rows
	// rather than silently clobbering — the bool surfaces that so the handler
	// can 409 instead of reporting a misleading success.
	var res sql.Result
	var err error
	switch h.Kind {
	case domain.EventHandlerKindRule:
		res, err = s.q.ExecContext(ctx, `
			UPDATE org_template_handlers
			SET scope_predicate_json = ?, enabled = ?, name = ?, default_priority = ?, sort_order = ?, updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'rule'
		`, pred, h.Enabled, h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder), now, orgID, h.ID)
	case domain.EventHandlerKindTrigger:
		res, err = s.q.ExecContext(ctx, `
			UPDATE org_template_handlers
			SET scope_predicate_json = ?, enabled = ?, breaker_threshold = ?, min_autonomy_suitability = ?, updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'trigger'
		`, pred, h.Enabled, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability), now, orgID, h.ID)
	default:
		return false, fmt.Errorf("sqlite org_template UpdateHandler: unknown kind %q", h.Kind)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *orgTemplateStore) SetHandlerEnabled(ctx context.Context, orgID, id string, enabled bool) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_template_handlers SET enabled = ?, updated_at = ? WHERE org_id = ? AND id = ?
	`, enabled, time.Now().UTC(), orgID, id)
	return err
}

func (s *orgTemplateStore) DeleteHandler(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM org_template_handlers WHERE org_id = ? AND id = ?`, orgID, id)
	return err
}

func (s *orgTemplateStore) RetargetHandlerBlueprint(ctx context.Context, orgID, id, newBlueprintID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_template_handlers SET blueprint_id = ?, updated_at = ?
		WHERE org_id = ? AND id = ? AND kind = 'trigger'
	`, newBlueprintID, time.Now().UTC(), orgID, id)
	return err
}

func (s *orgTemplateStore) PromoteHandler(ctx context.Context, orgID, id string, t domain.EventHandler) error {
	if t.Kind != domain.EventHandlerKindTrigger {
		return errors.New("sqlite org_template PromoteHandler: target kind must be 'trigger'")
	}
	if t.BlueprintID == "" || t.BreakerThreshold == nil || t.MinAutonomySuitability == nil {
		return errors.New("sqlite org_template PromoteHandler: trigger fields required")
	}
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE org_template_handlers
		SET kind = 'trigger', blueprint_id = ?, breaker_threshold = ?, min_autonomy_suitability = ?,
		    name = NULL, default_priority = NULL, sort_order = NULL, scope_predicate_json = ?, updated_at = ?
		WHERE org_id = ? AND id = ? AND kind = 'rule'
	`, t.BlueprintID, *t.BreakerThreshold, *t.MinAutonomySuitability, pred, time.Now().UTC(), orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("sqlite org_template PromoteHandler: row not found or not a rule")
	}
	return nil
}

func (s *orgTemplateStore) ReorderHandlers(ctx context.Context, orgID string, ids []string) error {
	return inTx(ctx, s.q, func(q queryer) error {
		now := time.Now().UTC()
		for i, id := range ids {
			if _, err := q.ExecContext(ctx, `
				UPDATE org_template_handlers SET sort_order = ?, updated_at = ?
				WHERE org_id = ? AND id = ? AND kind = 'rule'
			`, i, now, orgID, id); err != nil {
				return err
			}
		}
		return nil
	})
}

func scanOrgTemplateHandler(scanFn func(dst ...any) error) (domain.EventHandler, error) {
	var h domain.EventHandler
	var (
		pred          sql.NullString
		slug          sql.NullString
		nameNS        sql.NullString
		defPriority   sql.NullFloat64
		sortOrder     sql.NullInt64
		blueprintID   sql.NullString
		breakerNS     sql.NullInt64
		minAutonomyNS sql.NullFloat64
	)
	if err := scanFn(
		&h.ID, &h.Kind, &h.EventType, &pred, &h.Enabled, &h.Source, &slug,
		&nameNS, &defPriority, &sortOrder,
		&blueprintID, &breakerNS, &minAutonomyNS,
		&h.CreatedAt, &h.UpdatedAt,
	); err != nil {
		return h, err
	}
	if slug.Valid {
		h.SystemSlug = slug.String
	}
	if pred.Valid {
		v := pred.String
		h.ScopePredicateJSON = &v
	}
	if nameNS.Valid {
		h.Name = nameNS.String
	}
	if defPriority.Valid {
		v := defPriority.Float64
		h.DefaultPriority = &v
	}
	if sortOrder.Valid {
		v := int(sortOrder.Int64)
		h.SortOrder = &v
	}
	if blueprintID.Valid {
		h.BlueprintID = blueprintID.String
	}
	if breakerNS.Valid {
		v := int(breakerNS.Int64)
		h.BreakerThreshold = &v
	}
	if minAutonomyNS.Valid {
		v := minAutonomyNS.Float64
		h.MinAutonomySuitability = &v
	}
	if h.Kind == domain.EventHandlerKindTrigger {
		h.TriggerType = domain.TriggerTypeEvent
	}
	return h, nil
}
