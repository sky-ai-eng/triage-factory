package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// BlueprintStore owns the blueprint primitive and its in-flight tables:
//
//   - blueprints          — the triggerable, team-scoped header (header CRUD
//     modeled on PromptStore: Create / Get / List / GetBySystemSlug /
//     SeedOrUpdate).
//   - blueprint_steps      — ordered membership list for a blueprint.
//   - blueprint_runs       — one row per multi-step delegateBlueprint instance,
//     owning the worktree shared across every step.
//   - runs                — read-only here (per-step state lives on runs;
//     RunsForBlueprint returns the slice of step rows linked to a blueprint_run).
//     Step advancement reads each step run's terminal runs.outcome (see
//     delegate.decideBlueprintStep); there is no separate verdict channel.
//
// Audiences:
//
//   - HTTP handlers (server/blueprints_handler.go, server/prompts_handler.go,
//     server/pending_prs_handler.go, server/reviews_handler.go) — read-mostly.
//   - Delegate spawner (delegate/blueprint.go, delegate/run.go) — every method.
//
// # Postgres / RLS shape
//
// blueprints is team-scoped (mirrors prompts: composite uniques (id, org_id)
// and (id, team_id), RLS on creator + team membership). blueprint_steps
// inherits the blueprint's access; blueprint_runs is creator-scoped (RLS gates
// rows on creator_user_id = tf.current_user_id() for manual runs, org-visible
// for event runs). The Postgres impl threads org_id through every WHERE for
// defense in depth and lets RLS enforce the rest; SQLite collapses orgID to
// runmode.LocalDefaultOrg via assertLocalOrg.
//
// SeedOrUpdate routes through the admin pool (claims-less system rows);
// every other method runs on the app pool.
type BlueprintStore interface {
	// --- Blueprint header CRUD (modeled on PromptStore) ----------------

	// SeedOrUpdate inserts a shipped system blueprint as the team's own copy
	// if missing, keyed by (org_id, team_id, system_slug). Re-seed is
	// ON CONFLICT (org_id, team_id, system_slug) DO NOTHING — no versions
	// sidecar (pushing step-list updates to shipped blueprints is a future
	// concern). Returns the team copy's blueprint id (existing or freshly
	// inserted) so the caller can resolve slug→id for the trigger seed's
	// same-team FK and for wiring the steps. b.Source must be "" (defaulted
	// to "system") or "system" and b.SystemSlug must be non-empty.
	SeedOrUpdate(ctx context.Context, orgID, teamID string, b domain.Blueprint) (string, error)

	// List returns non-hidden blueprints ordered by updated_at DESC, scoped
	// to teamID when non-empty (the multi-team page narrowed to one team).
	// The SQLite impl ignores teamID (local mode is single-team).
	List(ctx context.Context, orgID string, teamID string) ([]domain.Blueprint, error)

	// Get returns one blueprint by id (regardless of hidden state) or
	// (nil, nil) if not found.
	Get(ctx context.Context, orgID string, id string) (*domain.Blueprint, error)

	// GetBySystemSlug resolves a team's copy of a shipped blueprint by its
	// stable system_slug. Returns (nil, nil) when the team has no copy.
	GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.Blueprint, error)

	// Create inserts a new blueprint (user or imported source) owned by
	// teamID. Caller-provided ID. The Postgres impl binds teamID directly
	// (it satisfies the team-membership RLS); the SQLite impl ignores it.
	Create(ctx context.Context, orgID, teamID string, b domain.Blueprint) error

	// Update renames a blueprint and marks it user-modified (mirrors
	// PromptStore.Update). The team-scoped editor's rename/meta save; touches
	// only the header — steps are replaced via ReplaceSteps. No-op when no row
	// matches (org_id + id).
	Update(ctx context.Context, orgID string, id, name string) error

	// Delete hard-deletes a blueprint. Its steps and any triggers referencing
	// it cascade (the blueprint_steps / event_handlers composite FKs are
	// ON DELETE CASCADE); a blueprint with run history is FK-protected
	// (blueprint_runs pins it via RESTRICT), so this errors rather than
	// orphaning the audit trail. Forward-only: deleting a shipped system
	// blueprint doesn't stop a later re-seed from reinstating it.
	Delete(ctx context.Context, orgID string, id string) error

	// GetSystem mirrors Get but routes through the admin pool in Postgres.
	// The delegate spawner resolves a trigger/manual blueprint from a
	// goroutine that continues past the request lifecycle.
	GetSystem(ctx context.Context, orgID string, id string) (*domain.Blueprint, error)

	// IncrementUsage bumps usage_count by 1.
	IncrementUsage(ctx context.Context, orgID string, id string) error
	IncrementUsageSystem(ctx context.Context, orgID string, id string) error

	// --- Steps ----------------------------------------------------------

	// ListSteps returns the ordered step list for a blueprint. Empty slice
	// (not error) when the blueprint has no steps configured.
	ListSteps(ctx context.Context, orgID string, blueprintID string) ([]domain.BlueprintStep, error)

	// CountStepReferences returns the number of distinct blueprints that
	// reference the given prompt as a step. Used by the prompt-delete handler
	// to surface "used by N blueprint(s)".
	CountStepReferences(ctx context.Context, orgID string, stepPromptID string) (int, error)

	// ReplaceSteps replaces the entire step list for a blueprint in a single
	// transaction. step_index is densely packed 0..N-1 by the writer; briefs
	// are taken positionally and may be empty.
	ReplaceSteps(ctx context.Context, orgID string, blueprintID string, stepPromptIDs []string, briefs []string) error

	// --- Runs -----------------------------------------------------------

	// CreateRun inserts a new blueprint instance row. TriggerType is required.
	CreateRun(ctx context.Context, orgID string, br domain.BlueprintRun) (string, error)

	// GetRun returns a blueprint run by id, or (nil, nil) when not found.
	GetRun(ctx context.Context, orgID string, id string) (*domain.BlueprintRun, error)

	// GetRunForRun returns the blueprint run that owns a step run, plus the
	// step index. Returns (nil, nil, nil) when the supplied run is not part of
	// a multi-step blueprint (single-run delegation).
	GetRunForRun(ctx context.Context, orgID string, runID string) (*domain.BlueprintRun, *int, error)

	// MarkRunStatus transitions a blueprint run to a terminal status and
	// records optional abort metadata. Returns (true, nil) when the row was
	// updated, (false, nil) when the guard rejected the write.
	MarkRunStatus(ctx context.Context, orgID string, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (changed bool, err error)

	// RunsForBlueprint returns every step run linked to a blueprint instance,
	// ordered by blueprint_step_index ASC, started_at ASC.
	RunsForBlueprint(ctx context.Context, orgID string, blueprintRunID string) ([]domain.AgentRun, error)

	// ActiveStepRunIDs returns the IDs of step runs on a blueprint that have
	// not reached a terminal state.
	ActiveStepRunIDs(ctx context.Context, orgID string, blueprintRunID string) ([]string, error)

	// --- Admin-pool variants (`...System`) ------------------------------
	//
	// These mirror the per-method shape of the corresponding app-pool methods
	// but route through the admin pool (BYPASSRLS) in Postgres. They exist for
	// the blueprint orchestrator goroutine — the long-running loop in
	// delegateBlueprint / runBlueprint / terminateBlueprint that drives a
	// multi-step blueprint through its step list with no JWT-claims in scope.
	//
	// CreateRun has no System counterpart — it routes internally on the
	// supplied BlueprintRun.TriggerType.
	ListStepsSystem(ctx context.Context, orgID string, blueprintID string) ([]domain.BlueprintStep, error)
	GetRunSystem(ctx context.Context, orgID string, id string) (*domain.BlueprintRun, error)
	GetRunForRunSystem(ctx context.Context, orgID string, runID string) (*domain.BlueprintRun, *int, error)
	MarkRunStatusSystem(ctx context.Context, orgID string, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (changed bool, err error)
	RunsForBlueprintSystem(ctx context.Context, orgID string, blueprintRunID string) ([]domain.AgentRun, error)
	ActiveStepRunIDsSystem(ctx context.Context, orgID string, blueprintRunID string) ([]string, error)
}
