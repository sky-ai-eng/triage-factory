package db

import (
	"context"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrNoSuchConversation means an id-keyed conversation write named no row in
// the given org — the miss a RETURNING clause on the conversations table
// reports itself, replacing the silent no-op the unconverted writes used to
// return.
var ErrNoSuchConversation = errors.New("no conversation with that id in this org")

// ErrNoSuchMessage means an id-keyed messages write (a compaction request
// settlement, keyed on its own row id within the conversation) named no row.
var ErrNoSuchMessage = errors.New("no message with that id on that conversation")

//go:generate go run github.com/vektra/mockery/v2 --name=ConversationStore --output=./mocks --case=underscore --with-expecter

// Park is why a conversation is being parked `open` — the sole input to
// ConversationStore.ParkOpen, and the sole thing that distinguishes the two
// ways a conversation stops without concluding.
//
// It is a type rather than a pair of strings because the distinction is
// load-bearing three times over (see ParkOpen) and was previously carried by
// having two nearly-identical store methods, which is exactly how their
// predicates drifted apart.
type Park struct {
	// Deliberate says whether this park was ASKED FOR rather than arrived at.
	// It is a field because it is a fact about the park, and the three things
	// that hang off it (see ParkOpen) are too load-bearing to infer.
	//
	// It used to be inferred, from Reason being non-empty. That made "someone
	// deliberately stopped this" and "there is a string to display" the same
	// bit: a park that wanted to record why it happened without being a
	// cancellation would have released its claim 'cancelled', and nothing
	// would have failed. `idle` is exactly such a reason, and it is why this
	// is now stated.
	Deliberate bool
	// Reason is recorded on conversations.park_reason — the closed
	// domain.ParkReason vocabulary, never free text and never the model's
	// stop reason (that one is per-turn and lives on messages.stop_reason).
	// Empty leaves whatever the row already carries.
	Reason domain.ParkReason
	// ResultSummary is the human-facing note for a deliberate stop. Empty
	// leaves whatever is already on the row.
	//
	// Every stop path passes it empty, and that is the point: the summary is
	// what the RunStation renders as the conversation's verdict, and a stop
	// reached no verdict. What happened is recorded on the transcript
	// instead, as a stop-note row the agent reads on resume and a human reads
	// in place. The field stays because a future deliberate park with an
	// actual conclusion to state would want it.
	ResultSummary string
}

// ParkIdle is the turn simply ending — the live driver's no-conclusion turn or
// its idle close. Claim released 'parked', an already-parked row left alone.
//
// It carries a reason like any other park. "The turn ended and nothing more
// arrived" IS what happened to this conversation, and saying so is what makes
// the column readable at a glance; it is not a deliberate stop, and the field
// above is what says so.
func ParkIdle() Park {
	return Park{Reason: domain.ParkReasonIdle}
}

// ParkStopped is a deliberate stop: someone or something ended this conversation.
// Records the reason, releases the claim 'cancelled', and re-parks an
// already-parked row so the caller knows the stop landed and can finalize the
// blueprint behind it.
func ParkStopped(reason domain.ParkReason, summary string) Park {
	return Park{Deliberate: true, Reason: reason, ResultSummary: summary}
}

// ClaimOutcome is what the engagement's claim releases with. A deliberate stop
// is the one place an engagement's cancellation is still recorded, now that
// the conversation status no longer spells it.
func (p Park) ClaimOutcome() string {
	if p.Deliberate {
		return "cancelled"
	}
	return "parked"
}

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
// ConversationQueueStore.EnqueueConversation; there is no direct Create here.
//
// Wired against the app pool in Postgres (RLS-active): every
// consumer is request-equivalent or runs inside a delegate spawner
// goroutine launched from a request handler. System-service reads
// of conversation state are routed through the admin-pooled FactoryReadStore
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
// MessageWindow bounds a transcript read — see
// ConversationStore.MessagesWindow for the direction rules.
type MessageWindow struct {
	SinceID  int
	BeforeID int
	Limit    int
}

// PRCoherenceTargetQuery describes the three independent ways a conversation
// can be working on a PR: its task entity, a pending review target, or a
// materialized PR/head-branch checkout. EventID is the duplicate fence for
// same-task additive delivery.
type PRCoherenceTargetQuery struct {
	EntityID     string
	EventID      string
	BaseRepo     string
	HeadRepo     string
	ReviewTarget string
	PRRef        string
	BranchRef    string
}

// Returned-row shapes. The lifecycle writes below split by which
// table they land on, and the split decides what each returns:
//
//   - conversations writes (Complete, SetSession, SetWorktreePath and their
//     System/ForClaimSystem twins) return (*domain.Conversation, error),
//     sharing Get's column list and scanner. A miss (no row with that id in
//     the org) is ErrNoSuchConversation — reachable only on the unfenced
//     doors; a ForClaimSystem call that passes the claim fence always lands
//     on a real conversation row, by the same FK the fence itself relies on.
//   - claims writes (SetExecutorSystem, SetExecutorForClaimSystem,
//     SetClaimPhaseSystem, SetActiveClaimPhaseSystem,
//     RecordClaimSandboxStatsSystem) return (*domain.ExecutorClaim, error),
//     sharing the operator claim reads' projection (ConversationQueueStore's
//     ClaimByIDSystem). These are all guard-shaped rather than id-keyed
//     against a caller-named row that must exist: "the active claim", "the
//     named claim if it's still active", "this claim id if it exists" — a
//     miss is the guard declining, (nil, nil), the same shape
//     EntityStore.Close uses, not an error. SetExecutorForClaimSystem and
//     SetClaimPhaseSystem are additionally fenced (ErrClaimReleased) on both
//     dialects, so for them the "named claim no longer active" miss is the
//     refusal rather than the decline.
//   - messages writes: SettleCompactionRequestForClaimSystem targets exactly
//     one row (the compaction request, keyed by its own message id) and
//     returns (*domain.Message, error), sharing Messages' column list and
//     scanner; a miss is ErrNoSuchMessage. CompactForClaimSystem is exempt —
//     it inserts up to two rows, batch-flips an arbitrary span to
//     window_state='inactive', and re-seqs every queued row ahead of the
//     result, so there is no single row a return value could name (the
//     standard's bulk bucket).

// ConversationListFilter is the filter set ConversationStore.List narrows on.
// The zero value narrows nothing: every conversation the caller can see, which
// is the resource-wide question the rail's counts ask.
//
// Each field is a NARROWING, never a mode: an absent one is not a second
// behavior, it is one fewer predicate. That is what lets the Board's read
// (task ids, no status) and the rail's (statuses, no task) be the same read.
type ConversationListFilter struct {
	// TaskIDs narrows to conversations of these tasks. Empty is no task
	// narrowing — NOT "no tasks", which is why a caller that named ids and
	// had every one of them rejected as malformed must still get an empty
	// result rather than the whole set (the impls hold that line).
	TaskIDs []string

	// TeamIDs narrows to conversations owned by these teams —
	// conversations.team_id, denormalized at mint, so the narrowing needs no
	// task hop. Empty is no team narrowing, NOT "no teams", and a caller
	// that named ids and had every one of them rejected as malformed still
	// gets an empty result rather than the whole set (the impls hold that
	// line, the same way TaskIDs does).
	//
	// It narrows WITHIN the caller's visible set rather than around it: on
	// Postgres the select policy already bounds which teams' conversations a
	// viewer sees, so a foreign team id here matches nothing.
	TeamIDs []string

	// Statuses narrows to these DISPLAY statuses — the value the read
	// projections produce, not the stored column, because the stored column
	// carries neither `queued` nor `running` (see domain's conversation
	// status vocabulary). Filtering the column instead would make a count
	// disagree with every surface: claim-phase setup would be invisible and
	// an input-woken `open` row would count as open rather than queued.
	// Empty = every status. Values come from domain.AllConversationStatuses;
	// the route validates them, and an unrecognized one simply matches
	// nothing here.
	Statuses []string

	// Attention keeps only the conversations waiting on a human: one holding
	// an unanswered permission prompt, or one that is not live and still
	// holds an unresolved artifact (a draft PR / a finalized pending review —
	// domain.HasUnresolvedArtifacts). False narrows nothing, the same shape
	// TaskListFilter.OnlyUnclaimed has.
	//
	// Derived, never stored: it is the product's "YOUR MOVE" counted per
	// conversation, so three prompts on one run are one row.
	Attention bool
}

type ConversationStore interface {
	// --- Lifecycle ---

	// Complete finalizes a conversation (status + terminal narrative
	// fields only — the conversation carries no accounting cache) and
	// releases the conversation's active claim (if one exists) with an
	// outcome mapped from status ('failed' releases as 'failed', anything
	// else as 'completed'), stamping the invocation's reported
	// duration/turns telemetry onto the released claim.
	//
	// costUSD is the invocation's reported total, settled as ONE lump on
	// the engagement's own newest message row — the newest row attributed
	// to the claim this call releases (rows insert claim-stamped while the
	// engagement is live, so the claim locates them). An engagement can bill while
	// recording no rows of its own (system-prompt/cache overhead on an
	// errored conversation); a nonzero lump then settles, additively, onto the
	// conversation's newest existing message row (which may already carry
	// an earlier invocation's lump) — the ledger is the only spend record.
	// Totals stay exact; per-row time attribution smears in that corner.
	// With no message rows at all the lump is unattributable: logged, not
	// stored. No proration across rows — proration without a pricing table
	// is confidently wrong.
	//
	// outcome / outcomeReason persist the parsed terminal-envelope
	// outcome and (abort-only) reason; pass "" for both on conversations that
	// have no agent outcome (cancellation, infra failure).
	//
	// failureKind is the machine-readable failure discriminator
	// (domain.ConversationFailureKind vocabulary) — non-empty only when status
	// is 'failed' and the caller classified the cause; "" → NULL.
	//
	// The model's own stop reason is deliberately absent. It is a per-turn
	// fact (`end_turn` / `max_tokens` describe ONE assistant turn) and the
	// runtimes stamp it on the turn that ended, messages.stop_reason; a
	// terminal write recording it at conversation scope was last-write-wins
	// over N turns.
	//
	// Returns the conversation row as it reads immediately after this call —
	// same shape as Get, including the claim- and ledger-derived fields, so a
	// caller never has to re-read to see the cost/duration/turns it just
	// settled. ErrNoSuchConversation if conversationID names no row in the org.
	Complete(ctx context.Context, orgID, conversationID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) (*domain.Conversation, error)

	// ParkOpen flips a conversation to `open`: it stopped without concluding. This is
	// the ONLY writer of that state, and there is deliberately only one —
	// an idle hibernation and a user's cancel produce the same row, because
	// they are the same fact about the conversation. Stamps parked_at (only
	// when unset, so a re-park doesn't restart the snapshot-retention clock on
	// a workspace that went dormant earlier) and releases the active claim.
	// Returns ok=false (no error) if the row already reached a terminal state.
	//
	// park says WHY, and that is the one input the two callers differ on. It
	// decides three things at once, so the difference between "the turn ended"
	// and "someone stopped this" is stated once rather than forked into two
	// methods that drifted:
	//
	//   - the park_reason / result_summary recorded on the row (a Park with no
	//     reason leaves both untouched rather than blanking them),
	//   - the outcome the claim releases with — 'parked' for an idle turn-end,
	//     'cancelled' for a deliberate stop, which is now the ONLY place the
	//     cancellation of an engagement is recorded,
	//   - whether an already-parked row counts as a flip. A deliberate stop
	//     re-parks (the caller has to learn it landed, so it can finalize the
	//     blueprint); an idle park does not, because the live driver parks on
	//     every no-conclusion turn and each one would otherwise re-broadcast.
	ParkOpen(ctx context.Context, orgID, conversationID string, park Park) (bool, error)

	// MarkQueuedForResume is resume-by-enqueue's status flip: the
	// compare-and-swap over every state a conversation can come to rest on
	// and be woken from — `open` or `completed`, whatever the outcome —
	// back to mid-flight: resume-by-enqueue re-queues the SAME row as
	// ordinary claimable work instead of spawning an in-process goroutine.
	// Releases any still-active claim with outcome 'requeued' (ownership is
	// re-established by ClaimNextConversation minting a fresh claim at the actual
	// claim, exactly like a fresh EnqueueConversation'd row). Clears parked_at and
	// park_reason together — both describe a park this call is undoing, and a
	// resumed conversation that went on to conclude must not still name the
	// stop it was picked back up from.
	//
	// Re-stamps preferred_executor_id to the executor of the conversation's
	// newest claim — advisory placement preference, not ownership: a resume
	// should land where the workspace tree already is, and the last engagement
	// is what says where that is. NULL when nothing ever claimed it.
	//
	// Re-stamps queued_at to now as well: the wake opens a new queue episode,
	// and the column marks when the current one began — the placement
	// claim's aging window and the UI's queue-dwell readout both measure
	// from it. started_at is never touched.
	// ok=false means the conversation is no longer resumable (a concurrent
	// resume/cancel/claim already moved it, or it failed) — the caller maps
	// the miss to 409.
	//
	// One blueprint fact IS checked here, in the same statement, because it
	// is the one no caller can check without racing: a `completed` row whose
	// blueprint is still running is refused. That row has handed its terminal
	// to the reactor and is moments from being advanced past or finalized;
	// un-terminaling it makes the reactor read a successor's state where this
	// engagement's terminal should be, and the blueprint dies on it. The
	// `open` arm is unconditional by contrast — a stopped mid-blueprint step
	// is a paused step continuing, and its conclusion SHOULD advance the
	// sequence.
	//
	// WHICH step of a finished blueprint may be woken is still not checked
	// here — only the last one may, and that gate needs an admin-pool read
	// (blueprint_runs RLS hides another user's manual blueprint, and a
	// teammate resuming a conversation must not be refused for a row they merely
	// cannot see), so it lives in the caller.
	MarkQueuedForResume(ctx context.Context, orgID, conversationID string) (bool, error)

	// SetSession stores the Claude Code session id captured from
	// the agent's init event into conversations.sdk_session_id.
	// Persisted mid-flight, before any terminal state, so the
	// write-gate retry loop can resume a conversation whose initial
	// invocation failed the memory check.
	//
	// Returns the conversation row as it reads immediately after this call —
	// same shape as Get. ErrNoSuchConversation if conversationID names no row
	// in the org.
	SetSession(ctx context.Context, orgID, conversationID, sessionID string) (*domain.Conversation, error)

	// SetExecutorSystem confirms the executor identity on the
	// conversation's ACTIVE claim — an idempotent go-live re-stamp, kept
	// because it is the one write that runs after the process actually
	// exists. If an active claim exists its executor_id/boot_epoch are
	// updated; if none exists one is minted (claimed_at = now) so the
	// live process is never unattributed. Passing an empty executorID
	// keeps the legacy clear semantics by releasing the active claim
	// with outcome 'requeued'. Both identity columns always travel
	// together. The admin pool is the right door because the spawner
	// goroutine that stamps it holds no JWT claims. No app-pool variant —
	// ownership is a system concern, never request-scoped.
	//
	// Returns the claim row this call wrote or minted. The release arm
	// (executorID == "") answers (nil, nil) when there was no active claim to
	// release — the guard declining, not a miss; the update-or-mint arm always
	// writes something and always returns it.
	SetExecutorSystem(ctx context.Context, orgID, conversationID, executorID string, bootEpoch int64) (*domain.ExecutorClaim, error)

	// SetWorktreePath writes conversations.worktree_path. Set as the
	// spawner finishes worktree setup (GitHub PR clone, Jira
	// run-root creation).
	//
	// Returns the conversation row as it reads immediately after this call —
	// same shape as Get. ErrNoSuchConversation if conversationID names no row
	// in the org.
	SetWorktreePath(ctx context.Context, orgID, conversationID, path string) (*domain.Conversation, error)

	// MarkFailedIfActive flips a conversation to 'failed' iff it hasn't
	// already reached a terminal state, releasing the active claim (if
	// any) with outcome 'failed'. The delegate spawner's
	// failConversation path uses this so a racing terminal write
	// (cancel, completion) isn't clobbered. Returns
	// ok=false (no error) if the row is already terminal; the
	// caller logs and continues — the racing path's terminal
	// status stands.
	//
	// `open` is intentionally NOT in the protected set, and it is safe
	// because of who can reach an `open` row with this write. A parked
	// conversation is only ever driven again by a wake that flips it to
	// `running` before any engagement could fail it, and the engagement
	// that parked it released its claim in that same park — so its own late
	// failure meets the claim fence (the ForClaimSystem twin refuses it),
	// never this predicate. What is left is a claimless writer failing a
	// conversation it still holds, and there a park that never took a
	// durable snapshot is not a conversation that can be left resumably open,
	// so failing it is correct — and the cleanup then tears the worktree
	// down.
	//
	// failureKind is the machine-readable failure discriminator
	// (domain.ConversationFailureKind vocabulary); "" → NULL (unclassified).
	MarkFailedIfActive(ctx context.Context, orgID, conversationID, failureKind string) (bool, error)

	// --- Queries ---

	// Get returns a single agent conversation by ID, or nil if absent — any
	// conversation type, not just delegation (subagent rows hydrate the
	// same shape). MemoryMissing is derived from a LEFT JOIN
	// to conversation_memory; ClaimedAt/Attempts/ExecutorID derive from claims per
	// the interface doc. The accounting fields are derived too:
	// TotalCostUSD + the four token fields are SUMs over the messages
	// ledger, DurationMs/NumTurns SUMs over the claims' telemetry.
	Get(ctx context.Context, orgID, conversationID string) (*domain.Conversation, error)

	// ListForTask returns all conversations for a given task, ordered
	// started_at DESC. MemoryMissing + claim fields derived per Get.
	ListForTask(ctx context.Context, orgID, taskID string) ([]domain.Conversation, error)

	// List is the conversations resource's list read: one page of the
	// conversations matching filter plus the unpaged total, ordered
	// (task_id, started_at DESC, id). The Board's aggregated conversation
	// fetch names its tasks and groups the flat result by TaskID, so a board
	// with N tasks costs one read instead of N; the shell's live rail names
	// no task at all and asks the resource-wide question ("how many are
	// running") a task-keyed read cannot express.
	//
	// The order is total, so a windowed read's pages partition the result set
	// and each task's conversations stay contiguous within it. A zero ListOpts.Limit
	// means "no window", which is what the internal callers that need every
	// conversation pass.
	//
	// SQLite chunks its task-id IN-list to stay under the variable limit, so an
	// UNWINDOWED read's order across distinct tasks is chunk order rather
	// than task order. A windowed read cannot chunk (a window is meaningless
	// across statements), so the SQLite impl refuses an id set larger than
	// one chunk; the HTTP route caps ids at exactly that bound, so a real
	// caller never reaches the refusal.
	//
	// MemoryMissing + claim fields derived per Get. QueuePosition is
	// projected HERE and nowhere else — a place in line is only legible
	// beside the line, so the point read leaves it nil rather than answering
	// "3rd" with nothing to be third among.
	List(ctx context.Context, orgID string, filter ConversationListFilter, opts ListOpts) ([]domain.Conversation, int, error)

	// ListPRCoherenceTargetsSystem finds delegation conversations relevant to
	// one PR event. Relevance is entity-, review-, or checkout-level; EventID
	// excludes a conversation whose same-task additive path already recorded
	// the event as injected. Admin-pool only: the coherence subscriber has no
	// viewer claims and must see every relevant team conversation in the org.
	ListPRCoherenceTargetsSystem(ctx context.Context, orgID string, query PRCoherenceTargetQuery) ([]domain.PRCoherenceTarget, error)

	// HasActiveAutoConversationForTask returns true if the task has a non-terminal
	// conversation with trigger_type='event'. Manual delegations are intentionally
	// excluded (manual is decoupled from the queue). Used by the router's
	// per-task firing gate.
	//
	// The task is the unit, not the entity: a task IS one situation needing
	// attention (that is what its dedup key means), so two tasks on one pull
	// request may each have an agent in flight. Keying this on the entity
	// also meant one conversation parked indefinitely — a stop freezes its
	// blueprint 'running' by design — held the gate shut for every other
	// situation on that entity.
	HasActiveAutoConversationForTask(ctx context.Context, orgID, taskID string) (bool, error)

	// ActiveIDsForTask returns the IDs of conversations on the task that
	// haven't reached a terminal state. Used by the task routes'
	// disposition cascade to enumerate the conversations to stop.
	ActiveIDsForTask(ctx context.Context, orgID, taskID string) ([]string, error)

	// ListParkedWorktreePathsSystem returns the worktree_path of every
	// conversation parked in `open` with a non-empty
	// worktree_path, via the admin pool in Postgres (the startup sweep
	// reads it before any JWT-claims context exists). Read at startup so
	// the worktree-cleanup sweep preserves a parked conversation's warm workspace
	// (worktree dir + session JSONL) as the fast resume path. A swept
	// entry still resumes via snapshot rehydrate, so this is an
	// optimization, not a correctness gate.
	ListParkedWorktreePathsSystem(ctx context.Context, orgID string) ([]string, error)

	// HasActiveAutoConversationForTaskSystem mirrors HasActiveAutoConversationForTask
	// but routes through the admin pool in Postgres. The router's
	// per-task firing gate consumes this from its eventbus subscriber
	// goroutine, which has no JWT-claims context.
	HasActiveAutoConversationForTaskSystem(ctx context.Context, orgID, taskID string) (bool, error)

	// ActiveAutoConversationIDForTaskSystem returns the ID of the task's active
	// event-triggered conversation, or "" when none. Same predicate as
	// HasActiveAutoConversationForTaskSystem (trigger_type='event', non-terminal);
	// if the at-most-one-active-auto-run-per-task invariant is ever
	// violated, returns the most recently created. Admin pool only — the
	// router's firing gate is the sole consumer, from the same claims-less
	// background goroutine as the Has* sibling. It returns the id rather
	// than a bool because a busy gate is the additive-injection path, which
	// needs the conversation to fold the new event into.
	ActiveAutoConversationIDForTaskSystem(ctx context.Context, orgID, taskID string) (conversationID string, err error)

	// ActiveIDsForTaskSystem mirrors ActiveIDsForTask but routes through
	// the admin pool in Postgres, for a claims-less background caller.
	//
	// No production caller today: the router's task-close cascade used to
	// enumerate here, and now takes the same set from the close transaction
	// itself (TaskStore.CloseWithConversationCancelIntentSystem) so the
	// conversations it stops are the conversations it stamped. Kept as the
	// admin-pool arm of a pair whose app-pool half is live, and covered by
	// the store conformance.
	ActiveIDsForTaskSystem(ctx context.Context, orgID, taskID string) ([]string, error)

	// ActiveIDsForTeamSystem returns the IDs of every active conversation owned by the
	// team (conversations.team_id = teamID), using the same active set as
	// ActiveIDsForTask: status NOT IN ('completed','failed').
	// This is the team-archive force-stop cascade's enumeration, the
	// team-scoped sibling of ActiveIDsForTaskSystem — each returned id is
	// passed to spawner.StopConversationAndCancelBlueprint, which hard-kills a
	// live process or parks a conversation that has none.
	// Admin pool / org-scoped: archive runs from an org-admin
	// handler whose caller may not be a member of the team, so the team-visibility
	// RLS would hide the rows on the app pool.
	ActiveIDsForTeamSystem(ctx context.Context, orgID, teamID string) ([]string, error)

	// EntitiesWithOpenConversations returns the subset of entityIDs that have at
	// least one conversation currently in the `open` state (a turn ended without
	// a conclusion). Drives the factory snapshot's idle badge.
	EntitiesWithOpenConversations(ctx context.Context, orgID string, entityIDs []string) (map[string]struct{}, error)

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
	// InsertMessage/InsertMessageSystem/Messages/MessagesForConversations below serve
	// today's readers (the SDK runtime's live stream, the UI transcript
	// endpoints, spend sums) and are unchanged in observable behavior.
	// ListForAssemblySystem/MarkDeliveredForClaimSystem/SetWindowStateSystem
	// exist for the native loop (P1+): assembly is a pure function of
	// messages rows and nothing else — no conversation-level or process-level
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

	// Messages returns the conversation's messages for display, ordered by the same
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
	Messages(ctx context.Context, orgID, conversationID string) ([]domain.Message, error)

	// MessagesSince is Messages restricted to rows above a watermark: the
	// same display read with `id > sinceID`. It exists so a client holding a
	// partial transcript can repair it — a websocket frame is a hint, never
	// the only path to a row, and the RunStation's transcript is otherwise
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
	MessagesSince(ctx context.Context, orgID, conversationID string, sinceID int) ([]domain.Message, error)

	// MessagesWindow is Messages with a window: the same display read, the
	// same visibility filter, bounded.
	//
	// The transcript is a sync/tail read, not a browse — a client follows it
	// forward from a watermark — so it keeps its own window type instead of
	// the ListOpts offset the list contract uses. An offset into a growing
	// append-only stream renumbers itself under the reader.
	//
	//   - w.SinceID > 0: rows strictly after that id, oldest-first. The
	//     tail-follow direction; a full page means more have already landed.
	//   - w.BeforeID > 0: the NEWEST rows strictly before that id, still
	//     returned oldest-first. This is how history pages backward: a client
	//     that opened on the tail walks toward the beginning.
	//   - neither: the newest w.Limit rows, oldest-first — the page a client
	//     opens on.
	//   - w.Limit 0: no bound, for internal callers that assemble a whole
	//     transcript. A route always passes one.
	//
	// SinceID and BeforeID are alternatives; setting both is a caller bug and
	// the impls apply SinceID.
	//
	// **Every direction here cuts at the OLD end, and that is what keeps a
	// tool call paired with its result.** A tool-result row is always newer
	// than the assistant row that requested it, and a client pairs the two by
	// tool_call_id across the rows it holds. Newest-N and BeforeID both drop
	// the oldest rows, so the only pair a window can break is one whose
	// assistant row it also dropped — and a renderer walking assistant rows
	// never looks for that result. Breaking a pair the other way needs a
	// window bounded at the NEW end while the caller holds rows below it: a
	// middle slice. Nothing here can express one, and a future read that
	// could — "jump to timestamp", say — would need its own answer for
	// pairing before it shipped.
	MessagesWindow(ctx context.Context, orgID, conversationID string, w MessageWindow) ([]domain.Message, error)

	// MessagesForConversations is the batched form of Messages: every message
	// for any of the given conversation IDs as one flat slice, with the same
	// withdrawn-pending exclusion and the same COALESCE(seq, id) ordering.
	// Each conversation's messages are contiguous, so the caller groups by
	// ConversationID with per-conversation order preserved; order across distinct
	// conversations is unspecified (the SQLite read chunks its IN-list). Backs
	// the Board's aggregated include=messages read. Empty conversationIDs
	// returns nil.
	MessagesForConversations(ctx context.Context, orgID string, conversationIDs []string) ([]domain.Message, error)

	// NewestAssistantToolCallsForConversations reads the tool calls carried by
	// each conversation's NEWEST assistant message, keyed by conversation id.
	// Batched over a set of ids like MessagesForConversations, and for the same
	// reason: the caller annotates a whole page of conversations at once.
	//
	// "Newest" is the same effective assembly key the display read orders on
	// (COALESCE(seq, id)), descending, so the row this answers with is the row
	// a reader sees last in the transcript. Withdrawn-pending rows are excluded
	// on the same terms as Messages — a derivation that named a row the
	// transcript hides would describe something nobody can go look at.
	//
	// Only tool_calls is selected. A message row carries content, reasoning and
	// content blocks that can each run to megabytes, and none of them is read
	// here.
	//
	// A conversation is ABSENT from the map when its newest assistant message
	// carries no tool calls, when it has no assistant message at all, and when
	// the stored tool_calls are unparseable (decoded on the same silent terms
	// as the display read's own tool_calls). Absent and empty are the same
	// answer to the only question this read is asked — what is the agent doing
	// right now — so they are not distinguished. Empty conversationIDs returns
	// nil.
	NewestAssistantToolCallsForConversations(ctx context.Context, orgID string, conversationIDs []string) (map[string][]domain.ToolCall, error)

	// ListForAssemblySystem returns every row a native loop needs to rebuild this
	// conversation's exact LLM context, ordered by the effective assembly key
	// COALESCE(seq, id). window_state='inactive' rows are excluded
	// (superseded by compaction, permanently out of the window);
	// 'elided' rows ARE included (the loop renders their deterministic stub
	// from the retained content/is_error) and so are undelivered
	// (delivered=false) rows, flagged as such via Message.Delivered —
	// the loop, not this method, decides whether a pending row is due for
	// consumption at this call site.
	//
	// Assembly purity: this reads messages and nothing else. No caller
	// may layer additional filtering that depends on conversation-level or
	// process-level state — if a future rule needs to change what gets
	// assembled, it must become a column on this table, per the epic's
	// standing rule.
	ListForAssemblySystem(ctx context.Context, orgID, conversationID string) ([]domain.Message, error)

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
	SetWindowStateSystem(ctx context.Context, orgID, conversationID string, beforeSeq float64, from, to domain.MessageWindowState) (int, error)

	// --- Admin-pool variants (`...System`) ---
	//
	// These mirror the per-method shape of the corresponding
	// app-pool methods but route through the admin pool (BYPASSRLS)
	// in Postgres. They exist for the delegate spawner goroutines —
	// the conversation-lifecycle, transcript-streaming, and post-terminal
	// bookkeeping paths that start from a request handler but
	// continue on detached contexts with no JWT-claims in scope.
	//
	// Behavior contract is identical to the non-System variants:
	// org_id stays in every WHERE clause as defense in depth, return
	// shapes are identical. The only difference is which Postgres
	// pool the statement runs on; SQLite has one connection and the
	// two variants collapse.
	GetSystem(ctx context.Context, orgID, conversationID string) (*domain.Conversation, error)
	// CompleteSystem is Complete's admin-pool twin — see Complete for the
	// return shape and miss semantics.
	CompleteSystem(ctx context.Context, orgID, conversationID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) (*domain.Conversation, error)
	// LookupOrgForConversationSystem returns the owning orgID for the given
	// conversationID, or the empty string with a nil error if no such
	// conversation exists. Used by the cmd/exec convident helper to discover the
	// conversation's tenant before any other read — at agent-subprocess cold
	// start the orgID isn't yet known, only TRIAGE_FACTORY_CONVERSATION_ID
	// has been passed in. Routes through the admin pool because the
	// agent subprocess has no JWT-claims context yet.
	LookupOrgForConversationSystem(ctx context.Context, conversationID string) (string, error)
	ParkOpenSystem(ctx context.Context, orgID, conversationID string, park Park) (bool, error)

	// SetSessionSystem is the claimless door onto sdk_session_id. Every
	// engagement holds a claim and goes through SetSessionForClaimSystem
	// instead; this one stays for a writer with no engagement in scope, and
	// for the fenced twin to delegate its write semantics to.
	//
	// Return shape and miss semantics match SetSession.
	SetSessionSystem(ctx context.Context, orgID, conversationID, sessionID string) (*domain.Conversation, error)
	// SetActiveClaimPhaseSystem writes claims.phase on the conversation's
	// ACTIVE claim — the setup/parked sub-state of a live engagement
	// (fetching, cloning, agent_starting, awaiting_credentials). Empty
	// phase clears to NULL (the agent process is live). A no-op — (nil, nil),
	// the guard declining — when the conversation has no active claim; a
	// released claim's phase is inert history and must not be rewritten.
	// Phase lives on the claim rather than the conversation because it is a
	// per-engagement fact: a retry or re-claim starts its own claim with its
	// own setup progress and never rewrites the conversation row.
	//
	// Returns the claim row this call wrote, sharing the projection
	// SetExecutorSystem returns.
	SetActiveClaimPhaseSystem(ctx context.Context, orgID, conversationID, phase string) (*domain.ExecutorClaim, error)

	// PriorClaimExecutorSystem returns the executor id of the newest claim on
	// the conversation other than claimID — the engagement that ran just
	// before the caller's. Empty (with a nil error) when the caller's claim
	// is the conversation's first, so "no predecessor" and "a predecessor on
	// an unrecorded executor" are the same answer: neither is evidence the
	// executor changed.
	//
	// The one read behind the resume notice's executor sentence, which may
	// only be said when a predecessor demonstrably ran somewhere else.
	PriorClaimExecutorSystem(ctx context.Context, orgID, conversationID, claimID string) (string, error)

	// RecordClaimSandboxStatsSystem stamps one claim's measured sandbox cost
	// — peak memory (MiB) and CPU time (µs), read from the jail's cgroup at
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
	// id is a no-op — (nil, nil), the guard declining — not an error: the
	// caller is on a best-effort teardown path where a missing row means the
	// accounting is simply lost.
	//
	// Returns the claim row this call wrote, sharing the projection
	// SetExecutorSystem returns.
	RecordClaimSandboxStatsSystem(ctx context.Context, orgID, claimID string, peakMemMB *int, cpuUsec *int64) (*domain.ExecutorClaim, error)

	// SetWorktreePathSystem is the claimless door onto worktree_path — the
	// same relationship SetSessionSystem has to its fenced twin. A setup or
	// rehydrate running as an engagement names its claim
	// (SetWorktreePathForClaimSystem); a writer that holds none, minting or
	// enqueueing a conversation no executor can have picked up yet, has no
	// ownership to assert and writes through here.
	//
	// Return shape and miss semantics match SetWorktreePath.
	SetWorktreePathSystem(ctx context.Context, orgID, conversationID, path string) (*domain.Conversation, error)

	MarkFailedIfActiveSystem(ctx context.Context, orgID, conversationID, failureKind string) (bool, error)
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

	// SetSessionForClaimSystem records the runtime's session id on the
	// conversation, refused once the engagement has been fenced out. Same
	// write as SetSessionSystem; the difference is who is allowed to make it.
	//
	// This is the resume coordinate. A fenced-out engagement's late
	// `system/init` — a zombie whose subprocess came up after a successor
	// claimed the conversation — would otherwise overwrite the id the
	// successor is resuming against, and the corruption surfaces a turn later
	// as a resume into a session that belongs to a dead process. The refusal
	// is terminal for that id: a zombie's session is discarded, never retried
	// through the unfenced door.
	//
	// Return shape matches SetSession. On Postgres a fence trip is always
	// ErrClaimReleased, never a miss — the fence passing is what guarantees a
	// row to return.
	SetSessionForClaimSystem(ctx context.Context, orgID, conversationID, claimID, sessionID string) (*domain.Conversation, error)

	// SetExecutorForClaimSystem stamps this engagement's executor identity on
	// its own claim — the go-live confirmation, made once the agent process
	// actually exists — refused once the claim is released.
	//
	// The unfenced twin resolves "the conversation's active claim" and mints
	// one when there is none, which is what makes it unsafe for an engagement
	// to call: setup can outlast a claim, so a conversation reaped mid-clone whose
	// process then comes up would re-stamp the SUCCESSOR's claim with a dead
	// executor's id and boot epoch. Nothing reads that column back to the
	// process, so the corruption is silent until the reaper reads it and
	// declares a live engagement's executor lost.
	//
	// Two properties, and only one of them is the fence.
	//
	// It writes the claim the caller NAMED and mints nothing — on both
	// dialects, because that is contract rather than enforcement. An
	// engagement holding a claim needs no mint (ClaimNextConversation made it
	// atomically, in the same statement that reserved the row), and one
	// holding none is not entitled to invent ownership; a call naming a claim
	// that isn't there must write nothing at all rather than conjure a row or
	// land on whichever claim happens to be active.
	//
	// It REFUSES a released claim with ErrClaimReleased on Postgres. Local
	// mode no-ops instead, under the standing N=1 exemption — the write is
	// equally absent either way, and there is no rival executor for the error
	// to protect anyone from.
	//
	// Returns the claim row this call wrote. Local mode's no-op arm answers
	// (nil, nil) — the guard declining, the same shape RecordClaimSandboxStatsSystem
	// uses for an unknown claim id.
	SetExecutorForClaimSystem(ctx context.Context, orgID, conversationID, claimID, executorID string, bootEpoch int64) (*domain.ExecutorClaim, error)

	// SetWorktreePathForClaimSystem records where this engagement's workspace
	// landed, refused once it no longer owns the conversation. Same write as
	// SetWorktreePathSystem.
	//
	// Setup is the slow part of an engagement — a clone or a cold rehydrate can outlast
	// the claim that started it — so this is the write most likely to arrive
	// late. The path is host-local and the successor may be running on a
	// different host, so a zombie's stamp points the conversation's resume at a
	// directory that does not exist where the work is now happening.
	//
	// Return shape matches SetSessionForClaimSystem.
	SetWorktreePathForClaimSystem(ctx context.Context, orgID, conversationID, claimID, path string) (*domain.Conversation, error)

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
	//
	// Exempt from the returned-row rule: it flips a batch of pending-input
	// rows delivered in one statement, so there is no single row a return
	// value could name.
	MarkDeliveredForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, ids []int, subtype string) error

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
	//
	// Exempt from the returned-row rule: this writes up to two inserted rows,
	// a batch flip of an arbitrary span to window_state='inactive', and a
	// re-seq of every queued row ahead of the result — there is no single row
	// a return value could name. Both replyRow/resultRow's assigned IDs are
	// already written back to the caller's own pointers, which is the
	// equivalent of "the row it persisted" for a write with more than one row.
	CompactForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, replyRow, resultRow *domain.Message, inactiveIDs []int) error

	// SettleCompactionRequestForClaimSystem records a discarded warm
	// compaction attempt on the request row that asked for it: the failed
	// call's token usage and cost (the reply itself is never inserted — a
	// botched summarize attempt is not a conversation event, but its dollars
	// are real and the ledger is SUM(messages.cost_usd) over messages alone),
	// plus a machine-readable reason merged into the row's metadata under
	// "compaction_failure". costUSD nil leaves the column NULL (unpriceable
	// model), never 0.
	//
	// Returns the settled message row (requestID), sharing Messages' column
	// list and scanner. ErrNoSuchMessage if requestID names no row on this
	// conversation — unreachable once the fence passes on Postgres (the
	// caller only ever names a request row its own earlier insert produced),
	// kept because the write is still id-keyed against a value it did not
	// itself resolve.
	SettleCompactionRequestForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, requestID, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int, costUSD *float64, reason string) (*domain.Message, error)

	// CompleteForClaimSystem is Complete driven by the engagement that ran
	// the invocation: same status flip, cost settlement, and claim release,
	// refused outright when claimID is already released. The claim it
	// releases is its own by construction — a fenced call can only reach the
	// release with the claim it validated.
	//
	// Return shape matches Complete; the fence passing rules out
	// ErrNoSuchConversation the same way it does for every other
	// ForClaimSystem write.
	CompleteForClaimSystem(ctx context.Context, orgID, conversationID, claimID, status string, costUSD float64, durationMs, numTurns int, resultSummary, outcome, outcomeReason, failureKind string) (*domain.Conversation, error)

	// MarkFailedIfActiveForClaimSystem is MarkFailedIfActive driven by the
	// engagement: the infra-failure terminal, refused once the engagement
	// has been fenced out. ok=false keeps its existing meaning (the row was
	// already terminal); a fenced-out caller gets ErrClaimReleased instead,
	// which is a different thing and must not be treated as a lost race.
	MarkFailedIfActiveForClaimSystem(ctx context.Context, orgID, conversationID, claimID, failureKind string) (bool, error)

	// ParkOpenForClaimSystem is ParkOpen driven by the engagement itself,
	// refused once it has been fenced out. Every park an executor writes
	// comes through here, deliberate or not: the self-park on its own
	// cancelled context, and the idle park a turn that simply ended produces.
	//
	// The idle one has the weaker story and still needs the fence — a zombie
	// idle-parking a conversation a successor is mid-turn on flips a running
	// conversation to `open` and hands the queue a row somebody is already driving.
	//
	// The unfenced twin stays, and is what a USER-initiated cancel uses. That
	// distinction is the whole reason both exist: a person cancelling a conversation
	// is deliberately overriding whichever executor holds it, so their write
	// must not be gated on ownership, while an executor parking itself is
	// only entitled to end a conversation it still owns. Reaching for the unfenced
	// version from an engagement path is how the cancel route around this
	// fence gets rebuilt.
	ParkOpenForClaimSystem(ctx context.Context, orgID, conversationID, claimID string, park Park) (bool, error)

	// SetClaimPhaseSystem writes claims.phase on one named claim — the
	// claim-keyed sibling of SetActiveClaimPhaseSystem, for the engagement
	// reporting its own setup progress. Empty phase clears to NULL. The
	// conversation is bound as well as the claim: the phase an engagement reports
	// must not be able to land on an engagement driving a different one.
	//
	// Returns the claim row this call wrote, sharing the projection
	// SetExecutorSystem returns. Fenced on both dialects: a call naming a
	// claim that is gone or released is refused with ErrClaimReleased, and
	// the engagement reporting it has lost the conversation — the pre-spawn
	// phase write reads the refusal as its last chance to not launch a
	// runtime into a conversation somebody else now owns.
	SetClaimPhaseSystem(ctx context.Context, orgID, conversationID, claimID, phase string) (*domain.ExecutorClaim, error)

	// LastAgentActivityAtSystem returns the created_at of the conversation's most
	// recent non-user messages row (role <> 'user') — the "agent last
	// ran" watermark the artifact-change feedback ledger derives
	// against. ok=false (zero time) when the conversation has no agent messages
	// yet, so the caller falls back to the conversation's start. User messages
	// are excluded so a just-recorded resume message can't poison the
	// watermark, and the agent's own messages advance it past anything injected
	// live. Admin pool: the resume path runs on a detached goroutine with no
	// JWT claims.
	LastAgentActivityAtSystem(ctx context.Context, orgID, conversationID string) (at time.Time, ok bool, err error)

	// ListReapableSnapshotKeysSystem returns the (org, blueprint_run_id) of
	// every blueprint_run all of whose snapshot-bearing conversations — parked `open` or
	// any `completed` terminal, matching what the write side snapshots — last
	// parked or concluded before cutoff. These are the workspace snapshot keys
	// the retention reaper may safely drop. A blueprint_run with any such
	// conversation still within the TTL is omitted (its shared blob is still
	// wanted). The timestamp is COALESCE(parked_at, completed_at, started_at):
	// parked_at tracks an open conversation's last park (stamped by MarkOpen,
	// cleared by the resume flips, so a repeatedly-resumed long-lived
	// conversation is keyed off its most recent park rather than its initial
	// start), completed_at covers the terminals, and started_at is a legacy
	// fallback. `failed` is absent on purpose — a failed conversation's blob is
	// dropped at the failure, not aged out. System-wide / no org scoping — the
	// retention sweep is a maintenance job that spans tenants; the admin pool
	// is the right door (BYPASSRLS) since the reaper holds no JWT claims.
	ListReapableSnapshotKeysSystem(ctx context.Context, cutoff time.Time) ([]domain.SnapshotReapKey, error)

	// ListEvictableWorkspacesSystem returns every snapshot key whose warm
	// on-disk workspace tree may be reclaimed, with the worktree paths its
	// conversations recorded. The predicate is the retention sweep's grouping
	// plus the two things that make deleting a *tree* safe where deleting a
	// blob is not:
	//
	//   - Every snapshot-bearing conversation on the key (parked `open` /
	//     `completed`) last parked or concluded before cutoff, grouped by
	//     (org_id, blueprint_run_id) exactly as ListReapableSnapshotKeysSystem
	//     groups — a blueprint's steps share one tree.
	//   - No conversation on the key holds an active claim. Steps share the
	//     tree, so a sibling step's live engagement is working in the very
	//     directory this would delete.
	//   - The key's workspace_snapshots row says `written`. A tree whose
	//     snapshot is pending, failed, or absent is the ONLY copy of the
	//     agent's uncommitted work, and the caller confirms the blob itself
	//     before removing anything.
	//
	// Reads across tenants with no org scoping, like the retention sweep it
	// sits beside: the caller is an executor-side maintenance job with no JWT
	// claims, so the admin pool is the right door.
	ListEvictableWorkspacesSystem(ctx context.Context, cutoff time.Time) ([]domain.EvictableWorkspace, error)

	// HasActiveClaimForBlueprintRunSystem reports whether any conversation
	// under blueprintRunID has a live claim — i.e. whether an executor is
	// engaged on the shared workspace tree right now.
	//
	// It exists for the re-check the eviction sweep takes immediately before
	// removing a tree, under the same per-key serialization the workspace
	// paths hold: the enumeration above is a snapshot of a moment, and a
	// dispatcher in the same process can claim a conversation on the key in
	// between. Admin pool, for the same claims-less caller.
	HasActiveClaimForBlueprintRunSystem(ctx context.Context, orgID, blueprintRunID string) (bool, error)

	// TokenTotalsSystem sums token usage across all assistant messages
	// in a conversation (Model is MAX(model), preserving last-wins-alphabetically),
	// via the admin pool in Postgres. Consumed by agentmeta.Build, which
	// formats the agent-metadata footer from contexts that don't carry
	// JWT claims (delegate-spawned agent subprocesses calling
	// `triagefactory exec gh pr-create`, server post-approval
	// submit paths). The admin pool keeps the
	// footer-building utility from having to construct a synthetic-
	// claims tx just to read one aggregate row.
	TokenTotalsSystem(ctx context.Context, orgID, conversationID string) (*domain.TokenTotals, error)

	// BlueprintSiblingCostUSDSystem sums the messages ledger's cost_usd
	// stamps across every conversation in blueprintRunID EXCEPT
	// excludeConversationID. agentmeta.Build adds this to the authoring
	// conversation's own cost so a multi-step blueprint's published review/PR
	// discloses the total spend across all steps, not just the step that
	// authored it. Routes through the admin pool in Postgres — the footer
	// builds from claims-less contexts (agent subprocess, post-approval
	// submit).
	BlueprintSiblingCostUSDSystem(ctx context.Context, orgID, blueprintRunID, excludeConversationID string) (float64, error)

	// BlueprintSiblingDurationMsSystem sums the claims' duration_ms
	// telemetry across every conversation in blueprintRunID EXCEPT
	// excludeConversationID. agentmeta.Build adds this to the authoring
	// conversation's own duration so a multi-step blueprint's published review/PR
	// discloses the total time spent across all steps, not just the step
	// that authored it — the time analog of BlueprintSiblingCostUSDSystem.
	// Routes through the admin pool in Postgres (footer builds from
	// claims-less contexts).
	BlueprintSiblingDurationMsSystem(ctx context.Context, orgID, blueprintRunID, excludeConversationID string) (int, error)
}
