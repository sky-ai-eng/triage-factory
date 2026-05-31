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

// orgTemplateStore is the SQLite impl of db.OrgTemplateStore (SKY-381).
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

const orgTemplatePromptColumns = `id, name, body, source, kind, allowed_tools, model, system_slug, created_at, updated_at`

const orgTemplateHandlerColumns = `id, kind, event_type, scope_predicate_json, enabled, source, system_slug,
	name, default_priority, sort_order,
	prompt_id, breaker_threshold, min_autonomy_suitability,
	created_at, updated_at`

// --- SeedFromShipped ------------------------------------------------

func (s *orgTemplateStore) SeedFromShipped(ctx context.Context, orgID string, shippedPrompts []domain.Prompt) error {
	now := time.Now().UTC()
	return inTx(ctx, s.q, func(q queryer) error {
		promptIDsBySlug := make(map[string]string, len(shippedPrompts))
		for _, p := range shippedPrompts {
			if p.SystemSlug == "" {
				return fmt.Errorf("org_template seed: shipped prompt %q has empty system_slug", p.Name)
			}
			kind := p.Kind
			if kind == "" {
				kind = domain.PromptKindLeaf
			}
			if _, err := q.ExecContext(ctx, `
				INSERT OR IGNORE INTO org_template_prompts (id, org_id, system_slug, name, body, source, kind, allowed_tools, model, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, 'system', ?, ?, ?, ?, ?)
			`, uuid.New().String(), orgID, p.SystemSlug, p.Name, p.Body, string(kind), p.AllowedTools, p.Model, now, now); err != nil {
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
				promptID, ok := promptIDsBySlug[h.PromptID]
				if !ok || promptID == "" {
					return fmt.Errorf("seed org_template trigger %s: prompt slug %q not found (seed prompts before handlers)", h.ID, h.PromptID)
				}
				if _, err := q.ExecContext(ctx, `
					INSERT OR IGNORE INTO org_template_handlers
						(id, org_id, system_slug, kind, event_type, scope_predicate_json, enabled, source,
						 prompt_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
					VALUES (?, ?, ?, 'trigger', ?, ?, 0, 'system', ?, ?, ?, ?, ?)
				`, uuid.New().String(), orgID, h.ID, h.EventType, pred, promptID, h.BreakerThreshold, h.MinAutonomySuitability, now, now); err != nil {
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
			SELECT id, system_slug, name, body, source, kind, allowed_tools, model
			FROM org_template_prompts WHERE org_id = ?
		`, orgID)
		if err != nil {
			return fmt.Errorf("org_template materialize: list prompts: %w", err)
		}
		type tmplPrompt struct {
			id, slug, name, body, source, kind, allowedTools, model string
		}
		var tprompts []tmplPrompt
		for rows.Next() {
			var p tmplPrompt
			if err := rows.Scan(&p.id, &p.slug, &p.name, &p.body, &p.source, &p.kind, &p.allowedTools, &p.model); err != nil {
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
				INSERT INTO prompts (id, org_id, team_id, system_slug, creator_user_id, name, body, source, kind, allowed_tools, model, usage_count, user_modified, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
			`, newID, orgID, teamID, p.slug, creator, p.name, p.body, p.source, p.kind, p.allowedTools, p.model, now, now); err != nil {
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

		// Phase 2: handlers. INSERT OR IGNORE for idempotency; re-point the
		// trigger prompt_id and preserve enabled + source verbatim.
		hrows, err := q.QueryContext(ctx, `
			SELECT system_slug, kind, event_type, scope_predicate_json, enabled, source,
			       name, default_priority, sort_order, prompt_id, breaker_threshold, min_autonomy_suitability
			FROM org_template_handlers WHERE org_id = ?
		`, orgID)
		if err != nil {
			return fmt.Errorf("org_template materialize: list handlers: %w", err)
		}
		type tmplHandler struct {
			slug, kind, eventType, source string
			pred, name, promptID          sql.NullString
			enabled                       bool
			defPriority, minAutonomy      sql.NullFloat64
			sortOrder, breaker            sql.NullInt64
		}
		var thandlers []tmplHandler
		for hrows.Next() {
			var h tmplHandler
			if err := hrows.Scan(&h.slug, &h.kind, &h.eventType, &h.pred, &h.enabled, &h.source,
				&h.name, &h.defPriority, &h.sortOrder, &h.promptID, &h.breaker, &h.minAutonomy); err != nil {
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
				teamPromptID, ok := teamPromptIDByTemplateID[h.promptID.String]
				if !ok || teamPromptID == "" {
					return fmt.Errorf("org_template materialize: trigger %s references template prompt %q with no team copy", h.slug, h.promptID.String)
				}
				if _, err := q.ExecContext(ctx, `
					INSERT OR IGNORE INTO event_handlers
						(id, org_id, team_id, creator_user_id, kind, event_type, system_slug,
						 scope_predicate_json, enabled, source, prompt_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
					VALUES (?, ?, ?, ?, 'trigger', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				`, newID, orgID, teamID, creator, h.eventType, h.slug, predArg, h.enabled, h.source,
					teamPromptID, h.breaker.Int64, h.minAutonomy.Float64, now, now); err != nil {
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
	kind := p.Kind
	if kind == "" {
		kind = domain.PromptKindLeaf
	}
	now := time.Now().UTC()
	_, err := s.q.ExecContext(ctx, `
		INSERT INTO org_template_prompts (id, org_id, system_slug, name, body, source, kind, allowed_tools, model, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, p.ID, orgID, p.SystemSlug, p.Name, p.Body, source, string(kind), p.AllowedTools, p.Model, now, now)
	return err
}

func (s *orgTemplateStore) UpdatePrompt(ctx context.Context, orgID, id, name, body, kind, model string) error {
	if kind == "" {
		kind = string(domain.PromptKindLeaf)
	}
	_, err := s.q.ExecContext(ctx, `
		UPDATE org_template_prompts SET name = ?, body = ?, kind = ?, model = ?, updated_at = ?
		WHERE org_id = ? AND id = ?
	`, name, body, kind, model, time.Now().UTC(), orgID, id)
	return err
}

func (s *orgTemplateStore) DeletePrompt(ctx context.Context, orgID, id string) error {
	_, err := s.q.ExecContext(ctx, `DELETE FROM org_template_prompts WHERE org_id = ? AND id = ?`, orgID, id)
	return err
}

func scanOrgTemplatePrompt(scanFn func(dst ...any) error) (domain.Prompt, error) {
	var p domain.Prompt
	var slug sql.NullString
	if err := scanFn(&p.ID, &p.Name, &p.Body, &p.Source, &p.Kind, &p.AllowedTools, &p.Model, &slug, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return p, err
	}
	if slug.Valid {
		p.SystemSlug = slug.String
	}
	return p, nil
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
				 prompt_id, breaker_threshold, min_autonomy_suitability, created_at, updated_at)
			VALUES (?, ?, ?, 'trigger', ?, ?, ?, 'user', ?, ?, ?, ?, ?)
		`, h.ID, orgID, h.SystemSlug, h.EventType, pred, h.Enabled, h.PromptID, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability), now, now)
		return err
	}
	return fmt.Errorf("sqlite org_template CreateHandler: unknown kind %q", h.Kind)
}

func (s *orgTemplateStore) UpdateHandler(ctx context.Context, orgID string, h domain.EventHandler) error {
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
			UPDATE org_template_handlers
			SET scope_predicate_json = ?, enabled = ?, name = ?, default_priority = ?, sort_order = ?, updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'rule'
		`, pred, h.Enabled, h.Name, derefFloat(h.DefaultPriority), derefInt(h.SortOrder), now, orgID, h.ID)
		return err
	case domain.EventHandlerKindTrigger:
		_, err := s.q.ExecContext(ctx, `
			UPDATE org_template_handlers
			SET scope_predicate_json = ?, enabled = ?, breaker_threshold = ?, min_autonomy_suitability = ?, updated_at = ?
			WHERE org_id = ? AND id = ? AND kind = 'trigger'
		`, pred, h.Enabled, derefInt(h.BreakerThreshold), derefFloat(h.MinAutonomySuitability), now, orgID, h.ID)
		return err
	}
	return fmt.Errorf("sqlite org_template UpdateHandler: unknown kind %q", h.Kind)
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

func (s *orgTemplateStore) PromoteHandler(ctx context.Context, orgID, id string, t domain.EventHandler) error {
	if t.Kind != domain.EventHandlerKindTrigger {
		return errors.New("sqlite org_template PromoteHandler: target kind must be 'trigger'")
	}
	if t.PromptID == "" || t.BreakerThreshold == nil || t.MinAutonomySuitability == nil {
		return errors.New("sqlite org_template PromoteHandler: trigger fields required")
	}
	var pred any
	if t.ScopePredicateJSON != nil {
		pred = *t.ScopePredicateJSON
	}
	res, err := s.q.ExecContext(ctx, `
		UPDATE org_template_handlers
		SET kind = 'trigger', prompt_id = ?, breaker_threshold = ?, min_autonomy_suitability = ?,
		    name = NULL, default_priority = NULL, sort_order = NULL, scope_predicate_json = ?, updated_at = ?
		WHERE org_id = ? AND id = ? AND kind = 'rule'
	`, t.PromptID, *t.BreakerThreshold, *t.MinAutonomySuitability, pred, time.Now().UTC(), orgID, id)
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
		promptID      sql.NullString
		breakerNS     sql.NullInt64
		minAutonomyNS sql.NullFloat64
	)
	if err := scanFn(
		&h.ID, &h.Kind, &h.EventType, &pred, &h.Enabled, &h.Source, &slug,
		&nameNS, &defPriority, &sortOrder,
		&promptID, &breakerNS, &minAutonomyNS,
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
	if promptID.Valid {
		h.PromptID = promptID.String
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
