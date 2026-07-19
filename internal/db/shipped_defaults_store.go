package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ShippedDefaultsStore seeds a team's prompts + blueprints (+ steps) +
// event_handlers directly from TF's compile-time shipped lists —
// ai.ShippedPrompts() / ai.ShippedBlueprints() (passed in so internal/db
// stays free of the internal/ai dependency) plus the in-package
// db.ShippedEventHandlers. Every new team — the founder's first team and
// every later one — is seeded the same way, straight from the shipped Go
// slices.
//
// SeedShippedIntoTeam: three-phase shape, (org_id, team_id, system_slug)
// idempotency keys, admin-pool / outside-WithTx posture — it reads from
// the shipped Go slices and does not maintain the system_prompt_versions
// sidecar (nothing reads it). Both dialects enforce the outside-WithTx
// constraint (SQLite has no genuine pool-escape hazard to guard against, but
// enforcing it anyway keeps a misuse from silently working in SQLite/local
// mode and only surfacing as a Postgres/multi-mode production error).
type ShippedDefaultsStore interface {
	// SeedShippedIntoTeam seeds teamID's prompts + blueprints (+ steps) +
	// event_handlers from shippedPrompts + shippedBlueprints + the
	// in-package db.ShippedEventHandlers. Three-phase, idempotent
	// throughout (a re-run no-ops; it never clobbers an existing row):
	//
	//  1. Prompts: probe (org_id, team_id, system_slug); insert missing
	//     rows with a fresh UUID, source='system', creator_user_id NULL,
	//     carrying model + allowed_tools verbatim from the shipped
	//     domain.Prompt (unlike the retired PromptStore.SeedOrUpdate,
	//     which dropped both).
	//  2. Blueprints + steps: insert missing headers keyed the same way;
	//     steps are written only for freshly-inserted headers — an
	//     existing team blueprint's step list is left untouched — each
	//     step's prompt slug resolved via the phase-1 map.
	//  3. Handlers: db.ShippedEventHandlers, rules enabled, triggers
	//     disabled (shipped convention) — delegates to
	//     EventHandlerStore.Seed, resolving each trigger's blueprint slug
	//     through the phase-2 map.
	//
	// Does not write system_prompt_versions. Runs on the admin pool; must
	// run OUTSIDE any WithTx — enforced by both dialects' impls.
	SeedShippedIntoTeam(ctx context.Context, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error
}
