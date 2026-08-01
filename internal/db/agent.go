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
	// the "injection:*" subtypes. "injection:steer" (input drained between
	// turns, while the model was mid-work) and "injection:executor-changed"
	// (the claim-time notice that the workspace was restored from its last
	// snapshot) are minted by the native loop;
	// "injection:compaction-request" and "injection:compaction-result" are
	// reserved and not yet minted by any code in this repo.
	//
	// InsertMessage/InsertMessageSystem/Messages/MessagesForRuns below serve
	// today's readers (the SDK runtime's live stream, the UI transcript
	// endpoints, spend sums) and are unchanged in observable behavior.
	// ListForAssemblySystem/MarkDeliveredForClaimSystem/SetWindowStateSystem
	// exist for the native loop (P1+): assembly is a pure function of
	// messages rows and nothing else — no run-level or process-level
	// side-state may influence what a loop reconstructs from these methods,
	// only the rows themselves.

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

	// Messages returns the run's messages for display, ordered by the same
	// effective assembly key ListForAssembly uses (COALESCE(seq, id)) rather
	// than by insertion id. A transcript that ordered on id alone would show
	// a row placed by seq — a compaction summary written between two existing
	// rows — somewhere other than where the model read it, which is the one
	// thing a transcript must never do. Every seq is NULL today, so the two
	// keys coincide; that they agree is the point, not a coincidence to rely
	// on.
	//
	// Withdrawn-pending rows (delivered=false AND window_state='inactive' —
	// a staged injection withdrawn before any flush) are excluded: withdrawn
	// means "never happened", so it must not render as transcript history.
	// Delivered inactive rows (compacted history) stay visible.
	Messages(ctx context.Context, orgID, runID string) ([]domain.Message, error)

	// MessagesSince is Messages restricted to rows above a watermark: the
	// same display read with `id > sinceID`. It exists so a client holding a
	// partial transcript can repair it — a websocket frame is a hint, never
	// the only path to a row, and the run station's transcript is otherwise
	// append-only from page load.
	//
	// Visibility is identical to Messages by construction (Messages is this
	// method at sinceID 0, which every real id clears), so the two reads can
	// never disagree about which rows a client is entitled to see.
	//
	// sinceID 0 means "from the beginning". Callers normalize anything that
	// isn't a real id to 0 rather than relying on a negative one selecting
	// everything — that it does is an artifact of ids starting at 1.
	//
	// The watermark is an id and the ordering is COALESCE(seq, id): those are
	// deliberately different keys. "Which rows has this client not seen yet"
	// is a question about insertion; "where does each row belong" is a
	// question about placement. Once anything writes seq, a returned row may
	// sort before a row the client already holds — merging it is the caller's
	// problem, not this read's.
	MessagesSince(ctx context.Context, orgID, runID string, sinceID int) ([]domain.Message, error)

	// MessagesForRuns is the batched form of Messages: every message
	// for any of the given run IDs as one flat slice, with the same
	// withdrawn-pending exclusion and the same COALESCE(seq, id) ordering.
	// Each run's messages are contiguous, so the caller groups by RunID with
	// per-run order preserved; order across distinct runs is unspecified (the
	// SQLite read chunks its IN-list). Backs the Board's aggregated
	// include=messages read. Empty runIDs returns nil.
	MessagesForRuns(ctx context.Context, orgID string, runIDs []string) ([]domain.Message, error)

	// ListForAssemblySystem returns every row a native loop needs to rebuild this
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
	ListForAssemblySystem(ctx context.Context, orgID, runID string) ([]domain.Message, error)

	// The delivered flush lives on MarkDeliveredForClaimSystem below:
	// consuming pending rows is an engagement write, so it goes through the
	// claim fence like every other write the loop makes.

	// SetWindowState is the elision/compaction primitive: a batched range
	// flip of window_state from `from` to `to`, restricted to rows currently
	// in state `from` whose effective assembly key (COALESCE(seq, id)) is
	// strictly less than beforeSeq. Returns the number of rows flipped.
	// Called only from a batched cold-moment pass (elision) or compaction —
	// never per-step — per the epic's KV-cache discipline; no production
	// caller exists yet (this ships the primitive, not the policy — P3).
	SetWindowStateSystem(ctx context.Context, orgID, runID string, beforeSeq float64, from, to domain.MessageWindowState) (int, error)

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

	// --- Claim-fenced engagement writes ---
	//
	// The writes an executor makes *as* the engagement driving a
	// conversation: the transcript it streams, the terminal status it
	// records, the setup sub-state it reports. Each names its own claim id
	// rather than letting the server resolve "the conversation's active
	// claim", and each refuses with ErrClaimReleased unless that claim is
	// both live and the one holding the conversation being written. Naming
	// the claim is the assertion of ownership; the refusal is what makes a
	// fencing failure a rejected write instead of silent corruption. See
	// ErrClaimReleased for the full contract and for what a caller must do
	// when it trips.
	//
	// Every method takes the conversation and the claim separately, and the
	// fence requires them to agree — a live claim on some other conversation
	// is refused exactly like a released one. Liveness alone would let a
	// mis-threaded pair write wherever the caller pointed it.
	//
	// The server-side active-claim fallback stays for writers that are not
	// engagements — a user's message typed into a conversation belongs to
	// whatever claim happens to be driving it, which is exactly what the
	// fallback resolves.
	//
	// Admin pool in Postgres, always: the fence's locking read and the write
	// it guards have to share one transaction, and claims is a
	// system-written table that the app pool holds no UPDATE grant on (and
	// therefore cannot lock). These are system writes on a server-derived
	// conversation id, so the ownership check the fence performs stands in
	// for the RLS check the app pool would have made.

	// InsertMessageForClaimSystem inserts a transcript row attributed to
	// claimID, which must still be live. Same insert semantics as
	// InsertMessage in every other respect, except that msg.ClaimID is
	// overwritten with the explicit argument — the claim the fence validated
	// and the claim the row records are the same one by construction.
	InsertMessageForClaimSystem(ctx context.Context, orgID, claimID string, msg *domain.Message) (int64, error)

	// MarkDeliveredForClaimSystem is the engagement's drain flush: the
	// pending rows it folded into an assembly are only its to consume while
	// it still owns the conversation.
	//
	// subtype, when non-empty, is stamped onto the same rows in the same
	// statement. The loop's mid-turn drain flushes human input with
	// "injection:steer" so the row records — durably, in the column
	// assembly reads — that it arrived while the model was working; a bare
	// drain passes "" and leaves each row's own subtype alone. One
	// statement rather than flush-then-stamp because a crash between the
	// two would leave a delivered row whose provenance was lost, and
	// assembly would then render it as an ordinary user turn.
	MarkDeliveredForClaimSystem(ctx context.Context, orgID, runID, claimID string, ids []int, subtype string) error

	// CompactForClaimSystem commits one compaction atomically: optionally
	// insert the reconstructed reply row (forced inactive — it is history the
	// moment it exists, never part of any assembly), insert the result row,
	// flip inactiveIDs to window_state='inactive', and re-seq every
	// undelivered row whose effective assembly key precedes the result row to
	// fractional positions strictly between the result row's id and id+1,
	// preserving their existing relative order. The re-seq is the
	// queued-input ordering contract: a message queued at any time before the
	// compaction commits lands after the summary, in assembly and in display,
	// and a row queued after the commit sorts after those fractions by its
	// integer id alone.
	//
	// One transaction because a partial compaction is transcript corruption
	// in every direction: a result row without its flips doubles the history,
	// flips without a result row erase it. replyRow may be nil (the warm
	// path's reply persisted through the normal insert; only the forced-shape
	// path reconstructs one here). Both row pointers get their assigned IDs
	// written back. Refused with ErrClaimReleased when claimID no longer
	// holds the conversation — zombie executors don't compact.
	CompactForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, replyRow, resultRow *domain.Message, inactiveIDs []int) error

	// SettleCompactionRequestForClaimSystem records a discarded warm
	// compaction attempt on the request row that asked for it: the failed
	// call's token usage and cost (the reply itself is never inserted — a
	// botched summarize attempt is not a conversation event, but its dollars
	// are real and the ledger is SUM(messages.cost_usd) over messages alone),
	// plus a machine-readable reason merged into the row's metadata under
	// "compaction_failure". costUSD nil leaves the column NULL (unpriceable
	// model), never 0.
	SettleCompactionRequestForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, costUSD *float64, reason string) error

	// CompleteForClaimSystem is Complete driven by the engagement that ran
	// the invocation: same status flip, cost settlement, and claim release,
	// refused outright when claimID is already released. The claim it
	// releases is its own by construction — a fenced call can only reach the
	// release with the claim it validated.
	CompleteForClaimSystem(ctx context.Context, orgID, runID, claimID, status string, costUSD float64, durationMs, numTurns int, stopReason, resultSummary, outcome, outcomeReason, failureKind string) error

	// MarkFailedIfActiveForClaimSystem is MarkFailedIfActive driven by the
	// engagement: the infra-failure terminal, refused once the engagement
	// has been fenced out. ok=false keeps its existing meaning (the row was
	// already terminal); a fenced-out caller gets ErrClaimReleased instead,
	// which is a different thing and must not be treated as a lost race.
	MarkFailedIfActiveForClaimSystem(ctx context.Context, orgID, runID, claimID, failureKind string) (bool, error)

	// MarkCancelledIfActiveForClaimSystem is MarkCancelledIfActive driven by
	// the engagement: the terminal an executor writes when its own run's
	// context is cancelled, refused once it has been fenced out.
	//
	// The unfenced twin stays, and is what a USER-initiated cancel uses. That
	// distinction is the whole reason both exist: a person cancelling a run
	// is deliberately overriding whichever executor holds it, so their write
	// must not be gated on ownership, while an executor cancelling itself is
	// only entitled to end a run it still owns. Reaching for the unfenced
	// version from an engagement path is how the cancel route around this
	// fence gets rebuilt.
	MarkCancelledIfActiveForClaimSystem(ctx context.Context, orgID, runID, claimID, stopReason, summary string) (bool, error)

	// SetClaimPhaseSystem writes claims.phase on one named claim — the
	// claim-keyed sibling of SetActiveClaimPhaseSystem, for the engagement
	// reporting its own setup progress. Empty phase clears to NULL. The
	// conversation is bound as well as the claim: the phase a run reports
	// must not be able to land on an engagement driving a different one.
	SetClaimPhaseSystem(ctx context.Context, orgID, conversationID, claimID, phase string) error

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
