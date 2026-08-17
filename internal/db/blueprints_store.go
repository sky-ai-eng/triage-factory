package db

import (
	"context"
	"errors"
	"sort"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Blueprint-duplication sentinel errors. DuplicatePrompts returns one of
// these for the handler to translate into a clean status code rather than a
// raw 500.
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
	// ErrBlueprintRunFenceRequiresEventAndTrigger is returned by
	// CreateRunIfNotFiredSystem when TriggeringEventID or TriggerID is empty.
	// Both bind to SQL NULL, which the partial unique fence index treats as
	// distinct — the fence would silently not engage. The event path always
	// supplies both, so an empty value is a programming error surfaced loud.
	ErrBlueprintRunFenceRequiresEventAndTrigger = errors.New("db: CreateRunIfNotFiredSystem requires non-empty TriggeringEventID and TriggerID")
	// ErrTaskBusyActiveAutoRun is returned by CreateRunIfNotFiredSystem
	// (Postgres) when the insert loses to the one-active-auto-run-per-task
	// partial unique index: a DIFFERENT (event, trigger) pair won the race
	// to fire on the same task. Deliberately distinct from the
	// inserted=false replay-fence outcome — a replay is permanently
	// satisfied (the run for THIS event exists), while task-busy is a
	// deferral: the caller's intent is still valid and must be queued (or
	// released back to the queue), never dropped. Conflating the two turns
	// a routine busy-task race into silent intent loss.
	ErrTaskBusyActiveAutoRun = errors.New("db: another auto run is active on this task (one-active-per-task index)")
)

// DuplicationStep is one selected prompt resolved to its place in a source
// blueprint, carrying everything the deep-copy needs. It is scope-neutral
// (no team/org field): the team-scoped impl validates team membership
// separately.
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
//     modeled on PromptStore: Create / Get / List / GetBySystemSlug).
//   - blueprint_steps      — ordered membership list for a blueprint.
//   - blueprint_runs       — one row per multi-step blueprint run,
//     owning the worktree shared across every step.
//   - conversations       — read-only here (per-step state lives on
//     conversations; RunsForBlueprint returns the slice of step rows linked
//     to a blueprint_run).
//     Step advancement reads each step run's terminal conversations.outcome (see
//     delegate.decideBlueprintStep); there is no separate verdict channel.
//
// Audiences:
//
//   - HTTP handlers (server/blueprints_handler.go, server/prompts_handler.go,
//     server/artifacts_handler.go, server/reviews_handler.go) — read-mostly.
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
// runmode.LocalDefaultOrgID via assertLocalOrg.
//
// The ...System methods route through the admin pool (claims-less system
// callers); every other method runs on the app pool.
//
// # user_modified stamping contract
//
// An upcoming sync feature (separate ticket) will treat blueprints.user_modified
// as the durable signal that a team edited a shipped blueprint, so its copy
// must never be overwritten by a future re-seed. For that flag to be
// trustworthy, every structural edit that changes what a blueprint contains —
// not just cosmetic bookkeeping — must stamp it true on the affected
// surviving row(s):
//
//   - Rename stamps the renamed blueprint.
//   - ReplaceSteps stamps the blueprint whose step list changed.
//   - MergeInto stamps the host (the source is retired, not stamped).
//   - SplitAt stamps the kept upstream blueprint AND creates the new
//     downstream blueprint with user_modified already true (a split product
//     is never a shipped row, so it never carries a system_slug either).
//   - DeleteStep stamps the original blueprint (whether it keeps steps or is
//     retired) and, if a downstream is minted, creates it the same way
//     SplitAt does.
//
// Create inserts fresh rows with user_modified left false — a brand-new row
// hasn't diverged from anything yet, and non-shipped (no system_slug) rows are
// invisible to the sync regardless (the shipped seeder,
// ShippedDefaultsStore.SeedShippedIntoTeam, likewise inserts them unflagged).
// Delete, IncrementUsage/IncrementUsageSystem, and DuplicatePrompts (fresh
// user-source copies; originals untouched) never touch the flag.
// BlueprintListFilter is the blueprint list's filter set.
type BlueprintListFilter struct {
	// TeamID narrows to one team's blueprints (the multi-team page scoped to
	// a team). Empty means every blueprint the caller may see. The SQLite
	// impl ignores it — local mode is single-team.
	TeamID string

	// GatedEventTypes are the event types this org is NOT entitled to. A
	// blueprint is hidden when it has at least one attached trigger and every
	// one of those triggers fires on a gated type — it has no live behavior
	// the org can use. A blueprint with no triggers at all is never hidden by
	// this (that is the orphaned state, unrelated to entitlements).
	//
	// The gate lives here rather than as a post-read filter in the handler
	// because a filter applied after the window makes the page short and the
	// total wrong: the rows a caller cannot see must not be counted, and the
	// only place that can be true is the query that counts them.
	GatedEventTypes []string
}

