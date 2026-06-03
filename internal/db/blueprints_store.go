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

	// Get returns one blueprint by id or (nil, nil) if not found.
	// Request-facing, so it filters deleted_at IS NULL — a soft-deleted
	// blueprint reads as absent here (use GetSystem to resolve it for in-flight
	// runs / past timelines). Transparent to hidden: Get returns hidden=true
	// rows while filtering deleted rows — the two signals are intentionally
	// asymmetric (hidden removes a row from List but keeps it live; deleted_at
	// is a durable soft-delete that makes the row invisible to all
	// request-facing paths).
	Get(ctx context.Context, orgID string, id string) (*domain.Blueprint, error)

	// GetBySystemSlug resolves a team's copy of a shipped blueprint by its
	// stable system_slug. Returns (nil, nil) when the team has no copy.
	GetBySystemSlug(ctx context.Context, orgID, teamID, systemSlug string) (*domain.Blueprint, error)

	// Create inserts a new blueprint (user or imported source) owned by
	// teamID. Caller-provided ID. The Postgres impl binds teamID directly
	// (it satisfies the team-membership RLS); the SQLite impl ignores it.
	Create(ctx context.Context, orgID, teamID string, b domain.Blueprint) error

	// Delete soft-deletes a blueprint (stamps deleted_at). The row + its
	// blueprint_steps stay as the durable audit trail (a blueprint a trigger
	// fired has run history; its step FK is RESTRICT), but request-facing reads
	// (List/Get) filter deleted_at IS NULL. The ...System reads keep resolving
	// it so in-flight runs + past-run timelines still render the name. Called by
	// the prompt-delete handler's delete-pairing when a prompt that solely
	// constitutes a 1-step blueprint is deleted.
	Delete(ctx context.Context, orgID string, id string) error

	// StepPromptOwner returns the id of the blueprint that holds promptID as a
	// step, if any. The copy-only unique index guarantees at most one, so this
	// resolves the single owner. Used by the steps-put handler's cross-blueprint
	// 422 pre-check ("prompt already belongs to another blueprint — copy it to
	// reuse") and by the prompt-delete delete-pairing. ok=false when the prompt
	// is not a step in any blueprint.
	StepPromptOwner(ctx context.Context, orgID string, promptID string) (blueprintID string, ok bool, err error)

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

	// CountStepReferences returns the number of distinct non-deleted blueprints
	// that reference the given prompt as a step. Request-facing, so it filters
	// blueprints.deleted_at IS NULL. With the copy-only unique index this is
	// always 0 or 1; kept for the "used by a blueprint" surfacing.
	CountStepReferences(ctx context.Context, orgID string, stepPromptID string) (int, error)

	// ReplaceSteps replaces the entire step list for a blueprint in a single
	// transaction. step_index is densely packed 0..N-1 by the writer; briefs
	// are taken positionally and may be empty.
	ReplaceSteps(ctx context.Context, orgID string, blueprintID string, stepPromptIDs []string, briefs []string) error

	// MergeInto absorbs the source blueprint's steps onto the tail of the host
	// blueprint and soft-deletes the now-empty source, atomically. The host
	// keeps its identity, name, and trigger; the source is retired. This is the
	// canvas "merge" gesture (drag the tail of host A onto the entry of a
	// trigger-less source B) as one transactional primitive — done as a
	// sequence of ReplaceSteps + Delete over REST it would orphan prompts on a
	// mid-sequence failure.
	//
	// Source steps are densely appended after the host's existing steps
	// (step_index = host_len + source_step_index); each prompt still lives in
	// exactly one blueprint (the copy-only step-prompt unique index is
	// unaffected — ownership just moves from source to host). team_id/org_id are
	// unchanged since both blueprints share them (validated same-team by the
	// caller). The host's updated_at is bumped.
	//
	// The caller must reject a triggered source for a clean 422; the partial-
	// unique event_handlers index is the hard backstop. MergeInto itself does
	// not touch event_handlers — the source carrying no trigger is a
	// precondition, not something this method enforces or repairs.
	MergeInto(ctx context.Context, orgID string, hostID, sourceID string) error

	// SplitAt partitions a blueprint at atIndex into two, atomically: steps
	// [0, atIndex) stay on id (the trigger, if any, lives on step 0 and is
	// untouched); steps [atIndex, N) move to a new, trigger-less blueprint
	// (newBlueprintID, source="user", same team, name=newName) re-densified
	// 0-based. This is the canvas "split" gesture (disconnect a mid-chain
	// prompt→prompt edge) as one transactional primitive. Both updated_at's are
	// bumped. Returns newBlueprintID for the caller's convenience.
	//
	// The caller validates 0 < atIndex < N (a split that keeps one side empty
	// is a no-op) and supplies newName (defaulted to the new step-0 prompt's
	// name, consistent with auto-wrap).
	SplitAt(ctx context.Context, orgID string, id string, atIndex int, newBlueprintID, newName string) (string, error)

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
