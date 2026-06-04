package db

import (
	"context"
	"errors"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Blueprint-duplication sentinel errors. DuplicatePrompts (+ the org-template
// mirror) returns one of these for the handler to translate into a clean
// status code rather than a raw 500.
var (
	// ErrDuplicateNoPrompts is returned when the prompt-id set is empty after
	// de-duplication — there is nothing to copy.
	ErrDuplicateNoPrompts = errors.New("db: duplicate requires at least one prompt id")
	// ErrDuplicatePromptNotFound is returned when a supplied prompt id does not
	// resolve to a (non-deleted) blueprint step in the acting scope. Copy-only +
	// auto-wrap guarantee every live prompt is exactly one blueprint's step, so a
	// miss means the id is unknown, soft-deleted, or out of scope.
	ErrDuplicatePromptNotFound = errors.New("db: a prompt id did not resolve to a blueprint step")
	// ErrDuplicateCrossTeam is returned when the resolved blueprints span more
	// than the acting team — the endpoint is third-party-callable, so a mixed-
	// scope id set is rejected rather than trusted.
	ErrDuplicateCrossTeam = errors.New("db: duplicate prompt set spans more than the acting team")
)

// DuplicationStep is one selected prompt resolved to its place in a source
// blueprint, carrying everything the deep-copy needs. It is scope-neutral
// (no team/org field): the team-scoped impl validates team membership
// separately, and the org-template mirror has no team dimension.
type DuplicationStep struct {
	BlueprintID    string // source blueprint the prompt is a step of
	BlueprintName  string // source blueprint name (full-run copy naming)
	BlueprintTotal int    // total steps in the source blueprint (full vs partial)
	StepIndex      int    // 0-based position in the source blueprint
	Brief          string // per-step authored text, carried onto the copy
	PromptName     string
	PromptBody     string
	PromptModel    string
	PromptTools    string // allowed_tools
}

// DuplicationRun is one output blueprint: a maximal run of consecutive steps
// from a single source blueprint, with the derived (hybrid) name and the
// ordered steps to deep-copy.
type DuplicationRun struct {
	Name  string
	Steps []DuplicationStep // ordered by source step_index
}

// PartitionDuplicationRuns is the shared resolve-output stage both store impls
// run after reading the selected prompts: it groups the resolved steps by
// source blueprint, sorts by step_index, and partitions each group into
// maximal runs of consecutive indexes (a gap starts a new run). Duplicate
// selections of the same step collapse, so the result is order- and
// dup-independent — the output structure is reconstructed from step_index, not
// input order. Output order is deterministic: source blueprints sorted by id,
// runs by their first step_index.
//
// Naming is hybrid: a run that covers a whole source blueprint becomes
// "{source name} (copy)"; a partial run is named after its first (copied)
// prompt, consistent with auto-wrap / split.
func PartitionDuplicationRuns(steps []DuplicationStep) []DuplicationRun {
	byBlueprint := make(map[string][]DuplicationStep, len(steps))
	order := make([]string, 0, len(steps))
	for _, s := range steps {
		if _, seen := byBlueprint[s.BlueprintID]; !seen {
			order = append(order, s.BlueprintID)
		}
		byBlueprint[s.BlueprintID] = append(byBlueprint[s.BlueprintID], s)
	}
	sort.Strings(order)

	var runs []DuplicationRun
	for _, bpID := range order {
		group := byBlueprint[bpID]
		sort.Slice(group, func(i, j int) bool { return group[i].StepIndex < group[j].StepIndex })
		// Collapse duplicate selections of the same step.
		dedup := make([]DuplicationStep, 0, len(group))
		for i, s := range group {
			if i > 0 && s.StepIndex == group[i-1].StepIndex {
				continue
			}
			dedup = append(dedup, s)
		}
		// Partition into maximal consecutive runs (no step_index gaps).
		start := 0
		for i := 1; i <= len(dedup); i++ {
			if i == len(dedup) || dedup[i].StepIndex != dedup[i-1].StepIndex+1 {
				runs = append(runs, newDuplicationRun(dedup[start:i]))
				start = i
			}
		}
	}
	return runs
}

// newDuplicationRun derives a run's name and copies its step slice. A run that
// spans the whole source blueprint (its length equals the blueprint's total
// step count) is a full clone → "{source name} (copy)"; a partial run is named
// after its first copied prompt.
func newDuplicationRun(steps []DuplicationStep) DuplicationRun {
	name := steps[0].PromptName
	if len(steps) == steps[0].BlueprintTotal {
		name = steps[0].BlueprintName + " (copy)"
	}
	cp := make([]DuplicationStep, len(steps))
	copy(cp, steps)
	return DuplicationRun{Name: name, Steps: cp}
}

// DedupPreserveOrder returns ids with later duplicates removed, preserving
// first-seen order. Shared by both DuplicatePrompts impls so a caller passing
// the same prompt id twice (or the same prompt via two ids — impossible under
// copy-only, but cheap to guard) copies it once.
func DedupPreserveOrder(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

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

	// Rename updates a blueprint's name (its only mutable header field — steps
	// are managed via ReplaceSteps, composition via merge/split). Lets a
	// blueprint carry a name independent of its entry prompt's. No-op on a
	// missing / soft-deleted row; the handler 404s by re-reading.
	Rename(ctx context.Context, orgID, id, name string) error

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

	// DuplicatePrompts deep-copies a flat set of prompt ids into new,
	// trigger-less blueprint(s), atomically. Prompts are the duplication
	// primitive: each resolves to exactly one (blueprint_id, step_index) under
	// copy-only, so the endpoint derives the output structure rather than taking
	// blueprint ids or ranges. The rule (induced contiguous runs): resolve each
	// id → (blueprint, step_index), group by blueprint, sort by index, partition
	// into maximal runs of consecutive indexes, and clone each run into one new
	// blueprint with its prompts deep-copied in order and step_index re-densified
	// 0-based (see PartitionDuplicationRuns). One id → a 1-step blueprint; a whole
	// blueprint's ids → a full clone; a gap in the selection → separate copies
	// with no edge between them.
	//
	// Each duplicated prompt is a brand-new prompts row (new UUID; copied name /
	// body / model / allowed_tools; source="user"; system_slug NULL) so a copy of
	// a system/imported prompt becomes an independent user prompt — editing a copy
	// never touches the source. The step brief is carried onto the clone. Copies
	// are always trigger-less; the source's trigger is untouched (this method
	// never reads or writes event_handlers). Originals are left intact.
	//
	// Same-team assertion: every resolved blueprint must belong to teamID (the
	// acting team) — a mixed-scope id set returns ErrDuplicateCrossTeam rather
	// than being trusted. An id that resolves to no live blueprint step returns
	// ErrDuplicatePromptNotFound; an empty set returns ErrDuplicateNoPrompts.
	// Returns the new blueprint ids in deterministic order (source blueprint id,
	// then run start). All work runs in one transaction.
	DuplicatePrompts(ctx context.Context, orgID, teamID string, promptIDs []string) ([]string, error)

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