// BlueprintStepListFilter is the step list's filter set.
type BlueprintStepListFilter struct {
	// TeamID narrows to one team's blueprints, like BlueprintListFilter's.
	TeamID string
	// BlueprintIDs narrows to specific blueprints. Empty means every
	// blueprint in scope — the canvas's bulk read. A non-empty set is the
	// per-blueprint read, which is why there is no second route for it.
	BlueprintIDs []string
}

// BlueprintRunListFilter is the blueprint-run list's filter set.
type BlueprintRunListFilter struct {
	// BlueprintID narrows to the runs of one blueprint. Empty means every
	// run in the org.
	BlueprintID string
	// Statuses narrows by run status. Empty means every status.
	Statuses []string
}

type BlueprintStore interface {
	// --- Blueprint header CRUD (modeled on PromptStore) ----------------

	// List returns one page of non-hidden blueprints plus the unpaged total,
	// ordered by updated_at DESC with an id tiebreaker so the pages partition
	// a total order.
	List(ctx context.Context, orgID string, f BlueprintListFilter, opts ListOpts) ([]domain.Blueprint, int, error)

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
	// user_modified stays false — a fresh row hasn't diverged from anything.
	Create(ctx context.Context, orgID, teamID string, b domain.Blueprint) error

	// Rename updates a blueprint's name (its only mutable header field — steps
	// are managed via ReplaceSteps, composition via merge/split). Lets a
	// blueprint carry a name independent of its entry prompt's. No-op on a
	// missing / soft-deleted row; the handler 404s by re-reading. Stamps
	// user_modified true on the renamed row (see the stamping contract above).
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

	// ListAllSteps returns one page of blueprint steps plus the unpaged total,
	// ordered by (blueprint_id, step_index) — a total order, since a step's
	// index is unique within its blueprint. It is the binding canvas's bulk
	// read, and with f.BlueprintIDs it is also the per-blueprint read, so the
	// two GETs it replaced are one route. Soft-deleted and hidden blueprints'
	// steps are excluded (the canvas only renders listed blueprints). Caller
	// groups by blueprint_id.
	ListAllSteps(ctx context.Context, orgID string, f BlueprintStepListFilter, opts ListOpts) ([]domain.BlueprintStep, int, error)

	// CountStepReferences returns the number of distinct non-deleted blueprints
	// that reference the given prompt as a step. Request-facing, so it filters
	// blueprints.deleted_at IS NULL. With the copy-only unique index this is
	// always 0 or 1; kept for the "used by a blueprint" surfacing.
	CountStepReferences(ctx context.Context, orgID string, stepPromptID string) (int, error)

	// ReplaceSteps replaces the entire step list for a blueprint in a single
	// transaction. step_index is densely packed 0..N-1 by the writer; briefs
	// are taken positionally and may be empty. Stamps user_modified true on
	// blueprintID (see the stamping contract above).
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
	//
	// Stamps user_modified true on the host (see the stamping contract
	// above); the source is retired, not stamped.
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
	//
	// Stamps user_modified true on the kept upstream blueprint (id) and
	// creates newBlueprintID with user_modified already true and no
	// system_slug — a split product is never a shipped row (see the
	// stamping contract above).
	SplitAt(ctx context.Context, orgID string, id string, atIndex int, newBlueprintID, newName string) (string, error)

	// DeleteStep removes the step at stepIndex from a multi-step blueprint
	// (the count must be >= 2), fragmenting the chain per the split rule in one
	// transaction. The deleted step's own row is preserved on a soft-deleted
	// 1-step blueprint — mirroring the sole-owner pair-delete: the prompt keeps
	// exactly one blueprint_steps row, so the copy-only step-prompt unique index
	// stays satisfied when the caller soft-deletes the companion prompt. This is
	// a physical row invariant, not a resolution one — StepPromptOwner joins
	// through deleted_at IS NULL, so it reports the deleted prompt as unowned
	// (ok=false), exactly as it does after the sole-owner delete. The three
	// observable cases, by position:
	//
	//   - head (stepIndex == 0): steps [1, N) move to a new trigger-less
	//     blueprint (newName), re-densified 0-based; the original blueprint —
	//     now holding only the deleted entry step — is soft-deleted. The
	//     original's trigger is the caller's to detach (EventHandlers is a
	//     separate store); the original blueprint_id retires with the entry
	//     prompt. Returns the new downstream id.
	//   - tail (stepIndex == N-1): the deleted step moves to a fresh
	//     soft-deleted 1-step blueprint; the original keeps steps [0, N-1) with
	//     its trigger + id. Returns "".
	//   - mid (0 < stepIndex < N-1): the original keeps steps [0, stepIndex)
	//     with its trigger + id; steps [stepIndex+1, N) move to a new
	//     trigger-less blueprint (newName), re-densified 0-based; the deleted
	//     step moves to a fresh soft-deleted 1-step blueprint. Returns the new
	//     downstream id.
	//
	// step_index stays densely packed 0-based on every surviving blueprint. Like
	// SplitAt this only ever reparents onto freshly created empty blueprints, so
	// no in-place step_index shift can transiently collide the (blueprint_id,
	// step_index) primary key. newName is the downstream blueprint's name (its
	// new step-0 prompt's name, consistent with SplitAt / auto-wrap); it is
	// ignored for a tail delete (no downstream is minted). The internal
	// isolation blueprint id is minted by the store (it is never surfaced — a
	// soft-deleted audit wrapper). Returns the new downstream blueprint id
	// (head / mid) or "" (tail).
	//
	// Stamps user_modified true on the original blueprintID row — whether it
	// keeps steps (tail/mid) or is retired (head) — and, when a downstream is
	// minted (head/mid), creates it the same way SplitAt does: user_modified
	// already true, no system_slug (see the stamping contract above). The
	// internal isolation wrapper is not part of this contract — it is a
	// born-soft-deleted audit row sync never resurrects regardless of the flag.
	DeleteStep(ctx context.Context, orgID, blueprintID string, stepIndex int, newName string) (downstreamID string, err error)

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
	// Manual delegations use this path (no triggering_event_id, so the replay
	// fence never engages). Event-triggered delegations use
	// CreateRunIfNotFiredSystem instead.
	CreateRun(ctx context.Context, orgID string, br domain.BlueprintRun) (string, error)

	// CreateRunIfNotFiredSystem is the event-path fenced insert: it writes
	// triggering_event_id and relies on the blueprint_runs_event_trigger_fence
	// partial unique index to make (triggering_event_id, trigger_id) at-most-once.
	// Returns inserted=false (no error) when a blueprint_run for this
	// (event, trigger) already committed — the at-least-once router queue
	// replayed an event whose first firing already minted the blueprint_run.
	// The blueprint_run insert is the crash-consistent commit point of a
	// delegation, so the firing path mints it before the orchestrator goroutine
	// does any expensive worktree setup. Routes through the admin pool (event
	// runs carry no JWT claims) and forces trigger_type='event'
	// (creator_user_id NULL per the schema CHECK).
	//
	// Precondition: br.TriggeringEventID and br.TriggerID must be non-empty —
	// both are part of the fence key, and an empty value binds NULL (which the
	// partial index treats as distinct, silently skipping the fence). Impls
	// reject that with ErrBlueprintRunFenceRequiresEventAndTrigger.
	//
	// claim rides the same transaction as the run row: this insert IS the
	// commitment point of a delegation, so the task's agent claim is written
	// with it or not at all (see AgentClaimStamp). Skipped on the fenced
	// no-op — a replay must not re-stamp a claim the original firing already
	// settled, and may not steal one the user has since taken. Returns
	// claimed=true only when the stamp actually moved the claim; a refusal
	// still commits the run.
	CreateRunIfNotFiredSystem(ctx context.Context, orgID string, br domain.BlueprintRun, claim AgentClaimStamp) (inserted, claimed bool, err error)

	// SetRunWorktreePathSystem fills in a blueprint_run's worktree_path after
	// the shared worktree is built. The row is created up front (before setup,
	// so the replay fence commits before expensive work) with an empty path;
	// this stamps the resolved path so the resume/cancel cleanup machinery can
	// reconstruct the worktree later.
	SetRunWorktreePathSystem(ctx context.Context, orgID, id, worktreePath string) error

	// ActiveRunForTaskSystem returns the most recent still-running blueprint_run
	// for a task, or (nil, nil) when none is active. The board-column aggregate
	// (recomputeTaskBoardColumn) reads it to place a task with live work.
	ActiveRunForTaskSystem(ctx context.Context, orgID, taskID string) (*domain.BlueprintRun, error)

	// GetRun returns a blueprint run by id, or (nil, nil) when not found.
	GetRun(ctx context.Context, orgID string, id string) (*domain.BlueprintRun, error)

	// ListRuns returns one page of the org's blueprint runs plus the unpaged
	// total, newest first (started_at DESC) with an id tiebreaker. Before it
	// the collection was unenumerable: a run could only be read by an id a
	// caller already held, so a run whose id was lost was invisible forever.
	ListRuns(ctx context.Context, orgID string, f BlueprintRunListFilter, opts ListOpts) ([]domain.BlueprintRun, int, error)

	// GetRunForRun returns the blueprint run that owns a step run, plus the
	// step index. Returns (nil, nil, nil) when the supplied run is not part of
	// a multi-step blueprint (single-run delegation).
	GetRunForRun(ctx context.Context, orgID string, runID string) (*domain.BlueprintRun, *int, error)

	// MarkRunStatus transitions a blueprint run to a terminal status and
	// records optional abort metadata. Returns (true, nil) when the row was
	// updated, (false, nil) when the guard rejected the write.
	MarkRunStatus(ctx context.Context, orgID string, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (changed bool, err error)

	// ReopenRunForResume flips an `aborted` blueprint_run back to `running` (a
	// compare-and-swap guarded on status='aborted'), clearing its abort metadata,
	// so a resume of the run's completed+abort step can re-run and re-finalize
	// through the normal post-resume disposition. The blueprint terminated when
	// the step aborted, so without this the resumed step's new conclusion would
	// find a terminal blueprint and never advance/close it. Returns (true, nil)
	// when it re-opened the row, (false, nil) when the blueprint was not aborted
	// (already running for an `open` resume, or finalized by a racing
	// path). Runs inside the same tx as ConversationStore.MarkQueuedForResume
	// so the run flip and the blueprint re-open commit atomically.
	ReopenRunForResume(ctx context.Context, orgID string, id string) (reopened bool, err error)

	// SetRunCurrentStepSystem stamps the blueprint_run's durable
	// current_step_index — the queue-driven reactor's sequencing pointer,
	// bumped as it enqueues each next step so a mid-flight blueprint resumes by
	// re-enqueuing this step at boot. Admin pool (reactor has no JWT claims).
	SetRunCurrentStepSystem(ctx context.Context, orgID, id string, stepIndex int) error

	// RequestRunCancelSystem raises the DB sequence-cancel signal
	// (cancel_requested = true) on a still-running blueprint_run, so the claim
	// stops handing out its queued steps and the reactor finalizes it
	// 'cancelled' instead of advancing. Idempotent; returns (true, nil) when it
	// flipped a running row, (false, nil) when the blueprint was already
	// terminal (nothing to cancel). The active-subprocess kill is separate
	// (in-memory s.cancels). Admin pool — callers also route the user-attributed
	// terminal write through their own pool.
	RequestRunCancelSystem(ctx context.Context, orgID, id string) (changed bool, err error)

	// RunsForBlueprint returns every step run linked to a blueprint instance,
	// ordered by blueprint_step_index ASC, started_at ASC.
	RunsForBlueprint(ctx context.Context, orgID string, blueprintRunID string) ([]domain.Conversation, error)

	// ActiveStepRunIDs returns the IDs of step runs on a blueprint that have
	// not reached a terminal state.
	ActiveStepRunIDs(ctx context.Context, orgID string, blueprintRunID string) ([]string, error)

	// StepPlanLengths returns how many steps each named blueprint run's frozen
	// plan holds, keyed by blueprint_run id. A run whose row is absent — not
	// found, or hidden from the caller (manual runs are creator-scoped under
	// RLS) — is simply missing from the map rather than reported as 0, so a
	// caller can tell "no such plan visible" from a plan of no steps.
	//
	// It counts in SQL rather than returning the plan: step_plan holds every
	// step's snapshotted prompt body, and the caller wants a length. Batched
	// because the run-list projection resolves a whole board's worth of
	// conversations at once and must stay O(1) in queries.
	//
	// App pool: the only callers are request handlers projecting runs the same
	// request already read under RLS.
	StepPlanLengths(ctx context.Context, orgID string, blueprintRunIDs []string) (map[string]int, error)

	// --- Admin-pool variants (`...System`) ------------------------------
	//
	// These mirror the per-method shape of the corresponding app-pool methods
	// but route through the admin pool (BYPASSRLS) in Postgres. They exist for
	// the queue-driven orchestrator — the dispatcher + reactor that claim,
	// run, and advance a blueprint through its step list with no JWT-claims in
	// scope (delegate/dispatch.go).
	//
	// CreateRun has no System counterpart — it routes internally on the
	// supplied BlueprintRun.TriggerType.
	ListStepsSystem(ctx context.Context, orgID string, blueprintID string) ([]domain.BlueprintStep, error)
	GetRunSystem(ctx context.Context, orgID string, id string) (*domain.BlueprintRun, error)
	GetRunForRunSystem(ctx context.Context, orgID string, runID string) (*domain.BlueprintRun, *int, error)
	MarkRunStatusSystem(ctx context.Context, orgID string, id string, status domain.BlueprintRunStatus, abortReason string, abortedAtStep *int) (changed bool, err error)
	RunsForBlueprintSystem(ctx context.Context, orgID string, blueprintRunID string) ([]domain.Conversation, error)
	ActiveStepRunIDsSystem(ctx context.Context, orgID string, blueprintRunID string) ([]string, error)
}
