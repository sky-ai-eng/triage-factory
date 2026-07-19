package sqlite

import (
	"context"
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
