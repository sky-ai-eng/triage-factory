package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OrgTemplateStore owns the org_template_prompts + org_template_blueprints
// (+ org_template_blueprint_steps) + org_template_handlers tables — the
// org-level template that BootstrapNewOrg / BootstrapNewTeam copy into a new
// team's prompts + blueprints + event_handlers (SKY-381). It is the editable
// source that sits between TF's shipped defaults and the per-team seed:
//
//	ai.ShippedPrompts() + ai.ShippedBlueprints() + db.ShippedEventHandlers
//	        │  SeedFromShipped — once, at org-create
//	        ▼
//	org_template_*  (per-org, admin-editable)
//	        │  MaterializeIntoTeam — at every team-create (first + Nth)
//	        ▼
//	prompts + blueprints + event_handlers  (per-team copies, the real rows)
//
// Editing the template is forward-only: it changes what the *next* new team
// inherits and never touches a team that already exists (consistent with the
// CLAUDE.md tracking-changes invariant).
//
// # Multi-mode only
//
// The template is a multi-tenant concept. Local mode (N=1) has one team and
// never seeds or reads a template — seedDefaultPrompts continues to seed the
// sole team straight from the shipped lists. The SQLite impl is wired so the
// db-package bootstrap + conformance tests can exercise the store without a
// Postgres container, but production local-mode never reaches it (the HTTP
// surface is org-admin + multi-mode gated, and BootstrapNewOrg/NewTeam are the
// multi-mode org/team-create paths).
//
// # Pool split (Postgres)
//
//   - app   — tf_app, RLS-active. Every CRUD method runs here; the
//     org_template_*_all policies gate each row on tf.user_is_org_admin so a
//     non-admin's reads return nothing and writes are refused.
//   - admin — supabase_admin, BYPASSRLS. SeedFromShipped + MaterializeIntoTeam
//     run here: they're claims-less bootstrap work and MaterializeIntoTeam
//     also writes the team's prompts/event_handlers/system_prompt_versions
//     (system_prompt_versions is REVOKE'd from tf_app).
//
// Inside WithTx both pools collapse onto the caller's *sql.Tx; SeedFromShipped
// and MaterializeIntoTeam refuse to run there (they'd escape the tx scope and
// need the admin pool), matching PromptStore.SeedOrUpdate.
type OrgTemplateStore interface {
	// SeedFromShipped seeds the org's template from the TF shipped defaults:
	// shippedPrompts + shippedBlueprints (passed in so internal/db stays free
	// of the internal/ai dependency — callers supply ai.ShippedPrompts() /
	// ai.ShippedBlueprints()) plus the in-package db.ShippedEventHandlers.
	// Three-phase like SeedTeamDefaults: prompts first (capturing system_slug →
	// template prompt id), then blueprints + their steps (resolving each step's
	// prompt slug via the phase-1 map), then handlers resolving each trigger's
	// blueprint slug via the phase-2 map. Rules seed enabled, triggers disabled
	// (shipped convention). Idempotent via ON CONFLICT(org_id, system_slug)
	// DO NOTHING — a re-run preserves any admin edits and never resurrects a
	// row the admin deleted. Admin pool; must run OUTSIDE WithTx.
	SeedFromShipped(ctx context.Context, orgID string, shippedPrompts []domain.Prompt, shippedBlueprints []domain.SeedBlueprint) error

	// MaterializeIntoTeam copies the org's current template into teamID's
	// prompts + blueprints (+ blueprint_steps) + event_handlers (and
	// system_prompt_versions for system rows), reproducing what SeedTeamDefaults
	// would have written from the shipped lists — only the source rows differ.
	// Three-phase: each template prompt becomes a team prompt copy (new random
	// UUID, same system_slug + source + content); each template blueprint is
	// deep-copied into a team blueprint (same system_slug + source + name) with
	// its steps re-pointed at the team's prompt copies; then each template
	// handler becomes a team handler copy, a trigger's template blueprint
	// remapped to the team's blueprint copy and its enabled state preserved
	// verbatim. Idempotent on (org_id, team_id, system_slug) so a bootstrap
	// re-run no-ops. Admin pool; must run OUTSIDE WithTx.
	MaterializeIntoTeam(ctx context.Context, orgID, teamID string) error

	// --- template prompts CRUD (app pool, org-admin RLS) ---

	// ListPrompts returns the org's template prompts ordered by updated_at
	// DESC (the editor's prompt list).
	ListPrompts(ctx context.Context, orgID string) ([]domain.Prompt, error)
	// GetPrompt returns one template prompt by id, or (nil, nil) if absent.
	GetPrompt(ctx context.Context, orgID, id string) (*domain.Prompt, error)
	// CreatePrompt inserts an admin-authored template prompt. The caller
	// supplies ID + SystemSlug (a generated tmpl-<uuid>) and sets Source.
	CreatePrompt(ctx context.Context, orgID string, p domain.Prompt) error
	// UpdatePrompt edits a template prompt's name + body + model.
	UpdatePrompt(ctx context.Context, orgID, id, name, body, model string) error
	// DeletePrompt hard-deletes a template prompt. A prompt used as a step in a
	// template blueprint can't be deleted out from under it (RESTRICT, mirroring
	// the team prompts→blueprint_steps guard). Unlike the team prompts table
	// there is no boot re-seed to resurrect a deleted shipped row, so removing a
	// shipped default from the template is a real delete.
	DeletePrompt(ctx context.Context, orgID, id string) error

	// --- template blueprints CRUD (app pool, org-admin RLS) ---

	// ListBlueprints returns the org's template blueprints ordered by
	// updated_at DESC (the editor's blueprint list / the canvas node set).
	ListBlueprints(ctx context.Context, orgID string) ([]domain.Blueprint, error)
	// GetBlueprint returns one template blueprint by id, or (nil, nil) if absent.
	GetBlueprint(ctx context.Context, orgID, id string) (*domain.Blueprint, error)
	// CreateBlueprint inserts an admin-authored template blueprint header. The
	// caller supplies ID + SystemSlug (a generated tmpl-<uuid>) and sets Source.
	// Steps are written separately via ReplaceBlueprintSteps.
	CreateBlueprint(ctx context.Context, orgID string, b domain.Blueprint) error
	// UpdateBlueprint renames a template blueprint header.
	UpdateBlueprint(ctx context.Context, orgID, id, name string) error
	// DeleteBlueprint hard-deletes a template blueprint; its steps and any
	// template triggers referencing it cascade (FK). Like template prompts there
	// is no boot re-seed to resurrect a deleted shipped row.
	DeleteBlueprint(ctx context.Context, orgID, id string) error
	// ListBlueprintSteps returns the ordered step list for a template blueprint.
	ListBlueprintSteps(ctx context.Context, orgID, blueprintID string) ([]domain.BlueprintStep, error)
	// ReplaceBlueprintSteps replaces the entire step list for a template
	// blueprint in one transaction. step_index is densely packed 0..N-1; briefs
	// are positional and may be empty. Each step_prompt_id must reference a
	// template prompt in the same org (same-org composite FK).
	ReplaceBlueprintSteps(ctx context.Context, orgID, blueprintID string, stepPromptIDs, briefs []string) error
	// CountBlueprintStepReferences returns the number of distinct template
	// blueprints that step through the given template prompt. The prompt-delete
	// handler uses it to refuse (409) deleting a prompt still in use — surfacing
	// a friendly message instead of the raw RESTRICT FK error
	// (org_template_blueprint_steps.step_prompt_id is ON DELETE RESTRICT).
	CountBlueprintStepReferences(ctx context.Context, orgID, stepPromptID string) (int, error)
	// BlueprintStepPromptOwner returns the id of the template blueprint that
	// holds stepPromptID as a step, if any. The copy-only unique index
	// guarantees at most one. Used by the template steps-put handler's
	// cross-blueprint 422 pre-check and by the template prompt-delete pairing
	// (hard-delete the wrapping blueprint → its CASCADE drops the step → delete
	// the prompt). ok=false when the prompt is not a step in any blueprint.
	BlueprintStepPromptOwner(ctx context.Context, orgID, stepPromptID string) (blueprintID string, ok bool, err error)

	// --- template handlers CRUD (app pool, org-admin RLS) ---

	// ListHandlers returns the org's template handlers. kind="" returns both;
	// "rule"/"trigger" filters. Same rules-before-triggers ordering as
	// EventHandlerStore.List.
	ListHandlers(ctx context.Context, orgID, kind string) ([]domain.EventHandler, error)
	// GetHandler returns one template handler by id, or (nil, nil) if absent.
	GetHandler(ctx context.Context, orgID, id string) (*domain.EventHandler, error)
	// CreateHandler inserts an admin-authored template handler. The caller
	// supplies ID + SystemSlug (a generated tmpl-<uuid>); a trigger's BlueprintID
	// must reference a template blueprint in the same org.
	CreateHandler(ctx context.Context, orgID string, h domain.EventHandler) error
	// UpdateHandler edits a template handler's mutable fields (predicate +
	// enabled + the per-kind fields). kind, event_type, and a trigger's
	// blueprint_id are immutable. The UPDATE pins the row's kind in its WHERE, so
	// it returns matched=false (rather than silently no-op'ing) when the row
	// was deleted or promoted (rule→trigger) since the caller read it — the
	// handler maps that to 404/409 instead of a misleading 200. Correct under
	// READ COMMITTED on both dialects (a single conditional UPDATE re-checks
	// its WHERE under a row lock); no isolation bump or retry loop needed.
	UpdateHandler(ctx context.Context, orgID string, h domain.EventHandler) (matched bool, err error)
	// SetHandlerEnabled flips just the enabled bit.
	SetHandlerEnabled(ctx context.Context, orgID, id string, enabled bool) error
	// DeleteHandler hard-deletes a template handler (no soft-disable: the
	// template isn't re-seeded on boot, so a delete sticks).
	DeleteHandler(ctx context.Context, orgID, id string) error
	// PromoteHandler converts a kind='rule' template row to kind='trigger'.
	PromoteHandler(ctx context.Context, orgID, id string, t domain.EventHandler) error
	// ReorderHandlers updates sort_order for the rules in the given id order.
	ReorderHandlers(ctx context.Context, orgID string, ids []string) error
}
