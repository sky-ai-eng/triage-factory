package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// OrgTemplateStore owns the org_template_prompts + org_template_handlers
// tables — the org-level template that BootstrapNewOrg / BootstrapNewTeam
// copy into a new team's prompts + event_handlers (SKY-381). It is the
// editable source that sits between TF's shipped defaults and the per-team
// seed:
//
//	ai.ShippedPrompts() + db.ShippedEventHandlers  (TF defaults, Go slices)
//	        │  SeedFromShipped — once, at org-create
//	        ▼
//	org_template_*  (per-org, admin-editable)
//	        │  MaterializeIntoTeam — at every team-create (first + Nth)
//	        ▼
//	prompts + event_handlers  (per-team copies, the real working rows)
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
	// shippedPrompts (passed in so internal/db stays free of the internal/ai
	// dependency — callers supply ai.ShippedPrompts()) plus the in-package
	// db.ShippedEventHandlers. Two-phase like SeedTeamDefaults: prompts first
	// (capturing system_slug → template prompt id), then handlers resolving
	// each trigger's prompt slug. Rules seed enabled, triggers disabled
	// (shipped convention). Idempotent via ON CONFLICT(org_id, system_slug)
	// DO NOTHING — a re-run preserves any admin edits and never resurrects a
	// row the admin deleted. Admin pool; must run OUTSIDE WithTx.
	SeedFromShipped(ctx context.Context, orgID string, shippedPrompts []domain.Prompt) error

	// MaterializeIntoTeam copies the org's current template into teamID's
	// prompts + event_handlers (and system_prompt_versions for system rows),
	// reproducing what SeedTeamDefaults would have written from the shipped
	// lists — only the source rows differ. Two-phase: each template prompt
	// becomes a team prompt copy (new random UUID, same system_slug + source +
	// content), then each template handler becomes a team handler copy with its
	// prompt_id remapped to the team's copy and its enabled state preserved
	// verbatim (an org admin enabling a trigger in the template flows to the
	// new team). Idempotent on (org_id, team_id, system_slug) so a bootstrap
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
	// UpdatePrompt edits a template prompt's name + body + kind + model.
	UpdatePrompt(ctx context.Context, orgID, id, name, body, kind, model string) error
	// DeletePrompt hard-deletes a template prompt; its template triggers
	// cascade (FK). Unlike the team prompts table there is no boot re-seed to
	// resurrect a deleted shipped row, so removing a shipped default from the
	// template is a real delete (that's the "a shipped trigger removed" lever).
	DeletePrompt(ctx context.Context, orgID, id string) error

	// --- template handlers CRUD (app pool, org-admin RLS) ---

	// ListHandlers returns the org's template handlers. kind="" returns both;
	// "rule"/"trigger" filters. Same rules-before-triggers ordering as
	// EventHandlerStore.List.
	ListHandlers(ctx context.Context, orgID, kind string) ([]domain.EventHandler, error)
	// GetHandler returns one template handler by id, or (nil, nil) if absent.
	GetHandler(ctx context.Context, orgID, id string) (*domain.EventHandler, error)
	// CreateHandler inserts an admin-authored template handler. The caller
	// supplies ID + SystemSlug (a generated tmpl-<uuid>); a trigger's PromptID
	// must reference a template prompt in the same org.
	CreateHandler(ctx context.Context, orgID string, h domain.EventHandler) error
	// UpdateHandler edits a template handler's mutable fields (predicate +
	// enabled + the per-kind fields). kind, event_type, and a trigger's
	// prompt_id are immutable. The UPDATE pins the row's kind in its WHERE, so
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
