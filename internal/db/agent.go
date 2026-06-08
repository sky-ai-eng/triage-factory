package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrFenceRequiresEventAndTrigger is returned by CreateIfNotFiredSystem
// when run.TriggeringEventID or run.TriggerID is empty. Both bind to SQL
// NULL, which the runs_event_trigger_fence partial unique index treats as
// distinct — the fence would silently not engage and the method's
// exactly-once contract would be lost without a trace (SKY-424). The event
// path always supplies both (the router threads the event id + the matched
// handler id), so an empty value is a programming error, surfaced loud
// rather than degrading to an unfenced insert.
var ErrFenceRequiresEventAndTrigger = errors.New("db: CreateIfNotFiredSystem requires non-empty TriggeringEventID and TriggerID")

//go:generate go run github.com/vektra/mockery/v2 --name=AgentRunStore --output=./mocks --case=underscore --with-expecter

// AgentRunStore owns the runs / run_messages tables — agent run
// lifecycle, transcript messages, and the derived queries the delegate
// spawner + agent handler + chains depend on. All methods take orgID;
// local mode passes runmode.LocalDefaultOrg.
//
// Wired against the app pool in Postgres (RLS-active): every
// consumer is request-equivalent or runs inside a delegate spawner
// goroutine launched from a request handler. System-service reads
// of run state are routed through the admin-pooled FactoryReadStore
// instead — that's the snapshot path; this store is for the actor
// lifecycle.
//
// The MemoryMissing field returned by Get and ListForTask is
// derived from a LEFT JOIN to run_memory (SKY-204) rather than read
// off a column on runs. The JOIN keeps the projection honest by
// construction — a denormalized column drifted from ground truth
// whenever a memory row was written outside the spawner's gate.
//
// The transcript layer (Messages, InsertMessage, TokenTotals) sits on
// run_messages.
type AgentRunStore interface {
	// --- Lifecycle ---

	// Create inserts a new agent run. CreatorUserID defaults to
	// runmode.LocalDefaultUserID for trigger_type='manual' when
	// the caller leaves it empty (test fixtures); for
	// trigger_type='event' empty CreatorUserID maps to SQL NULL
	// per the schema CHECK that pairs trigger_type and creator
	// nullability.
	Create(ctx context.Context, orgID string, run domain.AgentRun) error

	// CreateIfNotFiredSystem inserts an event-triggered run fenced on
	// (triggering_event_id, trigger_id) by the runs_event_trigger_fence
	// partial unique index (SKY-424). Returns inserted=false (no error)
	// when a run for this (event, trigger) already committed — the
	// at-least-once router queue (SKY-414) replayed an event whose first
	// auto-delegation already happened. The run insert is the crash-
	// consistent commit point: fence row exists iff run exists, so a crash
	// after the run commits replays into a clean skip and a crash before it
	// re-fires cleanly. Event path only — run.TriggerType is forced to
	// 'event' (creator_user_id NULL per the runs_creator_matches_trigger_type
	// CHECK), so on Postgres it routes through the admin pool like the
	// event branch of Create. Manual runs stay on Create, whose NULL
	// triggering_event_id never reaches the partial index.
	//
	// Precondition: run.TriggeringEventID and run.TriggerID must be
	// non-empty — both are part of the fence key, and an empty value binds
	// NULL (which the partial index treats as distinct, silently skipping
	// the fence). Impls reject that with ErrFenceRequiresEventAndTrigger
	// rather than insert an unfenced row.
	CreateIfNotFiredSystem(ctx context.Context, orgID string, run domain.AgentRun) (inserted bool, err error)

	// Complete finalizes a run with the terminal totals folded
	// into any partial totals already on the row. The resume path
	// keeps cost/duration/turns running via AddPartialTotals; this
	// call adds the terminal invocation's deltas to produce correct
	// cumulative spend.
	//
	// outcome / outcomeReason persist the parsed terminal-envelope
	// outcome and (abort-only) reason; pass "" for both on runs that
	// have no agent outcome (cancellation, infra failure).
	Complete(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason string) error

	// AddPartialTotals folds an invocation's cost/duration/turns
	// into the running totals without flipping status or
	// completed_at. Called to accumulate spend across turns of one run.
	AddPartialTotals(ctx context.Context, orgID, runID string, costUSD float64, durationMs, numTurns int) error

	// MarkOpen flips a running run to `open` — a turn ended without a
	// conclusion (or the live process idle-closed). Returns ok=false (no
	// error) if the row already reached a terminal state.
	MarkOpen(ctx context.Context, orgID, runID string) (bool, error)

	// MarkResuming flips an `open` run back to running when it is woken
	// (a resume goroutine is about to spawn). ok=false means the run is no
	// longer `open` — caller must not spawn the resume.
	MarkResuming(ctx context.Context, orgID, runID string) (bool, error)

	// SetSession stores the Claude Code session_id captured from
	// the agent's init event. Persisted mid-run, before any
	// terminal state, so the write-gate retry loop (SKY-141) can
	// resume a run whose initial invocation failed the memory
	// check.
	SetSession(ctx context.Context, orgID, runID, sessionID string) error

	// SetExecutorSystem stamps runs.executor_id with the instance id of
	// the executor that owns this run's live process. Called when a run
	// goes live (an unguarded write, like SetStatus); pass "" to clear
	// the pointer. Forward-compat run→executor ownership at N=1; the
	// admin pool is the right door because the run goroutine that stamps
	// it holds no JWT claims. No app-pool variant — ownership is a
	// system concern, never request-scoped.
	SetExecutorSystem(ctx context.Context, orgID, runID, executorID string) error

	// SetStatus writes runs.status without a guard. Used by the
	// delegate spawner for transient progress transitions
	// (fetching, cloning, agent_starting, running) and the
	// completed → pending_approval flip the side-table gates
	// trigger. Guarded transitions go through the Mark* methods.
	SetStatus(ctx context.Context, orgID, runID, status string) error

	// SetWorktreePath writes runs.worktree_path. Set as the
	// spawner finishes worktree setup (GitHub PR clone, Jira
	// run-root creation); takeover transitions use MarkTakenOver
	// to set the path atomically with the status flip.
	SetWorktreePath(ctx context.Context, orgID, runID, path string) error

	// MarkTakenOver atomically flips runs.status to 'taken_over'
	// AND (when claimUserID != "") flips the parent task's claim
	// from the bot to the user in a single transaction. SKY-261
	// B+ tightened this to atomic. ok=false means either the run
	// or the task claim raced out from under us; caller calls
	// abortTakeover to clean up.
	MarkTakenOver(ctx context.Context, orgID, runID, takeoverPath, claimUserID string) (bool, error)

	// MarkReleased flips a held takeover into the "released"
	// sub-state: status stays 'taken_over', worktree_path cleared,
	// result_summary appended. ok=false on double-click or
	// release of a never-taken-over row.
	MarkReleased(ctx context.Context, orgID, runID string) (bool, error)

	// MarkCancelledIfActive marks a run cancelled with the given
	// stop_reason / summary, but only if the row hasn't already
	// reached a terminal state. Used by takeover-rollback.
	MarkCancelledIfActive(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error)

	// MarkFailedIfActive flips a run to 'failed' iff it hasn't
	// already reached a terminal state. The delegate spawner's
	// failRun path uses this so a racing terminal write
	// (takeover, cancel, completion) isn't clobbered. Returns
	// ok=false (no error) if the row is already terminal; the
	// caller logs and continues — the racing path's terminal
	// status stands.
	//
	// `open` is intentionally NOT in the protected set (unlike
	// pending_approval): an `open` run reaches failRun only in the warm
	// window after a no-conclusion turn flipped it open but before idle
	// hibernation took its workspace snapshot (e.g. a proc.Send error on the
	// next correction attempt). With no durable snapshot yet, the run can't be
	// left resumably-open, so failing it is correct — and the per-run cleanup
	// then tears the worktree down. A durably-parked open run (snapshot taken,
	// worktree kept) is only ever woken via ResumeOpenRun, which flips it to
	// `running` before any failRun could see it, so this never clobbers a
	// resumable run.
	MarkFailedIfActive(ctx context.Context, orgID, runID string) (bool, error)

	// MarkPendingApprovalIfCompleted flips a 'completed' run to
	// 'pending_approval' iff the row is currently 'completed'.
	// The delegate spawner's processCompletion uses this when a
	// pending review/PR side-table row gates the terminal status.
	// Returns ok=false on a racing terminal write (cancel,
	// takeover) so the racing path's status stands.
	MarkPendingApprovalIfCompleted(ctx context.Context, orgID, runID string) (bool, error)

	// MarkCompletedIfPendingApproval is the inverse transition: flips
	// a 'pending_approval' run back to 'completed' iff the row is
	// currently 'pending_approval'. The reviews / pending_prs handlers
	// call this after the user submits the artifact (review posted or
	// PR opened) so the run row leaves the approval queue. The guard
	// prevents racing terminal writes (cancel, takeover, discard) from
	// being clobbered if they reach the row first.
	MarkCompletedIfPendingApproval(ctx context.Context, orgID, runID string) (bool, error)

	// MarkDiscarded marks a pending_approval run as cancelled
	// when the user requeues / dismisses the task without
	// submitting the review the agent prepared. The agent process
	// has already exited; this is purely a DB cleanup.
	MarkDiscarded(ctx context.Context, orgID, runID, stopReason string) (bool, error)

	// --- Queries ---

	// Get returns a single agent run by ID, or nil if absent.
	// MemoryMissing is derived from a LEFT JOIN to run_memory.
	Get(ctx context.Context, orgID, runID string) (*domain.AgentRun, error)

	// ListForTask returns all runs for a given task, ordered
	// started_at DESC. MemoryMissing derived per Get.
	ListForTask(ctx context.Context, orgID, taskID string) ([]domain.AgentRun, error)

	// PendingApprovalIDForTask returns the id of the (single)
	// pending_approval run on a task, or "" if none. Bounded to
	// one row by construction.
	PendingApprovalIDForTask(ctx context.Context, orgID, taskID string) (string, error)

	// HasActiveForTask returns true if the task has any agent
	// run that hasn't reached a terminal state. Used as an
	// in-flight gate for auto-delegation.
	HasActiveForTask(ctx context.Context, orgID, taskID string) (bool, error)

	// HasOtherActiveRunForTask returns true if the task has any
	// non-terminal run other than excludeRunID. Used by the
	// spawner's processCompletion to decide whether to flip the
	// parent task to 'done' on terminal — if a newer run is in
	// flight (user re-delegated mid-stream), the task stays open.
	HasOtherActiveRunForTask(ctx context.Context, orgID, taskID, excludeRunID string) (bool, error)

	// HasActiveAutoRunForEntity returns true if any task on the
	// entity has a non-terminal run with trigger_type='event'.
	// Manual delegations are intentionally excluded per SKY-189
	// (manual decoupled from the queue). Used by the router's
	// per-entity firing gate; sweeper uses the same predicate to
	// skip entities that wouldn't drain anyway.
	HasActiveAutoRunForEntity(ctx context.Context, orgID, entityID string) (bool, error)

	// ActiveIDsForTask returns the IDs of runs on the task that
	// haven't reached a terminal state. Used by the task-close
	// → run-cancel cascade.
	ActiveIDsForTask(ctx context.Context, orgID, taskID string) ([]string, error)

	// ListTakenOverIDs returns the IDs of every run currently
	// held in the taken_over state with a live takeover dir.
	// Read at startup so the worktree-cleanup sweep preserves
	// the corresponding ~/.claude/projects entries.
	ListTakenOverIDs(ctx context.Context, orgID string) ([]string, error)

	// ListTakenOverIDsSystem mirrors ListTakenOverIDs but routes
	// through the admin pool in Postgres. The startup worktree-
	// cleanup gate reads this before any JWT-claims context could
	// exist — it has to see every user's taken-over runs to know
	// which worktrees on disk to preserve. Same SKY-296 admin/app
	// split as the rest of this wave.
	ListTakenOverIDsSystem(ctx context.Context, orgID string) ([]string, error)

	// ListParkedWorktreePaths returns the worktree_path of every run
	// parked in `open` or pending_approval with a non-empty
	// worktree_path. Read at startup so the worktree-cleanup sweep
	// preserves a parked run's warm workspace (worktree dir + session
	// JSONL) as the fast resume path. A swept entry still resumes via
	// snapshot rehydrate, so this is an optimization, not a correctness
	// gate.
	ListParkedWorktreePaths(ctx context.Context, orgID string) ([]string, error)

	// ListParkedWorktreePathsSystem mirrors ListParkedWorktreePaths but
	// routes through the admin pool in Postgres. The startup sweep reads
	// it before any JWT-claims context exists.
	ListParkedWorktreePathsSystem(ctx context.Context, orgID string) ([]string, error)

	// HasActiveAutoRunForEntitySystem mirrors HasActiveAutoRunForEntity
	// but routes through the admin pool in Postgres. The router's
	// per-entity firing gate consumes this from its eventbus subscriber
	// goroutine, which has no JWT-claims context.
	HasActiveAutoRunForEntitySystem(ctx context.Context, orgID, entityID string) (bool, error)

	// ActiveIDsForTaskSystem mirrors ActiveIDsForTask but routes through
	// the admin pool in Postgres. The router's task-close cascade uses
	// this to enumerate runs to cancel from its background goroutine.
	ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error)

	// ListTakenOverForResume returns every taken-over run in the
	// local DB, joined with its task + entity for display,
	// ordered newest-first. Used by the CLI's resume command.
	ListTakenOverForResume(ctx context.Context, orgID string) ([]domain.TakenOverRun, error)

	// EntitiesWithOpenRuns returns the subset of entityIDs that have at
	// least one run currently in the `open` state (a turn ended without a
	// conclusion). Drives the factory snapshot's idle-run badge.
	EntitiesWithOpenRuns(ctx context.Context, orgID string, entityIDs []string) (map[string]struct{}, error)

	// --- Transcript / messages ---

	// InsertMessage inserts a run_messages row and returns its
	// auto-assigned id. If msg.CreatedAt is zero, it is stamped
	// with time.Now().UTC() and written back to the caller so a
	// subsequent WS broadcast can carry the same value without a
	// re-read.
	InsertMessage(ctx context.Context, orgID string, msg *domain.AgentMessage) (int64, error)

	// Messages returns all messages for a given run, ordered by id.
	Messages(ctx context.Context, orgID, runID string) ([]domain.AgentMessage, error)

	// TokenTotals sums token usage across all assistant messages
	// in a run. Model is MAX(model) (preserves the
	// last-wins-alphabetically pre-migration behavior).
	TokenTotals(ctx context.Context, orgID, runID string) (*domain.TokenTotals, error)

	// --- Admin-pool variants (`...System`) ---
	//
	// These mirror the per-method shape of the corresponding
	// app-pool methods but route through the admin pool (BYPASSRLS)
	// in Postgres. They exist for the delegate spawner goroutines —
	// the run-lifecycle, transcript-streaming, and post-terminal
	// bookkeeping paths that start from a request handler but
	// continue on detached contexts with no JWT-claims in scope.
	//
	// Behavior contract is identical to the non-System variants:
	// org_id stays in every WHERE clause as defense in depth, return
	// shapes are identical. The only difference is which Postgres
	// pool the statement runs on; SQLite has one connection and the
	// two variants collapse.
	//
	// Create has no System counterpart — it routes internally on
	// trigger_type so event-triggered runs land on the admin pool
	// and manual runs on the app pool. MarkTakenOver also has no
	// System variant: takeover is always user-initiated, so its
	// path goes through synthetic-claims rather than the admin pool.
	GetSystem(ctx context.Context, orgID, runID string) (*domain.AgentRun, error)
	// LookupOrgForRunSystem returns the owning orgID for the given
	// runID, or the empty string with a nil error if no such run
	// exists. Used by the cmd/exec runident helper to discover the
	// run's tenant before any other read — at agent-subprocess cold
	// start the orgID isn't yet known, only TRIAGE_FACTORY_RUN_ID
	// has been passed in. Routes through the admin pool because the
	// agent subprocess has no JWT-claims context yet.
	LookupOrgForRunSystem(ctx context.Context, runID string) (string, error)
	CompleteSystem(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason string) error
	AddPartialTotalsSystem(ctx context.Context, orgID, runID string, costUSD float64, durationMs, numTurns int) error
	MarkOpenSystem(ctx context.Context, orgID, runID string) (bool, error)
	MarkResumingSystem(ctx context.Context, orgID, runID string) (bool, error)
	SetSessionSystem(ctx context.Context, orgID, runID, sessionID string) error
	SetStatusSystem(ctx context.Context, orgID, runID, status string) error
	SetWorktreePathSystem(ctx context.Context, orgID, runID, path string) error
	MarkReleasedSystem(ctx context.Context, orgID, runID string) (bool, error)
	MarkCancelledIfActiveSystem(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error)
	MarkFailedIfActiveSystem(ctx context.Context, orgID, runID string) (bool, error)
	MarkPendingApprovalIfCompletedSystem(ctx context.Context, orgID, runID string) (bool, error)
	HasOtherActiveRunForTaskSystem(ctx context.Context, orgID, taskID, excludeRunID string) (bool, error)
	InsertMessageSystem(ctx context.Context, orgID string, msg *domain.AgentMessage) (int64, error)

	// TokenTotalsSystem mirrors TokenTotals but routes through the
	// admin pool in Postgres. Consumed by agentmeta.Build, which
	// formats the run-metadata footer from contexts that don't carry
	// JWT claims (delegate-spawned agent subprocesses calling
	// `triagefactory exec gh pr-create`, server post-approval
	// submit paths). Adding the read on the admin pool keeps the
	// footer-building utility from having to construct a synthetic-
	// claims tx just to read one aggregate row.
	TokenTotalsSystem(ctx context.Context, orgID, runID string) (*domain.TokenTotals, error)
}
