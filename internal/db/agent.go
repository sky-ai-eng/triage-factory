package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=ConversationStore --output=./mocks --case=underscore --with-expecter

// ConversationStore owns the conversations / messages tables — agent
// conversation lifecycle, transcript messages, and the derived queries the
// delegate spawner + agent handler + chains depend on. All methods take
// orgID; local mode passes runmode.LocalDefaultOrgID.
//
// Per-engagement execution state (who is driving the conversation, when it
// was claimed, how many times) lives on the claims table. The read
// projections derive Conversation.ClaimedAt (latest claim's claimed_at),
// Attempts (count of claims), and ExecutorID (the active claim's executor,
// "" when none) from claims rather than columns on the conversation, and
// return Conversation.Status as the active claim's phase coalesced over the
// stored status — so a live engagement's setup sub-state surfaces through
// the same field the wire has always carried; the
// terminal lifecycle writes release the conversation's active claim in the
// same operation as the status flip. Conversation rows are minted by
// RunQueueStore.EnqueueRun; there is no direct Create here.
//
// Wired against the app pool in Postgres (RLS-active): every
// consumer is request-equivalent or runs inside a delegate spawner
// goroutine launched from a request handler. System-service reads
// of run state are routed through the admin-pooled FactoryReadStore
// instead — that's the snapshot path; this store is for the actor
// lifecycle. Claim writes always run on the admin pool (claims is a
// system-written table); on the app-pool lifecycle variants the
// conversation flip and the claim release are therefore adjacent
// statements rather than one transaction.
//
// The MemoryMissing field returned by Get and ListForTask is
// derived from a LEFT JOIN to conversation_memory rather than read
// off a column on conversations. The JOIN keeps the projection honest by
// construction — a denormalized column drifted from ground truth
// whenever a memory row was written outside the spawner's gate.
//
// The transcript layer (Messages, InsertMessage, TokenTotalsSystem) sits on
// the messages table.
type ConversationStore interface {
	// --- Lifecycle ---

	// Complete finalizes a conversation (status + terminal narrative
	// fields only — the conversation carries no accounting cache) and
	// releases the conversation's active claim (if one exists) with an
	// outcome mapped from status ('failed'/'task_unsolvable' release as
	// 'failed', 'cancelled' as 'cancelled', anything else as 'completed'),
	// stamping the invocation's reported duration/turns telemetry onto the
	// released claim.
	//
	// costUSD is the invocation's reported total, settled as ONE lump on
	// the engagement's own newest message row — the newest row attributed
	// to the claim this call releases (rows insert claim-stamped while the
	// engagement is live, so the claim locates them; the curator turn
	// release settles the same way). An engagement can bill while
	// recording no rows of its own (system-prompt/cache overhead on an
	// errored run); a nonzero lump then settles, additively, onto the
	// conversation's newest existing message row (which may already carry
	// an earlier invocation's lump) — the ledger is the only spend record.
	// Totals stay exact; per-row time attribution smears in that corner.
	// With no message rows at all the lump is unattributable: logged, not
	// stored. No proration across rows — proration without a pricing table
	// is confidently wrong.
	//
	// outcome / outcomeReason persist the parsed terminal-envelope
	// outcome and (abort-only) reason; pass "" for both on runs that
	// have no agent outcome (cancellation, infra failure).
	//
	// failureKind is the machine-readable failure discriminator
	// (domain.RunFailureKind vocabulary) — non-empty only when status
	// is 'failed' and the caller classified the cause; "" → NULL.
	Complete(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error

	// MarkOpen flips a running run to `open` — a turn ended without a
	// conclusion (or the live process idle-closed). Stamps parked_at to the
	// current time so the snapshot-retention sweep keys this open run off its
	// last park rather than started_at, and releases the active claim with
	// outcome 'parked' (the engagement ended; the parked conversation has no
	// owner until the next claim). Returns ok=false (no error) if the row
	// already reached a terminal state.
	MarkOpen(ctx context.Context, orgID, runID string) (bool, error)

	// MarkQueuedForResume is resume-by-enqueue's status flip: the
	// (status, outcome) compare-and-swap over every non-finish
	// parked/terminal state — `open`, or `completed` with outcome `abort` —
	// with target `queued`: resume-by-enqueue re-queues the SAME row as
	// ordinary claimable work instead of spawning an in-process goroutine.
	// Releases any still-active claim with outcome 'requeued' (ownership is
	// re-established by ClaimNextRun minting a fresh claim at the actual
	// claim, exactly like a fresh EnqueueRun'd row). Clears parked_at and
	// stamps queued_at so the queue timer reads the fresh episode.
	// ok=false means the run is no longer resumable (a concurrent
	// resume/cancel/claim already moved it) — the caller maps the miss to
	// 409.
	MarkQueuedForResume(ctx context.Context, orgID, runID string) (bool, error)

	// SetSession stores the Claude Code session id captured from
	// the agent's init event into conversations.sdk_session_id.
	// Persisted mid-run, before any terminal state, so the
	// write-gate retry loop can resume a run whose initial
	// invocation failed the memory check.
	SetSession(ctx context.Context, orgID, runID, sessionID string) error

	// SetExecutorSystem confirms the executor identity on the
	// conversation's ACTIVE claim — an idempotent go-live re-stamp, kept
	// because it is the one write that runs after the process actually
	// exists. If an active claim exists its executor_id/boot_epoch are
	// updated; if none exists one is minted (claimed_at = now) so the
	// live process is never unattributed. Passing an empty executorID
	// keeps the legacy clear semantics by releasing the active claim
	// with outcome 'requeued'. Both identity columns always travel
	// together. The admin pool is the right door because the run
	// goroutine that stamps it holds no JWT claims. No app-pool variant —
	// ownership is a system concern, never request-scoped.
	SetExecutorSystem(ctx context.Context, orgID, runID, executorID string, bootEpoch int64) error

	// SetWorktreePath writes conversations.worktree_path. Set as the
	// spawner finishes worktree setup (GitHub PR clone, Jira
	// run-root creation).
	SetWorktreePath(ctx context.Context, orgID, runID, path string) error

	// MarkCancelledIfActive marks a run cancelled with the given
	// stop_reason / summary, but only if the row hasn't already
	// reached a terminal state. Releases the active claim (if any) with
	// outcome 'cancelled'. Used by the user-cancel path.
	MarkCancelledIfActive(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error)

	// MarkFailedIfActive flips a run to 'failed' iff it hasn't
	// already reached a terminal state, releasing the active claim (if
	// any) with outcome 'failed'. The delegate spawner's
	// failRun path uses this so a racing terminal write
	// (cancel, completion) isn't clobbered. Returns
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
	//
	// failureKind is the machine-readable failure discriminator
	// (domain.RunFailureKind vocabulary); "" → NULL (unclassified).
	MarkFailedIfActive(ctx context.Context, orgID, runID, failureKind string) (bool, error)

	// --- Queries ---

	// Get returns a single agent run by ID, or nil if absent — any
	// conversation type, not just delegation (curator/subagent rows
	// hydrate the same shape). MemoryMissing is derived from a LEFT JOIN
	// to conversation_memory; ClaimedAt/Attempts/ExecutorID derive from claims per
	// the interface doc. The accounting fields are derived too:
	// TotalCostUSD + the four token fields are SUMs over the messages
	// ledger, DurationMs/NumTurns SUMs over the claims' telemetry.
	Get(ctx context.Context, orgID, runID string) (*domain.Conversation, error)

	// ListForTask returns all runs for a given task, ordered
	// started_at DESC. MemoryMissing + claim fields derived per Get.
	ListForTask(ctx context.Context, orgID, taskID string) ([]domain.Conversation, error)

	// ListForTasks is the batched form of ListForTask: every run for
	// any of the given task IDs, each task's runs newest-first
	// (started_at DESC). The Board's aggregated agent-run fetch groups
	// the flat result by run.TaskID, so a board with N tasks costs one
	// read instead of N. Only per-task order is guaranteed — order
	// across distinct tasks is unspecified (the SQLite read chunks its
	// IN-list to stay under the variable limit). Empty taskIDs returns
	// nil. MemoryMissing + claim fields derived per Get.
	ListForTasks(ctx context.Context, orgID string, taskIDs []string) ([]domain.Conversation, error)

	// HasActiveAutoRunForEntity returns true if any task on the
	// entity has a non-terminal run with trigger_type='event'.
	// Manual delegations are intentionally excluded
	// (manual decoupled from the queue). Used by the router's
	// per-entity firing gate; sweeper uses the same predicate to
	// skip entities that wouldn't drain anyway.
	HasActiveAutoRunForEntity(ctx context.Context, orgID, entityID string) (bool, error)

	// ActiveIDsForTask returns the IDs of runs on the task that
	// haven't reached a terminal state. Used by the task-close
	// → run-cancel cascade.
	ActiveIDsForTask(ctx context.Context, orgID, taskID string) ([]string, error)

	// ListParkedWorktreePathsSystem returns the worktree_path of every run
	// parked in `open` with a non-empty
	// worktree_path, via the admin pool in Postgres (the startup sweep
	// reads it before any JWT-claims context exists). Read at startup so
	// the worktree-cleanup sweep preserves a parked run's warm workspace
	// (worktree dir + session JSONL) as the fast resume path. A swept
	// entry still resumes via snapshot rehydrate, so this is an
	// optimization, not a correctness gate.
	ListParkedWorktreePathsSystem(ctx context.Context, orgID string) ([]string, error)

	// HasActiveAutoRunForEntitySystem mirrors HasActiveAutoRunForEntity
	// but routes through the admin pool in Postgres. The router's
	// per-entity firing gate consumes this from its eventbus subscriber
	// goroutine, which has no JWT-claims context.
	HasActiveAutoRunForEntitySystem(ctx context.Context, orgID, entityID string) (bool, error)

	// ActiveAutoRunIDForEntitySystem returns the ID of the entity's active
	// event-triggered run together with the ID of the task that run belongs
	// to, or ("", "") when none. Same predicate as
	// HasActiveAutoRunForEntitySystem (trigger_type='event', non-terminal);
	// if the at-most-one-active-auto-run-per-entity invariant is ever
	// violated, returns the most recently created. Admin pool only — the
	// router's additive-event injection branch is the sole consumer, from
	// the same claims-less background goroutine as the Has* sibling. The
	// task id lets the absorption rule confirm the active run belongs to the
	// firing's own task before folding into it.
	ActiveAutoRunIDForEntitySystem(ctx context.Context, orgID, entityID string) (runID, taskID string, err error)

	// ActiveIDsForTaskSystem mirrors ActiveIDsForTask but routes through
	// the admin pool in Postgres. The router's task-close cascade uses
	// this to enumerate runs to cancel from its background goroutine.
	ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error)

	// ActiveIDsForTeamSystem returns the IDs of every active run owned by the
	// team (conversations.team_id = teamID), using the same active set as
	// ActiveIDsForTask: status NOT IN ('completed','failed','cancelled',
	// 'task_unsolvable','pending_approval'). This is the team-archive force-stop
	// cascade's enumeration, the team-scoped sibling of
	// ActiveIDsForTaskSystem — each returned id is passed to
	// spawner.Cancel(orgID, runID, ""), which hard-kills a live process or marks
	// a parked `open` run cancelled. pending_approval is deliberately excluded:
	// the agent process already exited with a prepared artifact (no live work to
	// stop), spawner.Cancel can't flip it (MarkCancelledIfActive's filter omits
	// it), and leaving it inert means a later restore can still surface the
	// pending review. Admin pool / org-scoped: archive runs from an org-admin
	// handler whose caller may not be a member of the team, so the team-visibility
	// RLS would hide the rows on the app pool.
	ActiveIDsForTeamSystem(ctx context.Context, orgID, teamID string) ([]string, error)

	// EntitiesWithOpenRuns returns the subset of entityIDs that have at
	// least one run currently in the `open` state (a turn ended without a
	// conclusion). Drives the factory snapshot's idle-run badge.
	EntitiesWithOpenRuns(ctx context.Context, orgID string, entityIDs []string) (map[string]struct{}, error)

	// --- Transcript / messages ---
	//
	// messages.role is app-validated (no CHECK): "assistant" | "tool" |
	// "user". "user" covers both a human's free-form message and the native
	// loop's injected input; subtype further discriminates the latter via
	// the reserved (not yet minted by any code in this repo) subtypes
	// "injection:compaction-request", "injection:compaction-result", and
	// "injection:steer".
	//
	// InsertMessage/InsertMessageSystem/Messages/MessagesForRuns below serve
	// today's readers (the SDK runtime's live stream, the UI transcript
	// endpoints, spend sums) and are unchanged in observable behavior.
	// ListForAssembly/MarkDelivered/SetWindowState exist for the native
	// loop (P1+): assembly is a pure function of messages rows and nothing
	// else — no run-level or process-level side-state may influence what a
	// loop reconstructs from these three methods, only the rows themselves.

	// InsertMessage inserts a messages row and returns its
	// auto-assigned id. If msg.CreatedAt is zero, it is stamped
	// with time.Now().UTC() and written back to the caller so a
	// subsequent WS broadcast can carry the same value without a
	// re-read.
	//
	// msg.UserID is written when non-empty ("" → NULL): the requesting
	// user's turn attribution. msg.ClaimID names the executor engagement
	// that produced the row; an explicit non-empty value always wins,
	// and an empty one resolves server-side to the conversation's active
	// claim — rows written during an engagement belong to it, rows
	// written outside one (pending inputs, queued turns, injections)
	// correctly resolve NULL. A message racing its claim's release lands
	// NULL, harmless.
	//
	// msg.Delivered nil writes the schema default (true — delivered
	// immediately, today's only behavior); a non-nil value writes it
	// explicitly (a native loop's pending-input insert passes false).
	// msg.WindowState "" writes the schema default (domain.MessageWindowActive);
	// a non-empty value writes it explicitly. Neither existing caller in this
	// repo sets either field, so every current insert keeps writing exactly
	// what it always wrote (delivered=true, window_state='active').
	InsertMessage(ctx context.Context, orgID string, msg *domain.Message) (int64, error)

	// Messages returns the run's messages for display, ordered by id.
	// Withdrawn-pending rows (delivered=false AND window_state='inactive' —
	// a staged injection withdrawn before any flush) are excluded: withdrawn
	// means "never happened", so it must not render as transcript history.
	// Delivered inactive rows (compacted history) stay visible.
	Messages(ctx context.Context, orgID, runID string) ([]domain.Message, error)

	// MessagesForRuns is the batched form of Messages: every message
	// for any of the given run IDs as one flat slice, with the same
	// withdrawn-pending exclusion. Each run's
	// messages are contiguous and in insertion order (id ASC), so the
	// caller groups by RunID with per-run order preserved; order across
	// distinct runs is unspecified (the SQLite read chunks its IN-list).
	// Backs the Board's aggregated include=messages read. Empty runIDs
	// returns nil.
	MessagesForRuns(ctx context.Context, orgID string, runIDs []string) ([]domain.Message, error)

	// ListForAssembly returns every row a native loop needs to rebuild this
	// run's exact LLM context, ordered by the effective assembly key
	// COALESCE(seq, id). window_state='inactive' rows are excluded
	// (superseded by compaction, permanently out of the window);
	// 'elided' rows ARE included (the loop renders their deterministic stub
	// from the retained content/is_error) and so are undelivered
	// (delivered=false) rows, flagged as such via Message.Delivered —
	// the loop, not this method, decides whether a pending row is due for
	// consumption at this call site.
	//
	// Assembly purity: this reads messages and nothing else. No caller
	// may layer additional filtering that depends on run-level or
	// process-level state — if a future rule needs to change what gets
	// assembled, it must become a column on this table, per the epic's
	// standing rule.
	ListForAssembly(ctx context.Context, orgID, runID string) ([]domain.Message, error)

	// MarkDelivered flips delivered=true on the given message ids — the
	// batch primitive a native loop calls once it has folded a run of
	// pending rows into an assembly. ids outside runID, already delivered,
	// or nonexistent are silently skipped (no error, no-op).
	MarkDelivered(ctx context.Context, orgID, runID string, ids []int) error

	// SetWindowState is the elision/compaction primitive: a batched range
	// flip of window_state from `from` to `to`, restricted to rows currently
	// in state `from` whose effective assembly key (COALESCE(seq, id)) is
	// strictly less than beforeSeq. Returns the number of rows flipped.
	// Called only from a batched cold-moment pass (elision) or compaction —
	// never per-step — per the epic's KV-cache discipline; no production
	// caller exists yet (this ships the primitive, not the policy — P3).
	SetWindowState(ctx context.Context, orgID, runID string, beforeSeq float64, from, to domain.MessageWindowState) (int, error)

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
	GetSystem(ctx context.Context, orgID, runID string) (*domain.Conversation, error)
	CompleteSystem(ctx context.Context, orgID, runID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error
	// LookupOrgForRunSystem returns the owning orgID for the given
	// runID, or the empty string with a nil error if no such run
	// exists. Used by the cmd/exec runident helper to discover the
	// run's tenant before any other read — at agent-subprocess cold
	// start the orgID isn't yet known, only TRIAGE_FACTORY_CONVERSATION_ID
	// has been passed in. Routes through the admin pool because the
	// agent subprocess has no JWT-claims context yet.
	LookupOrgForRunSystem(ctx context.Context, runID string) (string, error)
	MarkOpenSystem(ctx context.Context, orgID, runID string) (bool, error)
	SetSessionSystem(ctx context.Context, orgID, runID, sessionID string) error
	// SetActiveClaimPhaseSystem writes claims.phase on the conversation's
	// ACTIVE claim — the setup/parked sub-state of a live engagement
	// (fetching, cloning, agent_starting, awaiting_credentials). Empty
	// phase clears to NULL (the agent process is live). Phase lives on the
	// claim rather than the conversation because it is a per-engagement
	// fact: a retry or re-claim starts its own claim with its own setup
	// progress and never rewrites the conversation row. A no-op (no error)
	// when the conversation has no active claim — a released claim's phase
	// is inert history and must not be rewritten.
	SetActiveClaimPhaseSystem(ctx context.Context, orgID, conversationID, phase string) error

	// RecordClaimSandboxStatsSystem stamps one claim's measured sandbox cost
	// — peak memory (MiB) and CPU time (µs), read from the run's cgroup at
	// teardown. Keyed on the CLAIM ID, and deliberately valid on an
	// already-released row: teardown runs after the completion bookkeeping
	// releases the claim, so resolving "the active claim" here would race the
	// release and usually find nothing. The engagement that paid for the
	// resources is named by the caller, not inferred from live state.
	//
	// nil for either value writes NULL — "not measured" (local mode has no
	// sandbox; a pre-5.19 kernel has no memory.peak; a crashed teardown
	// reports neither) as distinct from a measured zero, which is why these
	// are pointers rather than zero-valued ints. A stamp for an unknown claim
	// id is a no-op, not an error: the caller is on a best-effort teardown
	// path where a missing row means the accounting is simply lost.
	RecordClaimSandboxStatsSystem(ctx context.Context, orgID, claimID string, peakMemMB *int, cpuUsec *int64) error

	SetWorktreePathSystem(ctx context.Context, orgID, runID, path string) error
	MarkCancelledIfActiveSystem(ctx context.Context, orgID, runID, stopReason, summary string) (bool, error)
	MarkFailedIfActiveSystem(ctx context.Context, orgID, runID, failureKind string) (bool, error)
	InsertMessageSystem(ctx context.Context, orgID string, msg *domain.Message) (int64, error)

	// LastAgentActivityAtSystem returns the created_at of the run's most
	// recent non-user messages row (role <> 'user') — the "agent last
	// ran" watermark the artifact-change feedback ledger derives
	// against. ok=false (zero time) when the run has no agent messages yet,
	// so the caller falls back to the run's start. User messages are excluded
	// so a just-recorded resume message can't poison the watermark, and the
	// agent's own messages advance it past anything injected live. Admin pool:
	// the resume path runs on a detached goroutine with no JWT claims.
	LastAgentActivityAtSystem(ctx context.Context, orgID, runID string) (at time.Time, ok bool, err error)

	// ListReapableSnapshotKeysSystem returns the (org, blueprint_run_id) of
	// every blueprint_run all of whose resumable-state runs (open /
	// completed+abort) last parked before cutoff — the workspace snapshot keys the
	// retention reaper may safely drop. A blueprint_run with any resumable run
	// still within the TTL is omitted (its shared blob is still wanted). The park
	// timestamp is COALESCE(parked_at, completed_at, started_at): parked_at tracks
	// an open run's last park (stamped by MarkOpen, cleared by the resume flips, so
	// a repeatedly-resumed long-lived run is keyed off its most recent park rather
	// than its initial start), completed_at covers the completed+abort terminal,
	// and started_at is a legacy fallback. System-wide / no org scoping — the
	// retention sweep is a maintenance job that spans tenants; the admin pool is
	// the right door (BYPASSRLS) since the reaper holds no JWT claims.
	ListReapableSnapshotKeysSystem(ctx context.Context, cutoff time.Time) ([]domain.SnapshotReapKey, error)

	// TokenTotalsSystem sums token usage across all assistant messages
	// in a run (Model is MAX(model), preserving last-wins-alphabetically),
	// via the admin pool in Postgres. Consumed by agentmeta.Build, which
	// formats the run-metadata footer from contexts that don't carry
	// JWT claims (delegate-spawned agent subprocesses calling
	// `triagefactory exec gh pr-create`, server post-approval
	// submit paths). The admin pool keeps the
	// footer-building utility from having to construct a synthetic-
	// claims tx just to read one aggregate row.
	TokenTotalsSystem(ctx context.Context, orgID, runID string) (*domain.TokenTotals, error)

	// BlueprintSiblingCostUSDSystem sums the messages ledger's cost_usd
	// stamps across every run in blueprintRunID EXCEPT excludeRunID.
	// agentmeta.Build adds this to the authoring run's
	// own cost so a multi-step blueprint's published review/PR discloses
	// the total spend across all steps, not just the step that authored
	// it. Routes through the admin pool in Postgres — the footer builds
	// from claims-less contexts (agent subprocess, post-approval submit).
	BlueprintSiblingCostUSDSystem(ctx context.Context, orgID, blueprintRunID, excludeRunID string) (float64, error)

	// BlueprintSiblingDurationMsSystem sums the claims' duration_ms
	// telemetry across every run in blueprintRunID EXCEPT excludeRunID.
	// agentmeta.Build adds this to the authoring
	// run's own duration so a multi-step blueprint's published review/PR
	// discloses the total time spent across all steps, not just the step
	// that authored it — the time analog of BlueprintSiblingCostUSDSystem.
	// Routes through the admin pool in Postgres (footer builds from
	// claims-less contexts).
	BlueprintSiblingDurationMsSystem(ctx context.Context, orgID, blueprintRunID, excludeRunID string) (int, error)
}
