package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ShippedDefaultsStore seeds a team's prompts + blueprints (+ steps) +
// event_handlers directly from TF's compile-time shipped lists —
// promptseed.Prompts() / promptseed.Blueprints() (passed in so internal/db
// stays free of the internal/promptseed dependency) plus the in-package
// db.ShippedEventHandlers. Every new team — the founder's first team and
// every later one — is seeded the same way, straight from the shipped Go
// slices.
//
// SeedShippedIntoTeam: three-phase shape, (org_id, team_id, system_slug)
// idempotency keys, admin-pool / outside-WithTx posture — it reads straight
// from the shipped Go slices. Both dialects enforce the outside-WithTx
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
	//     carrying name + body + model + allowed_tools verbatim from the
	//     shipped domain.Prompt.
	//  2. Blueprints + steps: insert missing headers keyed the same way;
	//     steps are written only for freshly-inserted headers — an
	//     existing team blueprint's step list is left untouched — each
	//     step's prompt slug resolved via the phase-1 map.
	//  3. Handlers: db.ShippedEventHandlers, rules enabled, triggers
	//     disabled (shipped convention) — delegates to
	//     EventHandlerStore.Seed, resolving each trigger's blueprint slug
	//     through the phase-2 map.
	//
	// Runs on the admin pool; must run OUTSIDE any WithTx — enforced by both
	// dialects' impls.
	SeedShippedIntoTeam(ctx context.Context, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error

	// SyncShippedIntoTeam brings teamID's UNMODIFIED copies of the shipped
	// defaults up to the current shipped content: shipped blueprints (see
	// below), then db.ShippedEventHandlers via EventHandlerStore.Sync — run
	// after the blueprint pass so a trigger's blueprint-slug re-resolution
	// sees post-sync blueprint ids. Each shipped blueprint is synced as one
	// "sync unit" at a time. A unit is a shipped blueprint's header row + its
	// ordered step list (prompt slugs + briefs) + every step prompt's content
	// (name, body, model, allowed_tools; source stays 'system'). It replaces
	// the old version-hash sidecar: "unmodified equals shipped" is decided by
	// direct content comparison. Per unit:
	//
	//   - Skip if the blueprint row OR any step prompt row is user_modified, or
	//     if the blueprint row (or any current step prompt row) is soft-deleted
	//     — a soft-deleted slug keeps its (org, team, slug) slot occupied and is
	//     never resurrected.
	//   - No-op if the unit already equals shipped (specifically no updated_at
	//     churn — the UI orders by it).
	//   - Insert the whole unit if the team has no blueprint row for the shipped
	//     slug (a default added in a new release). A slug whose blueprint row
	//     exists but is soft-deleted is NOT re-inserted.
	//   - Otherwise rewrite only the diverging parts in one transaction: header
	//     name, step list, each step prompt's content; insert prompts for step
	//     slugs the team lacks (probe (org_id, team_id, slug) first); soft-delete
	//     step prompts dropped from the shipped structure. usage_count, hidden,
	//     created_at, row ids and team ownership are never touched.
	//
	// Hidden step-prompt rows ARE content-updated (hide is reversible) and stay
	// hidden: the sync never writes the hidden column, so an updated hidden row
	// keeps its hidden bit.
	//
	// Before a team's first sync ever runs, a one-time grandfather backfill —
	// gated on the durable teams.shipped_defaults_backfilled_at marker — stamps
	// user_modified on any system-slugged blueprint whose ordered step structure
	// already diverges from shipped, so pre-stamping-era structural edits are
	// protected from the very first clobber. Prompts are not grandfathered: their
	// user_modified has always been stamped by PromptStore.Update, and unflagged
	// prompt-content drift (edited-org-template descendants) is deliberately
	// overwritten.
	//
	// Idempotent and cheap to repeat every boot — the equality check makes an
	// already-synced team a pure read. Runs on the admin pool; must run OUTSIDE
	// any WithTx.
	SyncShippedIntoTeam(ctx context.Context, orgID, teamID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error
}
