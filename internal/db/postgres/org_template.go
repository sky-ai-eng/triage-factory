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

// orgTemplateStore is the Postgres impl of db.OrgTemplateStore.
//
// # Pool split
//
//   - app   — tf_app, RLS-active. Every CRUD method runs here; the
//     org_template_*_all policies gate each row on tf.user_is_org_admin.
//   - admin — supabase_admin, BYPASSRLS. SeedFromShipped + MaterializeIntoTeam
//     run here (claims-less bootstrap; MaterializeIntoTeam also writes the
//     team's prompts/event_handlers/system_prompt_versions).
//
// Inside WithTx both fields point at the same *sql.Tx and inTx is true; the two
// admin operations refuse to run there (same guard as PromptStore.SeedOrUpdate).
type orgTemplateStore struct {
	app   queryer
	admin queryer
	inTx  bool
}

func newOrgTemplateStore(app, admin queryer) db.OrgTemplateStore {
	return &orgTemplateStore{app: app, admin: admin}
}

func newTxOrgTemplateStore(tx queryer) db.OrgTemplateStore {
	return &orgTemplateStore{app: tx, admin: tx, inTx: true}
}

var _ db.OrgTemplateStore = (*orgTemplateStore)(nil)

const orgTemplatePromptColumns = `id, name, body, source, allowed_tools, model, system_slug, created_at, updated_at`

const orgTemplateHandlerColumns = `id, kind, event_type, scope_predicate_json::text, enabled, source, system_slug,
	name, default_priority, sort_order,
	blueprint_id, breaker_threshold, min_autonomy_suitability,
	created_at, updated_at`

const orgTemplateBlueprintColumns = `id, name, source, system_slug, created_at, updated_at`

// --- SeedFromShipped ------------------------------------------------

