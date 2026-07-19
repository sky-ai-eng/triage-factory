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
// BYPASSRLS) — it is claims-less bootstrap work, the same posture as
// OrgTemplateStore.SeedFromShipped / MaterializeIntoTeam. Phases 1+2
// (prompts, blueprints+steps) run in one admin-pool transaction; phase 3
// delegates to the composed EventHandlerStore.Seed, which manages its own
// admin-pool writes. Inside WithTx, admin points at the caller's *sql.Tx and
// inTx is true; SeedShippedIntoTeam refuses to run there (matches
// OrgTemplateStore).
type shippedDefaultsStore struct {
	admin         queryer
	inTx          bool
	eventHandlers db.EventHandlerStore
}

func newShippedDefaultsStore(admin queryer, eventHandlers db.EventHandlerStore) db.ShippedDefaultsStore {
	return &shippedDefaultsStore{admin: admin, eventHandlers: eventHandlers}
}

// newTxShippedDefaultsStore composes a tx-bound ShippedDefaultsStore for
// WithTx / NewForTx. SeedShippedIntoTeam refuses to run there (inTx=true) —
// mirrors newTxOrgTemplateStore.
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