func (s *orgTemplateStore) SeedFromShipped(ctx context.Context, orgID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error {
	if s.inTx {
		return errors.New("postgres org_template: SeedFromShipped must not be called inside WithTx; call stores.OrgTemplate.SeedFromShipped directly")
	}
	conn, ok := s.admin.(*sql.DB)
	if !ok {
		return fmt.Errorf("postgres org_template: SeedFromShipped requires a *sql.DB admin handle, got %T", s.admin)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin admin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()
	// Phase 1: prompts. ON CONFLICT DO NOTHING keeps admin edits + deletes
	// across re-runs; the follow-up SELECT resolves the (existing or freshly
	// inserted) id so steps + triggers can wire to it.
	promptIDsBySlug := make(map[string]string, len(shippedPrompts))
	for _, p := range shippedPrompts {
		if p.SystemSlug == "" {
			return fmt.Errorf("org_template seed: shipped prompt %q has empty system_slug", p.Name)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO org_template_prompts (id, org_id, system_slug, name, body, source, allowed_tools, model, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'system', $6, $7, $8, $8)
			ON CONFLICT (org_id, system_slug) DO NOTHING
		`, uuid.New().String(), orgID, p.SystemSlug, p.Name, p.Body, p.AllowedTools, p.Model, now); err != nil {
			return fmt.Errorf("seed org_template prompt %s: %w", p.SystemSlug, err)
		}
		var id string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM org_template_prompts WHERE org_id = $1 AND system_slug = $2`, orgID, p.SystemSlug,
		).Scan(&id); err != nil {
			return fmt.Errorf("resolve org_template prompt %s: %w", p.SystemSlug, err)
		}
		promptIDsBySlug[p.SystemSlug] = id
	}

	// Phase 2: blueprints + steps. Each shipped blueprint becomes a template
	// blueprint (source='system'); its steps re-point at the phase-1 prompts.
	// Steps are written only when the blueprint is freshly inserted (RowsAffected
	// > 0) so a re-seed never clobbers an admin's edited step list.
	blueprintIDsBySlug := make(map[string]string, len(shippedBlueprints))
	for _, b := range shippedBlueprints {
		if b.SystemSlug == "" {
			return fmt.Errorf("org_template seed: shipped blueprint %q has empty system_slug", b.Name)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO org_template_blueprints (id, org_id, system_slug, name, source, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'system', $5, $5)
			ON CONFLICT (org_id, system_slug) DO NOTHING
		`, uuid.New().String(), orgID, b.SystemSlug, b.Name, now)
		if err != nil {
			return fmt.Errorf("seed org_template blueprint %s: %w", b.SystemSlug, err)
		}
		var bpID string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM org_template_blueprints WHERE org_id = $1 AND system_slug = $2`, orgID, b.SystemSlug,
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
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO org_template_blueprint_steps (org_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
				VALUES ($1, $2, $3, $4, '', $5)
			`, orgID, bpID, i, promptID, now); err != nil {
				return fmt.Errorf("seed org_template blueprint %s step %d: %w", b.SystemSlug, i, err)
			}
		}
	}

	// Phase 3: handlers. Rules seed enabled, triggers disabled (shipped
	// convention); a trigger's blueprint slug resolves to the phase-2 template
	// blueprint id.
	for _, h := range db.ShippedEventHandlers {
		var pred any
		if h.Predicate != "" {
			pred = h.Predicate
		}
		switch h.Kind {
		case domain.EventHandlerKindRule:
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO org_template_handlers
					(id, org_id, system_slug, kind, event_type, scope_predicate_json, enabled, source,
					 name, default_priority, sort_order, created_at, updated_at)
				VALUES ($1, $2, $3, 'rule', $4, $5::jsonb, TRUE, 'system', $6, $7, $8, $9, $9)
				ON CONFLICT (org_id, system_slug) DO NOTHING
			`, uuid.New().String(), orgID, h.ID, h.EventType, pred, h.Name, h.DefaultPriority, h.SortOrder, now); err != nil {
				return fmt.Errorf("seed org_template rule %s: %w", h.ID, err)
			}
		case domain.EventHandlerKindTrigger:
			blueprintID, ok := blueprintIDsBySlug[h.BlueprintID]
			if !ok || blueprintID == "" {
				return fmt.Errorf("seed org_template trigger %s: blueprint slug %q not found (seed blueprints before handlers)", h.ID, h.BlueprintID)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO org_template_handlers
					(id, org_id, system_slug, kind, event_type, scope_predicate_json, enabled, source,
					 blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
				VALUES ($1, $2, $3, 'trigger', $4, $5::jsonb, FALSE, 'system', $6, $7, $8, $9, $9)
				ON CONFLICT (org_id, system_slug) DO NOTHING
			`, uuid.New().String(), orgID, h.ID, h.EventType, pred, blueprintID, h.BreakerThreshold, h.MinAutonomySuitability, now); err != nil {
				return fmt.Errorf("seed org_template trigger %s: %w", h.ID, err)
			}
		default:
			return fmt.Errorf("seed org_template handler %s: unknown kind %q", h.ID, h.Kind)
		}
	}
	return tx.Commit()
}

// --- MaterializeIntoTeam --------------------------------------------

func (s *orgTemplateStore) MaterializeIntoTeam(ctx context.Context, orgID, teamID string) error {
	if s.inTx {
		return errors.New("postgres org_template: MaterializeIntoTeam must not be called inside WithTx; call stores.OrgTemplate.MaterializeIntoTeam directly")
	}
	if teamID == "" {
		return errors.New("postgres org_template: MaterializeIntoTeam requires team_id")
	}
	conn, ok := s.admin.(*sql.DB)
	if !ok {
		return fmt.Errorf("postgres org_template: MaterializeIntoTeam requires a *sql.DB admin handle, got %T", s.admin)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin admin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	// owner is the creator stamped on user-source copies (admin pool has no
	// current user). System-source copies carry NULL (prompts/handlers
	// *_system_has_no_creator CHECK). NULL owner is fine — only used for
	// user rows, and an org always has an owner by the time it has a template.
	var owner sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT owner_user_id FROM orgs WHERE id = $1`, orgID).Scan(&owner); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("org_template materialize: read org owner: %w", err)
	}

	// Phase 1: prompts. Map each template prompt id → the team's copy id so
	// triggers can re-point. Idempotent on (org_id, team_id, system_slug).
	rows, err := tx.QueryContext(ctx, `
		SELECT id, system_slug, name, body, source, allowed_tools, model
		FROM org_template_prompts WHERE org_id = $1
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
		var creator any
		if p.source != "system" {
			if !owner.Valid {
				return fmt.Errorf("org_template materialize: user-source template prompt %s but org %s has no owner", p.slug, orgID)
			}
			creator = owner.String
		}
		newID := uuid.New().String()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO prompts (id, org_id, team_id, system_slug, creator_user_id, name, body, source, allowed_tools, model, usage_count, user_modified, created_at, updated_at)
			VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, $8, $9, $10, 0, FALSE, $11, $11)
			ON CONFLICT (org_id, team_id, system_slug) DO NOTHING
		`, newID, orgID, teamID, p.slug, creator, p.name, p.body, p.source, p.allowedTools, p.model, now); err != nil {
			return fmt.Errorf("org_template materialize: insert prompt %s: %w", p.slug, err)
		}
		var teamPromptID string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM prompts WHERE org_id = $1 AND team_id = $2 AND system_slug = $3`, orgID, teamID, p.slug,
		).Scan(&teamPromptID); err != nil {
			return fmt.Errorf("org_template materialize: resolve team prompt %s: %w", p.slug, err)
		}
		teamPromptIDByTemplateID[p.id] = teamPromptID

		// system_prompt_versions sidecar for system rows (mirrors
		// PromptStore.SeedOrUpdate so a re-seed stays a no-op).
		if p.source == "system" {
			hash := shippedContentHash(domain.Prompt{Name: p.name, Body: p.body, Source: p.source})
			if err := upsertSystemPromptVersionPG(ctx, tx, orgID, teamPromptID, hash, now); err != nil {
				return err
			}
		}
	}

	// Phase 2: deep-copy template blueprints (+ their steps) into the team's
	// blueprints / blueprint_steps. Buffer the template rows first: the INSERTs
	// below run on the same tx connection and can't be issued mid-cursor.
	type tmplBlueprint struct {
		id, slug, name, source string
	}
	brows, err := tx.QueryContext(ctx, `
		SELECT id, system_slug, name, source FROM org_template_blueprints WHERE org_id = $1
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

	type tmplStep struct {
		stepPromptID, brief string
	}
	srows, err := tx.QueryContext(ctx, `
		SELECT blueprint_id, step_prompt_id, brief
		FROM org_template_blueprint_steps WHERE org_id = $1
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

	teamBlueprintIDByTemplateID := make(map[string]string, len(tblueprints))
	for _, b := range tblueprints {
		var creator any
		if b.source != domain.EventHandlerSourceSystem {
			if !owner.Valid {
				return fmt.Errorf("org_template materialize: user-source template blueprint %s but org %s has no owner", b.slug, orgID)
			}
			creator = owner.String
		}
		newID := uuid.New().String()
		res, err := tx.ExecContext(ctx, `
			INSERT INTO blueprints (id, org_id, team_id, system_slug, creator_user_id, name, source, usage_count, user_modified, created_at, updated_at)
			VALUES ($1, $2, $3::uuid, $4, $5, $6, $7, 0, FALSE, $8, $8)
			ON CONFLICT (org_id, team_id, system_slug) DO NOTHING
		`, newID, orgID, teamID, b.slug, creator, b.name, b.source, now)
		if err != nil {
			return fmt.Errorf("org_template materialize: insert blueprint %s: %w", b.slug, err)
		}
		var teamBlueprintID string
		if err := tx.QueryRowContext(ctx,
			`SELECT id FROM blueprints WHERE org_id = $1 AND team_id = $2 AND system_slug = $3`, orgID, teamID, b.slug,
		).Scan(&teamBlueprintID); err != nil {
			return fmt.Errorf("org_template materialize: resolve team blueprint %s: %w", b.slug, err)
		}
		teamBlueprintIDByTemplateID[b.id] = teamBlueprintID

		// Write steps only when the blueprint header was freshly inserted; an
		// existing team blueprint keeps its steps untouched, so a bootstrap
		// re-run never re-adds a step the team intentionally removed. Matches the
		// SQLite probe-and-skip path and the documented idempotency contract.
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		for i, st := range stepsByBlueprint[b.id] {
			teamPromptID, ok := teamPromptIDByTemplateID[st.stepPromptID]
			if !ok || teamPromptID == "" {
				return fmt.Errorf("org_template materialize: blueprint %s step %d references a template prompt with no team copy", b.slug, i)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO blueprint_steps (org_id, team_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
				VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)
			`, orgID, teamID, teamBlueprintID, i, teamPromptID, st.brief, now); err != nil {
				return fmt.Errorf("org_template materialize: insert blueprint step %s/%d: %w", b.slug, i, err)
			}
		}
	}

	// Phase 3: handlers. Re-point a trigger's template blueprint to the team's
	// blueprint copy and preserve enabled + source verbatim. Idempotent on
	// (org, team, slug). Buffer the rows first: the INSERTs below run on the
	// same tx connection, so they can't be issued while hrows is still streaming.
	hrows, err := tx.QueryContext(ctx, `
		SELECT system_slug, kind, event_type, scope_predicate_json::text, enabled, source,
		       name, default_priority, sort_order, blueprint_id, breaker_threshold, min_autonomy_suitability
		FROM org_template_handlers WHERE org_id = $1
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
		if h.source != domain.EventHandlerSourceSystem {
			if !owner.Valid {
				return fmt.Errorf("org_template materialize: user-source template handler %s but org %s has no owner", h.slug, orgID)
			}
			creator = owner.String
		}
		var predArg any
		if h.pred.Valid {
			predArg = h.pred.String
		}
		newID := uuid.New().String()
		switch h.kind {
		case domain.EventHandlerKindRule:
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO event_handlers
					(id, org_id, team_id, creator_user_id, kind, event_type, system_slug,
					 scope_predicate_json, enabled, source, name, default_priority, sort_order, created_at, updated_at)
				VALUES ($1, $2, $3::uuid, $4, 'rule', $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $13)
				ON CONFLICT (org_id, team_id, system_slug) DO NOTHING
			`, newID, orgID, teamID, creator, h.eventType, h.slug, predArg, h.enabled, h.source,
				h.name.String, h.defPriority.Float64, h.sortOrder.Int64, now); err != nil {
				return fmt.Errorf("org_template materialize: insert rule %s: %w", h.slug, err)
			}
		case domain.EventHandlerKindTrigger:
			teamBlueprintID, ok := teamBlueprintIDByTemplateID[h.blueprintID.String]
			if !ok || teamBlueprintID == "" {
				return fmt.Errorf("org_template materialize: trigger %s references template blueprint %q with no team copy", h.slug, h.blueprintID.String)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO event_handlers
					(id, org_id, team_id, creator_user_id, kind, event_type, system_slug,
					 scope_predicate_json, enabled, source, blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
				VALUES ($1, $2, $3::uuid, $4, 'trigger', $5, $6, $7::jsonb, $8, $9, $10, $11, $12, $13, $13)
				ON CONFLICT (org_id, team_id, system_slug) DO NOTHING
			`, newID, orgID, teamID, creator, h.eventType, h.slug, predArg, h.enabled, h.source,
				teamBlueprintID, h.breaker.Int64, h.minAutonomy.Float64, now); err != nil {
				return fmt.Errorf("org_template materialize: insert trigger %s: %w", h.slug, err)
			}
		default:
			return fmt.Errorf("org_template materialize: handler %s unknown kind %q", h.slug, h.kind)
		}
	}
	return tx.Commit()
}

// --- prompts CRUD ---------------------------------------------------

func (s *orgTemplateStore) ListPrompts(ctx context.Context, orgID string) ([]domain.Prompt, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+orgTemplatePromptColumns+`
		FROM org_template_prompts WHERE org_id = $1 ORDER BY updated_at DESC
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
	p, err := scanOrgTemplatePrompt(s.app.QueryRowContext(ctx, `
		SELECT `+orgTemplatePromptColumns+`
		FROM org_template_prompts WHERE org_id = $1 AND id = $2
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
		return errors.New("postgres org_template CreatePrompt: system_slug required")
	}
	source := p.Source
	if source == "" {
		source = "user"
	}
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO org_template_prompts (id, org_id, system_slug, name, body, source, allowed_tools, model, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, now(), now())
	`, p.ID, orgID, p.SystemSlug, p.Name, p.Body, source, p.AllowedTools, p.Model)
	return err
}

func (s *orgTemplateStore) UpdatePrompt(ctx context.Context, orgID, id, name, body, model string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE org_template_prompts SET name = $1, body = $2, model = $3, updated_at = now()
		WHERE org_id = $4 AND id = $5
	`, name, body, model, orgID, id)
	return err
}

func (s *orgTemplateStore) DeletePrompt(ctx context.Context, orgID, id string) error {
	_, err := s.app.ExecContext(ctx, `DELETE FROM org_template_prompts WHERE org_id = $1 AND id = $2`, orgID, id)
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
	rows, err := s.app.QueryContext(ctx, `
		SELECT `+orgTemplateBlueprintColumns+`
		FROM org_template_blueprints WHERE org_id = $1 ORDER BY updated_at DESC
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
	b, err := scanOrgTemplateBlueprint(s.app.QueryRowContext(ctx, `
		SELECT `+orgTemplateBlueprintColumns+`
		FROM org_template_blueprints WHERE org_id = $1 AND id = $2
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
		return errors.New("postgres org_template CreateBlueprint: system_slug required")
	}
	source := b.Source
	if source == "" {
		source = "user"
	}
	_, err := s.app.ExecContext(ctx, `
		INSERT INTO org_template_blueprints (id, org_id, system_slug, name, source, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now(), now())
	`, b.ID, orgID, b.SystemSlug, b.Name, source)
	return err
}

func (s *orgTemplateStore) UpdateBlueprint(ctx context.Context, orgID, id, name string) error {
	_, err := s.app.ExecContext(ctx, `
		UPDATE org_template_blueprints SET name = $1, updated_at = now()
		WHERE org_id = $2 AND id = $3
	`, name, orgID, id)
	return err
}

func (s *orgTemplateStore) DeleteBlueprint(ctx context.Context, orgID, id string) error {
	_, err := s.app.ExecContext(ctx, `DELETE FROM org_template_blueprints WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *orgTemplateStore) ListBlueprintSteps(ctx context.Context, orgID, blueprintID string) ([]domain.BlueprintStep, error) {
	rows, err := s.app.QueryContext(ctx, `
		SELECT blueprint_id, step_index, step_prompt_id, brief, created_at
		FROM org_template_blueprint_steps
		WHERE org_id = $1 AND blueprint_id = $2
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
	rows, err := s.app.QueryContext(ctx, `
		SELECT blueprint_id, step_index, step_prompt_id, brief, created_at
		FROM org_template_blueprint_steps
		WHERE org_id = $1
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
	return s.runInTx(ctx, func(tx *sql.Tx) error {
		return replaceOrgTemplateBlueprintSteps(ctx, tx, orgID, blueprintID, stepPromptIDs, briefs)
	})
}

// runInTx runs fn against a *sql.Tx, composing onto the caller's transaction
// when s.app is already a *sql.Tx (inside WithTx) or opening + committing a
// fresh one when it's a *sql.DB (standalone call). Shared by the org-template
// store's multi-statement writes (ReplaceBlueprintSteps, DuplicateBlueprintPrompts)
// so the type-assertion lives in one place.
func (s *orgTemplateStore) runInTx(ctx context.Context, fn func(*sql.Tx) error) error {
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
		return errors.New("postgres org_template: unexpected queryer type")
	}
}

// withTemplateTx runs fn in a transaction over the app pool. The dispatch turns
// on what s.app concretely is. Inside a handler's s.tx.WithTx(...) closure the
// WithTx machinery hands the closure a TxStores whose stores are bound to the
// open *sql.Tx — so the orgTemplateStore reached via tx.OrgTemplate has s.app ==
// that transaction, and running fn inline makes merge/split share the handler's
// tx (atomic with its GetBlueprint reads and the post-write re-read). Called
// directly with no enclosing WithTx (a conformance test), s.app is the *sql.DB
// pool, so we open a short-lived tx for atomicity. Mirrors the dispatch
// ReplaceBlueprintSteps does inline.
func (s *orgTemplateStore) withTemplateTx(ctx context.Context, fn func(*sql.Tx) error) error {
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
		return errors.New("postgres org_template: unexpected queryer type")
	}
}

func (s *orgTemplateStore) MergeBlueprints(ctx context.Context, orgID, hostID, sourceID string) error {
	return s.withTemplateTx(ctx, func(tx *sql.Tx) error {
		var hostLen int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM org_template_blueprint_steps WHERE org_id = $1 AND blueprint_id = $2`,
			orgID, hostID).Scan(&hostLen); err != nil {
			return fmt.Errorf("count host template steps: %w", err)
		}
		// Append source's steps after the host's, densely (host_len + i). The
		// source's indices shift into a range disjoint from the host's existing
		// 0..host_len-1, so the (org_id, blueprint_id, step_index) PK never
		// transiently collides; step_prompt_id is untouched, preserving the
		// (org_id, step_prompt_id) copy-only unique index.
		if _, err := tx.ExecContext(ctx, `
			UPDATE org_template_blueprint_steps
			SET blueprint_id = $1, step_index = step_index + $2
			WHERE org_id = $3 AND blueprint_id = $4
		`, hostID, hostLen, orgID, sourceID); err != nil {
			return fmt.Errorf("reparent source template steps: %w", err)
		}
		// Hard-delete the now-stepless source (template mirror — no soft-delete).
		// Its steps already moved, and the caller validated it trigger-less, so
		// nothing cascades.
		res, err := tx.ExecContext(ctx,
			`DELETE FROM org_template_blueprints WHERE org_id = $1 AND id = $2`, orgID, sourceID)
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
		if _, err := tx.ExecContext(ctx,
			`UPDATE org_template_blueprints SET updated_at = now() WHERE org_id = $1 AND id = $2`, orgID, hostID); err != nil {
			return fmt.Errorf("bump host updated_at: %w", err)
		}
		return nil
	})
}

func (s *orgTemplateStore) SplitBlueprint(ctx context.Context, orgID, id string, atIndex int, newBlueprintID, newSlug, newName string) (string, error) {
	err := s.withTemplateTx(ctx, func(tx *sql.Tx) error {
		// Create the new trigger-less downstream blueprint (template rows carry a
		// non-empty system_slug; the caller supplies a generated tmpl-<uuid>).
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO org_template_blueprints (id, org_id, system_slug, name, source, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'user', now(), now())
		`, newBlueprintID, orgID, newSlug, newName); err != nil {
			return fmt.Errorf("create downstream template blueprint: %w", err)
		}
		// Peel steps [atIndex, N) onto the new blueprint, re-densified 0-based.
		if _, err := tx.ExecContext(ctx, `
			UPDATE org_template_blueprint_steps
			SET blueprint_id = $1, step_index = step_index - $2
			WHERE org_id = $3 AND blueprint_id = $4 AND step_index >= $5
		`, newBlueprintID, atIndex, orgID, id, atIndex); err != nil {
			return fmt.Errorf("reparent tail template steps: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE org_template_blueprints SET updated_at = now() WHERE org_id = $1 AND id = $2`, orgID, id); err != nil {
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
	err := s.app.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT blueprint_id)
		FROM org_template_blueprint_steps
		WHERE org_id = $1 AND step_prompt_id = $2
	`, orgID, stepPromptID).Scan(&n)
	return n, err
}

func (s *orgTemplateStore) BlueprintStepPromptOwner(ctx context.Context, orgID, stepPromptID string) (string, bool, error) {
	var blueprintID string
	err := s.app.QueryRowContext(ctx,
		`SELECT blueprint_id FROM org_template_blueprint_steps WHERE org_id = $1 AND step_prompt_id = $2 LIMIT 1`,
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
	if err := s.runInTx(ctx, func(tx *sql.Tx) error {
		list, err := duplicateOrgTemplateBlueprintPrompts(ctx, tx, orgID, ids)
		newIDs = list
		return err
	}); err != nil {
		return nil, err
	}
	return newIDs, nil
}

// duplicateOrgTemplateBlueprintPrompts runs the resolve → partition → deep-copy
// against the org_template_* tables inside tx. org_id is threaded through every
// WHERE so an out-of-org id simply fails to resolve.
func duplicateOrgTemplateBlueprintPrompts(ctx context.Context, tx *sql.Tx, orgID string, ids []string) ([]string, error) {
	resolved := make([]db.DuplicationStep, 0, len(ids))
	for _, pid := range ids {
		var st db.DuplicationStep
		err := tx.QueryRowContext(ctx, `
			SELECT bs.blueprint_id, b.name, bs.step_index, bs.brief,
			       p.name, p.body, p.model, p.allowed_tools,
			       (SELECT COUNT(*) FROM org_template_blueprint_steps bs2
			        WHERE bs2.org_id = $1 AND bs2.blueprint_id = bs.blueprint_id)
			FROM org_template_blueprint_steps bs
			JOIN org_template_blueprints b ON b.id = bs.blueprint_id AND b.org_id = bs.org_id
			JOIN org_template_prompts p ON p.id = bs.step_prompt_id AND p.org_id = bs.org_id
			WHERE bs.org_id = $1 AND bs.step_prompt_id = $2
		`, orgID, pid).Scan(&st.BlueprintID, &st.BlueprintName, &st.StepIndex, &st.Brief,
			&st.PromptName, &st.PromptBody, &st.PromptModel, &st.PromptTools, &st.BlueprintTotal)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, db.ErrDuplicatePromptNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("resolve template prompt %s: %w", pid, err)
		}
		resolved = append(resolved, st)
	}

	var newIDs []string
	for _, r := range db.PartitionDuplicationRuns(resolved) {
		bpID := uuid.New().String()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO org_template_blueprints (id, org_id, system_slug, name, source, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 'user', now(), now())
		`, bpID, orgID, "tmpl-"+uuid.New().String(), r.Name); err != nil {
			return nil, fmt.Errorf("create duplicated template blueprint: %w", err)
		}
		for i, step := range r.Steps {
			newPromptID := uuid.New().String()
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO org_template_prompts (id, org_id, system_slug, name, body, source, allowed_tools, model, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, 'user', $6, $7, now(), now())
			`, newPromptID, orgID, "tmpl-"+uuid.New().String(), step.PromptName, step.PromptBody,
				step.PromptTools, step.PromptModel); err != nil {
				return nil, fmt.Errorf("copy template prompt: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO org_template_blueprint_steps (org_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
				VALUES ($1, $2, $3, $4, $5, now())
			`, orgID, bpID, i, newPromptID, step.Brief); err != nil {
				return nil, fmt.Errorf("insert duplicated template step %d: %w", i, err)
			}
		}
		newIDs = append(newIDs, bpID)
	}
	return newIDs, nil
}

func replaceOrgTemplateBlueprintSteps(ctx context.Context, tx *sql.Tx, orgID, blueprintID string, stepPromptIDs, briefs []string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM org_template_blueprint_steps WHERE org_id = $1 AND blueprint_id = $2`,
		orgID, blueprintID); err != nil {
		return fmt.Errorf("clear existing template steps: %w", err)
	}
	now := time.Now().UTC()
	for i, stepID := range stepPromptIDs {
		brief := ""
		if i < len(briefs) {
			brief = briefs[i]
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO org_template_blueprint_steps (org_id, blueprint_id, step_index, step_prompt_id, brief, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, orgID, blueprintID, i, stepID, brief, now); err != nil {
			return fmt.Errorf("insert template step %d: %w", i, err)
		}
	}
	return nil
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
	args := []any{orgID}
	q := `SELECT ` + orgTemplateHandlerColumns + ` FROM org_template_handlers WHERE org_id = $1`
	if kind != "" {
		args = append(args, kind)
		q += fmt.Sprintf(" AND kind = $%d", len(args))
	}
	q += `
		ORDER BY kind ASC,
		         CASE WHEN kind = 'rule' THEN sort_order ELSE 0 END ASC,
		         CASE WHEN kind = 'rule' THEN name ELSE '' END ASC,
		         created_at DESC`
	rows, err := s.app.QueryContext(ctx, q, args...)
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
	if !isValidUUID(id) {
		return nil, nil
	}
	h, err := scanOrgTemplateHandler(s.app.QueryRowContext(ctx, `
		SELECT `+orgTemplateHandlerColumns+` FROM org_template_handlers WHERE org_id = $1 AND id = $2
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
		return errors.New("postgres org_template CreateHandler: system_slug required")
	}
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	switch h.Kind {
	case domain.EventHandlerKindRule:
		_, err := s.app.ExecContext(ctx, `
			INSERT INTO org_template_handlers
				(id, org_id, system_slug, kind, event_type, scope_predicate_json, enabled, source,
				 name, default_priority, sort_order, created_at, updated_at)
			VALUES ($1, $2, $3, 'rule', $4, $5::jsonb, $6, 'user', $7, $8, $9, now(), now())
		`, h.ID, orgID, h.SystemSlug, h.EventType, pred, h.Enabled, h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder))
		return err
	case domain.EventHandlerKindTrigger:
		_, err := s.app.ExecContext(ctx, `
			INSERT INTO org_template_handlers
				(id, org_id, system_slug, kind, event_type, scope_predicate_json, enabled, source,
				 blueprint_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
			VALUES ($1, $2, $3, 'trigger', $4, $5::jsonb, $6, 'user', $7, $8, $9, now(), now())
		`, h.ID, orgID, h.SystemSlug, h.EventType, pred, h.Enabled, h.BlueprintID, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability))
		return err
	}
	return fmt.Errorf("postgres org_template CreateHandler: unknown kind %q", h.Kind)
}

func (s *orgTemplateStore) UpdateHandler(ctx context.Context, orgID string, h domain.EventHandler) (bool, error) {
	if !isValidUUID(h.ID) {
		return false, nil
	}
	if err := db.ValidateEventHandlerForCreate(&h); err != nil {
		return false, err
	}
	var pred any
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	// The WHERE pins the row's kind, so a concurrent promote (rule→trigger)
	// between the caller's read and this write makes the UPDATE match 0 rows
	// rather than silently clobbering — the bool surfaces that so the handler
	// can 409 instead of reporting a misleading success.
	var res sql.Result
	var err error
	switch h.Kind {
	case domain.EventHandlerKindRule:
		res, err = s.app.ExecContext(ctx, `
			UPDATE org_template_handlers
			SET scope_predicate_json = $1::jsonb, enabled = $2, name = $3, default_priority = $4, sort_order = $5, updated_at = now()
			WHERE org_id = $6 AND id = $7 AND kind = 'rule'
		`, pred, h.Enabled, h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder), orgID, h.ID)
	case domain.EventHandlerKindTrigger:
		res, err = s.app.ExecContext(ctx, `
			UPDATE org_template_handlers
			SET scope_predicate_json = $1::jsonb, enabled = $2, breaker_threshold = $3, min_autonomy_suitability = $4, updated_at = now()
			WHERE org_id = $5 AND id = $6 AND kind = 'trigger'
		`, pred, h.Enabled, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability), orgID, h.ID)
	default:
		return false, fmt.Errorf("postgres org_template UpdateHandler: unknown kind %q", h.Kind)
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
	if !isValidUUID(id) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE org_template_handlers SET enabled = $1, updated_at = now() WHERE org_id = $2 AND id = $3
	`, enabled, orgID, id)
	return err
}

func (s *orgTemplateStore) RetargetHandlerBlueprint(ctx context.Context, orgID, id, newBlueprintID string) error {
	if !isValidUUID(id) || !isValidUUID(newBlueprintID) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `
		UPDATE org_template_handlers SET blueprint_id = $1, updated_at = now()
		WHERE org_id = $2 AND id = $3 AND kind = 'trigger'
	`, newBlueprintID, orgID, id)
	return err
}

func (s *orgTemplateStore) DeleteHandler(ctx context.Context, orgID, id string) error {
	if !isValidUUID(id) {
		return nil
	}
	_, err := s.app.ExecContext(ctx, `DELETE FROM org_template_handlers WHERE org_id = $1 AND id = $2`, orgID, id)
	return err
}

func (s *orgTemplateStore) PromoteHandler(ctx context.Context, orgID, id string, t domain.EventHandler) error {
	if !isValidUUID(id) {
		return errors.New("postgres org_template PromoteHandler: invalid id")
	}
	if t.Kind != domain.EventHandlerKindTrigger {
		return errors.New("postgres org_template PromoteHandler: target kind must be 'trigger'")
	}
	if t.BlueprintID == "" || t.BreakerThreshold == nil || t.MinAutonomySuitability == nil {
		return errors.New("postgres org_template PromoteHandler: trigger fields required")
	}
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	res, err := s.app.ExecContext(ctx, `
		UPDATE org_template_handlers
		SET kind = 'trigger', blueprint_id = $1, breaker_threshold = $2, min_autonomy_suitability = $3,
		    name = NULL, default_priority = NULL, sort_order = NULL, scope_predicate_json = $4::jsonb, updated_at = now()
		WHERE org_id = $5 AND id = $6 AND kind = 'rule'
	`, t.BlueprintID, *t.BreakerThreshold, *t.MinAutonomySuitability, pred, orgID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("postgres org_template PromoteHandler: row not found or not a rule")
	}
	return nil
}

func (s *orgTemplateStore) ReorderHandlers(ctx context.Context, orgID string, ids []string) error {
	switch v := s.app.(type) {
	case *sql.Tx:
		return reorderOrgTemplateHandlers(ctx, v, orgID, ids)
	case *sql.DB:
		tx, err := v.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := reorderOrgTemplateHandlers(ctx, tx, orgID, ids); err != nil {
			return err
		}
		return tx.Commit()
	default:
		return errors.New("postgres org_template ReorderHandlers: unexpected queryer type")
	}
}

func reorderOrgTemplateHandlers(ctx context.Context, tx *sql.Tx, orgID string, ids []string) error {
	for i, id := range ids {
		if !isValidUUID(id) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE org_template_handlers SET sort_order = $1, updated_at = now()
			WHERE org_id = $2 AND id = $3 AND kind = 'rule'
		`, i, orgID, id); err != nil {
			return err
		}
	}
	return nil
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
