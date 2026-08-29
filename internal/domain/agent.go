package domain

import "time"

// ConversationOutcome is the single terminal vocabulary an agent emits in its
// completion envelope (the `outcome` field). It replaces the dual-channel
// design where single-step conversations used a completion `status` and
// blueprint steps used a separate per-step verdict CLI:
//
//   - ConversationOutcomeContinue → hand off to the next blueprint stage. Non-terminal
//     steps only; the overwhelmingly common, correct choice when a step's
//     part is done.
//   - ConversationOutcomeFinish   → end the whole blueprint successfully and close the
//     task. On a single/terminal step this is normal completion.
//   - ConversationOutcomeAbort    → stop; leave the task open for a human. Carries a
//     natural-language `reason` (the old `unsolvable` folds into this).
//
// A turn that ends without one of these (prose / nothing) is not an outcome at
// all — the conversation is left `open` (not concluded, not executing); see
// internal/delegate. conversations.outcome persists the parsed value; the blueprint
// orchestrator's step advancement and the later queue work read it.
type ConversationOutcome string

const (
	ConversationOutcomeContinue ConversationOutcome = "continue"
	ConversationOutcomeFinish   ConversationOutcome = "finish"
	ConversationOutcomeAbort    ConversationOutcome = "abort"
)

// Valid reports whether o is one of the recognized outcomes.
func (o ConversationOutcome) Valid() bool {
	switch o {
	case ConversationOutcomeContinue, ConversationOutcomeFinish, ConversationOutcomeAbort:
		return true
	}
	return false
}

// ConversationFailureKind is the machine-readable discriminator for WHY a
// conversation reached status='failed' — the same pattern as
// repositories.clone_error_kind. It exists so the UI can render
// specific failures specifically (a memory-limit kill points at the
// knob to turn) without anything in the chain matching on message
// text: the backend classifies with errors.Is, the enum rides the
// wire, and free text stays where it always was (messages /
// result_summary).
//
// Persisted to conversations.failure_kind (NULL === ConversationFailureUnclassified).
// Closed set — extend it here when a new failure genuinely needs
// distinct rendering; an unrecognized value renders as a generic
// failure, so additions are backward compatible.
type ConversationFailureKind string

const (
	// ConversationFailureUnclassified is the zero value: a failure with no
	// specific classification (infra/setup errors, legacy rows).
	// Stored as NULL; renders as today's generic failed state.
	ConversationFailureUnclassified ConversationFailureKind = ""
	// ConversationFailureMemoryLimit — the sandbox's per-claim memory ceiling
	// killed the agent process (agentproc.ErrClaimMemoryLimit in the
	// error chain). The UI pairs this with TF_CLAIM_MEMORY_LIMIT_MB
	// guidance.
	ConversationFailureMemoryLimit ConversationFailureKind = "memory_limit"
	// ConversationFailureCrash — the agent runtime process errored out
	// (nonzero exit, stream failure) for any reason other than the
	// memory ceiling.
	ConversationFailureCrash ConversationFailureKind = "crash"
	// ConversationFailureNoResult — the agent ended without a usable result:
	// a clean exit that never produced a result event, or an
	// envelope attempt that exhausted validation.
	ConversationFailureNoResult ConversationFailureKind = "no_result"
	// ConversationFailureAgentError — the agent itself reported an error
	// result (IsError terminal).
	ConversationFailureAgentError ConversationFailureKind = "agent_error"
	// ConversationFailureExecutorLost — the conversation's owning executor's
	// registry heartbeat went stale past the leader reaper's threshold and
	// the conversation had already exhausted TF_MAX_CLAIM_ATTEMPTS, so the
	// reaper terminal-failed it instead of requeuing (TFAC-586, spec §4.3).
	// A conversation that still had attempts left is requeued and re-claimed
	// instead — this kind only marks the case that ran out of retries.
	ConversationFailureExecutorLost ConversationFailureKind = "executor_lost"
	// ConversationFailureSessionLost — a resume could not continue because the
	// conversation's Claude session transcript was not on disk after the workspace
	// rehydrate: the parking executor snapshotted without it, or nothing
	// restored it (e.g. the executor was rebuilt and its warm copy was on
	// wiped ephemeral storage). The conversation is failed with an actionable reason
	// rather than handed an opaque "No conversation found" from the SDK.
	ConversationFailureSessionLost ConversationFailureKind = "session_lost"
)

// The conversations.runtime vocabulary — which engine drives a
// conversation's transcript. It is a one-way ratchet stamped at mint: a
// transcript that ran under the native loop can never be continued by the
// SDK, whose session-file state the native rows do not reconstruct.
const (
	// ConversationRuntimeSDK drives the transcript through the Claude Code
	// SDK subprocess (internal/agentproc).
	ConversationRuntimeSDK = "sdk"
	// ConversationRuntimeNative drives the transcript through the in-process
	// Go agent loop (internal/agentloop), dispatching tools into the jail.
	ConversationRuntimeNative = "native"
)

// The conversations.type vocabulary — which surface owns a conversation.
// App-validated (no schema CHECK, the house pattern for open vocabularies).
// One claim loop drives every one of them; the type is what selects the
// execution arm once a claim is minted.
const (
	// ConversationTypeDelegation is a task-driven conversation — every
	// blueprint step today.
	ConversationTypeDelegation = "delegation"
	// ConversationTypeInteractive is reserved: the taskless "pick repos and
	// type" surface. The claim predicate already admits it; nothing mints
	// one yet.
	ConversationTypeInteractive = "interactive"
)

// InjectionSubtype values discriminate a `role=user` row the system wrote
// on the agent's behalf from one a human typed. Assembly reads them (a
// steer row renders inside a keep-working envelope); display reads them to
// label the row; the native loop's turn budget reads them to tell genuine
// user input apart from its own insertions — only a human row renews the
// budget, so every loop-authored row must carry a subtype outside the
// human set (blank, the steer stamp).
const (
	// MessageSubtypeInjectionSteer marks input that was drained between
	// turns — the model was mid-work when it arrived.
	MessageSubtypeInjectionSteer = "injection:steer"
	// MessageSubtypeInjectionExecutorChanged marks the claim-time notice
	// that the workspace was restored from its last snapshot, so work done
	// during an interrupted engagement may be absent. The restore is what
	// the row means and what a re-claim dedupes on; whether the notice also
	// says the executor changed depends on the claims, and the name is the
	// stored value rather than a summary of the text.
	MessageSubtypeInjectionExecutorChanged = "injection:executor-changed"
	// MessageSubtypeInjectionNudge marks a would-stop nudge the loop
	// inserted on a hook's behalf (the artifact contract, today).
	MessageSubtypeInjectionNudge = "injection:nudge"
	// MessageSubtypeInjectionWrapUp marks the one-call-left notice asking
	// the model to write a wrap-up summary before the turn budget pauses
	// the conversation.
	MessageSubtypeInjectionWrapUp = "injection:wrap-up"
	// MessageSubtypeInjectionOutputLimit marks the notice written when a
	// model turn hit the output-token limit before producing any text or
	// tool call — the whole budget went to reasoning, so the turn recorded
	// nothing. Delivered, not pending: the next call must read it, and it is
	// a fact about the turn that just happened rather than input waiting to
	// be consumed.
	MessageSubtypeInjectionOutputLimit = "injection:output-limit"
	// MessageSubtypeInjectionCompactionRequest marks the loop's ask that the
	// model summarize the conversation so the history can be replaced. It
	// renders bare on the wire — the subtype is provenance, not an envelope —
	// and flips inactive with the span it asked to compact.
	MessageSubtypeInjectionCompactionRequest = "injection:compaction-request"
	// MessageSubtypeInjectionCompactionResult marks the machine-composed row
	// that replaces a compacted span: the preamble, the re-injected original
	// request (unanchored conversations), and the summary. The only
	// compaction text that stays in the active window.
	MessageSubtypeInjectionCompactionResult = "injection:compaction-result"
	// MessageSubtypeStopNote marks a delivered row recording why an
	// engagement stopped — a guard park, an unrecoverable provider error.
	// A statement of fact written into history, not input awaiting
	// consumption, which is why it carries no injection: prefix.
	MessageSubtypeStopNote = "stop-note"
)

// Conversation is the durable agent-context row: one row per transcript,
// regardless of surface (a delegated task conversation, a future interactive
// session or subagent). Per-engagement execution state lives on Claim. The
// status/summary projection is served through a handler-side map; the
// transcript rows cross the wire as the snake_case MessageDTO.
type Conversation struct {
	// Type names the owning surface: "delegation" | "interactive" (reserved)
	// | namespaced "subagent:<kind>" (reserved). Empty on legacy hydration
	// paths that don't select it.
	Type string `json:"type,omitempty"`
	// Runtime is the executing engine: ConversationRuntimeSDK |
	// ConversationRuntimeNative — a one-way ratchet per conversation (the
	// SDK can never continue a transcript that ran native).
	Runtime string `json:"runtime,omitempty"`
	// Visibility mirrors conversations.visibility ("private" | "team" |
	// "org").
	Visibility string `json:"visibility,omitempty"`
	// ParentConversationID links a subagent conversation to its spawner;
	// empty otherwise.
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
	// ArchivedAt retires the conversation from its surface's current view.
	// KV-cache warmth is deliberately NOT a conversation field: it derives
	// from the newest assistant message row (warmth drives elision, elision
	// changes assembly, and assembly must be a pure function of rows).
	ArchivedAt *time.Time `json:"archived_at,omitempty"`

	ID       string
	TaskID   string
	PromptID string // FK to prompts.id — which prompt was used for this conversation
	// Status is the DISPLAYED lifecycle: the stored column with the live
	// claim's phase coalesced over it, so a hydrated DTO may carry a phase
	// name here. The vocabulary and its classifiers live in conversation_status.go;
	// don't restate the names anywhere else. Approval is a derived view over
	// the unresolved-artifact set, never a stored status.
	Status    string
	Model     string
	StartedAt time.Time
	// QueuedAt is when the conversation last entered the queue; ClaimedAt is when the
	// dispatcher last claimed it (work actually began). Together they carry
	// the latest queue episode's dwell — the UI's queue timer — while
	// StartedAt stays the mint stamp and DurationMs stays pure working time
	// (the SDK-reported per-turn duration, never wall clock across the
	// queue). Both nil on legacy rows that predate the queue columns.
	QueuedAt    *time.Time
	ClaimedAt   *time.Time
	CompletedAt *time.Time

	// QueuePosition is this conversation's place in its OWN ORG's line:
	// 1-based, ordered by (started_at, id) over the org's display-`queued`
	// conversations. nil in every other state, and nil on reads that don't
	// project it — the list reads do, since the surface that renders the
	// hourglass is a list.
	//
	// Org-local is the contract, not an approximation waiting to be
	// sharpened. The dispatcher's real claim order is a placement tier plus
	// cross-org fairness (fewest-active-org first), and both are deployment
	// facts: a position computed from it would tell one tenant about another
	// tenant's load. So the derivation reads no fleet occupancy, no executor
	// identity, no placement tier, and no cross-org term, and must not grow
	// one — in a shared fleet it can therefore understate the wait, which is
	// accepted. The deployment-truthful view is the fleet console's, behind
	// the operator gate.
	QueuePosition *int

	// TotalCostUSD / DurationMs / NumTurns are derived at read time, not
	// stored: cost is the SUM of the messages ledger's cost_usd stamps,
	// duration/turns the SUM of the claims' per-engagement telemetry. nil
	// when nothing has settled yet. Wire shape unchanged from the
	// stored-column era.
	TotalCostUSD *float64
	DurationMs   *int
	NumTurns     *int

	// Token breakdown, derived at read time as the SUM over this
	// conversation's messages (plain ints — 0 for a conversation that never
	// streamed a usage-bearing message). Mirrors system_llm_runs' columns;
	// snake_case json tags keep the frozen wire shape.
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`

	// ParkReason is WHY this conversation was parked `open` — the closed
	// ParkReason vocabulary, see the type. Empty === SQL NULL: never parked,
	// or resumed since (a resume clears it, so a conversation that went on to
	// conclude doesn't still name the stop it was picked back up from).
	ParkReason    ParkReason
	WorktreePath  string
	ResultSummary string

	// Outcome is the parsed terminal envelope `outcome` (ConversationOutcome
	// vocabulary), persisted by processCompletion. Empty string === SQL
	// NULL: an infra-error conversation (status='failed') or a blueprint step whose
	// outcome gate exhausted its retries without a valid envelope. The
	// orchestrator reads this to advance.
	Outcome string
	// OutcomeReason is the natural-language "why I stopped / what a human
	// needs to do" populated only on an abort outcome. Distinct from
	// ResultSummary (the always-present "what I did"). Empty === NULL.
	OutcomeReason string
	// FailureKind is the machine-readable failure discriminator
	// (ConversationFailureKind vocabulary) for a status='failed' conversation —
	// see the type's doc. Empty string === SQL NULL: non-failed conversations,
	// legacy failed rows, and failures nothing classified.
	FailureKind   ConversationFailureKind
	SessionID     string // Claude Code session_id captured from `claude -p --output-format json`, used for --resume
	MemoryMissing bool   // true if the pre-complete memory-file gate was exhausted without the agent writing a memory file
	TriggerType   string // "manual" | "event" (matches conversations.trigger_type / blueprint_runs.trigger_type vocabulary)
	TriggerID     string // FK to event_handlers.id — the firing trigger, inherited from the parent blueprint_run onto every step's conversation so the llm_spend view can attribute autonomous spend by rule (TFAC-478); empty/NULL for manual conversations

	// TriggeringEventID is the event instance that auto-fired this
	// conversation. Set only on the event path; empty (→ SQL NULL) for manual
	// conversations and blueprint-step conversations. Paired with TriggerID it
	// forms the conversations_event_trigger_fence partial unique index, which
	// makes event-triggered auto-delegation exactly-once under the
	// at-least-once router queue: a replayed event whose first conversation
	// already committed conflicts on the fence and is skipped. Forward-only
	// provenance — written via BlueprintStore.CreateRunIfNotFiredSystem, not
	// hydrated by Get.
	TriggeringEventID string

	// ActorAgentID is the agents.id the spawner stamped when the conversation
	// started. Immutable audit pointer — survives later
	// config edits and agent-row deletion (SET NULL on delete). Empty
	// string = NULL on the row (conversation was spawned before the agent
	// bootstrap completed, or after the agent was deleted).
	ActorAgentID string

	// ActorAgentName is the display name of the actor agent, denormalized
	// from agents.display_name via a LEFT JOIN on the read projections (Get /
	// ListForTask / factory ActiveRuns) so the UI can render "Ran as: {name}"
	// without a second round-trip. Empty when actor_agent_id is NULL or the
	// referenced agent row was deleted (LEFT JOIN → NULL name); not a stored
	// column, so insert/scan paths that don't JOIN agents leave it "".
	ActorAgentName string

	// CreatorUserID is the users.id of the human who initiated this
	// conversation. Set for manual conversations (the delegate route /
	// drag-to-Agent / factory drop); empty / NULL for trigger-
	// spawned conversations where no human asked for the work. The schema
	// CHECK pairs this with trigger_type: 'manual' ↔ non-NULL,
	// 'event' ↔ NULL. Same shape the system_rows_nullable
	// migration introduced for prompts / task_rules / etc.
	CreatorUserID string

	// TeamID is the conversation's owning team — conversations.team_id, NOT NULL (the
	// LocalDefaultTeamID sentinel in local mode, the task-derived team in
	// multi mode; EnqueueConversation / Create denormalize it from the parent task
	// at insert, so it carries the team without a task hop at read time).
	// Surfaced onto ConversationInfo (TFAC-458) so the capture writers can stamp
	// artifacts.team_id (NOT NULL, per TFAC-455 F1) directly off the conversation.
	// Populated by the Get and ClaimNextConversation scan paths; empty on rows
	// hydrated by projections that don't select it.
	TeamID string

	BlueprintRunID     string `json:"blueprint_run_id,omitempty"`     // FK to blueprint_runs.id — populated for conversations that are a step inside a multi-step blueprint
	BlueprintStepIndex *int   `json:"blueprint_step_index,omitempty"` // 0-based step index within the blueprint; nil for non-blueprint-step conversations

	// Attempts counts this conversation's engagements — but WHICH engagements
	// depends on which read filled it in, because the two producers answer
	// different questions and both answers are wanted. Check the producer
	// before branching on it.
	//
	//   - ConversationQueueStore.ClaimNextConversation fills it with the CURRENT queue episode:
	//     this claim plus the consecutive hand-backs behind it ('requeued' /
	//     'reaped' — see the dialects' episodeAttemptsSQL). This is the retry
	//     budget's counter and the only meaning anything branches on
	//     (delegate.handlePreAgentFailure). An engagement that got anywhere
	//     ends the episode, so a conversation resumed four times still claims
	//     at 1.
	//   - The display reads (Get / GetSystem / the list projections) fill it
	//     with every claim the conversation has ever had, alongside the
	//     lifetime duration/turn sums it is bundled with. That is engagement
	//     history for a human, not a budget.
	//
	// Do not read one as the other. A lifetime count used as a budget is
	// exactly what made a healthy resumed conversation look like a poison
	// pill — five follow-ups and its next transient hiccup failed the
	// blueprint and took the worktree with it.
	//
	// 0 for never-claimed conversations, and on projections that don't select
	// it.
	Attempts int `json:"attempts,omitempty"`

	// OrgID is the conversation's owning tenant. Populated only by
	// ConversationQueueStore.ClaimNextConversation (a cross-org system claim that returns the row
	// it reserved); the dispatcher reads it to scope every downstream store
	// call. Empty on rows hydrated by the per-org Get paths, which already
	// carry org in their call args.
	OrgID string `json:"-"`

	// ClaimMessageID is the queued input row the claim was minted to drive —
	// the claim's mint intent, so a pickup that fails before attaching any
	// message stays attributable to its exact turn. Populated by
	// ConversationQueueStore.ClaimNextConversation for surfaces whose work unit is one queued
	// message (interactive, reserved); 0 for a delegation conversation, whose
	// work unit is the conversation itself.
	ClaimMessageID int64 `json:"-"`

	// ClaimID is the claims row minted for this engagement, populated only by
	// ConversationQueueStore.ClaimNextConversation (which returns the row it just reserved
	// along with the claim it minted for it). The dispatcher threads it into
	// the run config so teardown can stamp the engagement's measured sandbox
	// cost by id — an active-claim lookup at that point would race the
	// release. Empty on every read path that doesn't mint a claim.
	ClaimID string `json:"-"`

	// ExecutorID names the executor instance that owns this conversation's
	// live process while it runs — stamped when the conversation goes live,
	// NULL/empty otherwise. At N=1 it's a single per-process instance id; the
	// forward-compat ownership hook horizontal scaling turns into the
	// lease the control plane signals an owning executor through. Empty
	// string === SQL NULL.
	ExecutorID string `json:"-"`

	// PreferredExecutorID is the placement affinity stamp: which executor
	// should get this conversation if it can. At enqueue (and on each
	// blueprint-step advance) that is the capacity-weighted rendezvous winner
	// for the conversation's (org, repo) key; on a resume it is instead the
	// executor of the newest claim, which is the machine holding the workspace
	// tree the follow-up wants to land in. The two-tier claim reads it (tier 1
	// = preferred equals the claiming executor). Advisory: empty (→ SQL NULL)
	// means "unowned, claimable by anyone now" — placement disabled, local
	// N=1, a non-repo key, a conversation no engagement ever drove, or a
	// requeue that cleared it. Set on enqueue and while the row is queued
	// (unlike ExecutorID); read back only where placement needs it.
	PreferredExecutorID string `json:"-"`
}

// SnapshotReapKey identifies a parked workspace snapshot eligible for retention
// reaping: the owning org and the blueprint_run_id (which is the snapshot key
// id — every conversation is a blueprint step, so one blueprint_run shares one workspace
// blob). The retention reaper enumerates these from the DB and discards each.
type SnapshotReapKey struct {
	OrgID          string
	BlueprintRunID string
}

// EvictableWorkspace is one snapshot key whose warm workspace tree may be
// reclaimed from the executor holding it: every conversation sharing the key is
// at rest, none is claimed, the key aged past the eviction TTL, and the durable
// snapshot is recorded written. WorktreePaths are the distinct non-empty
// worktree_path values those conversations recorded — normally one, since a
// blueprint's steps share a tree, but a key resumed on a host with a different
// $TMPDIR records a second.
//
// The paths are candidates, not facts about this machine: the enumerating pod
// is not necessarily the pod holding the tree, so a caller stats each one
// itself and evicts only what is actually here.
type EvictableWorkspace struct {
	OrgID          string
	BlueprintRunID string
	WorktreePaths  []string
}

// ModelSynthetic is the model id the agent runtime stamps on an assistant
// message it composed itself rather than received from a provider — an API
// error notice, an interrupt/abort line, a stop-reason placeholder. It names
// no model and prices to nothing, so spend must never be attributed to it:
// the cost-settle paths skip these rows when choosing the ledger row an
// invocation's lump lands on, and the usage breakdowns exclude it the way
// they exclude a NULL model (the dollars still land in the period total).
const ModelSynthetic = "<synthetic>"

// Message is one transcript row: a neutral (OpenAI-shaped) API message
// owned by exactly one conversation. It is the in-memory row shape; the
// snake_case MessageDTO is what crosses the HTTP/WS wire.
type Message struct {
	ID int
	// ConversationID is the owning conversation id.
	ConversationID string
	// UserID stamps the whole turn: the requesting user owns their user
	// row and the assistant/tool rows produced in response. Empty =
	// system-triggered.
	UserID string
	// ClaimID attributes the row to the executor engagement that produced
	// it; empty for rows produced outside any claim (a queued user
	// message, a pending injection). Never read by assembly.
	ClaimID string
	Role    string // "assistant" | "tool" | "user" — see ConversationStore's doc comment for the full allowed set incl. reserved subtypes
	Content string
	// Subtype discriminates a row that deviates from ordinary role
	// behavior. Blank is normal — an assistant turn, a tool result, a
	// person's message — and every discriminating value is named: the
	// InjectionSubtype constants, MessageSubtypeStopNote, the artifact
	// system_note.
	Subtype             string
	ToolCalls           []ToolCall
	ToolCallID          string
	IsError             bool
	Metadata            map[string]any
	Model               string
	InputTokens         *int
	OutputTokens        *int
	CacheReadTokens     *int
	CacheCreationTokens *int
	// CostUSD is dollars settled at this row: nil = not a settlement row,
	// 0 = genuinely free. The runtime stamps an invocation's reported
	// total as one lump on the invocation's last recorded row at terminal
	// time, so a SUM over any window is the spend in that window.
	CostUSD   *float64
	CreatedAt time.Time

	// StopReason is why the provider stopped generating THIS turn, in the
	// provider's own spelling ("end_turn" / "max_tokens" / "tool_use" /
	// "refusal" / …) — the API's field name for the API's fact. Assistant
	// rows only; empty everywhere else and on any row whose stream reported
	// none, which is why "" and "unknown" stay the same thing rather than
	// being separated by a placeholder.
	//
	// It sits beside the token and cost columns because it is the same kind
	// of per-call measurement, and at turn scope because that is its scope: a
	// conversation of N turns has N of these, and the one an investigation
	// wants is the one attached to the turn it is looking at. Not an assembly
	// input — the loop classifies the reason it just received, never one read
	// back from a row.
	StopReason string

	// DurationMs is the wall-clock milliseconds of the work this row
	// represents: for an assistant row, request issued → message complete
	// (reasoning included); for a tool row, dispatch → result; nil for every
	// other role and for any row written before the runtime measured it.
	// Always a measured value the writer held both marks for — never a
	// subtraction of two rows' CreatedAt, which streaming, paging, and
	// compaction each break in their own way. nil is "not measured", 0 is
	// "measured, and it was that fast".
	//
	// It measures work, so time spent waiting on a human never lands in it:
	// a permission-gated call reads as the time it ran, not the time it
	// stood at the prompt.
	DurationMs *int

	// Reasoning is the model's persisted chain-of-thought for this message —
	// nil when the message carries none. Reference-only (see ReasoningDetail):
	// never reconstructed or reordered, only replayed verbatim.
	Reasoning []ReasoningDetail

	// ContentBlocks carries non-text content (images, files) the flat Content
	// string can't represent — nil when Content is the whole story. Applies
	// to any role: a tool row uses this for an image result.
	ContentBlocks []ContentBlock

	// Delivered is nil for "apply the schema default" (true — the message is
	// immediately part of context, exactly today's SDK-runtime behavior).
	// Only a native loop's durable pending input (a steer / follow-up not yet
	// folded into any assembly) sets this to a non-nil false. A *bool rather
	// than bool so every existing caller — none of which know about this
	// field — keeps writing the correct default instead of silently writing
	// the bool zero value (false) on every insert.
	Delivered *bool

	// WindowState is "" for "apply the schema default" (MessageWindowActive)
	// and otherwise one of the MessageWindowState vocabulary. The
	// empty-means-default convention mirrors Conversation.TriggerType / origin
	// elsewhere in this package.
	WindowState MessageWindowState

	// Seq overrides assembly order: the effective sort key is
	// COALESCE(Seq, ID). nil for a row whose arrival order IS its position —
	// no backfill, no insert-time dance. A value places the row between two
	// existing ones without renumbering either: a compaction result ahead of
	// the tail it excludes, and every tool result, which belongs under the
	// call it answers no matter what else took an id in between.
	Seq *float64
}

// MessageDTO is the single snake_case wire shape for one transcript row: the
// delegated conversation's message read/stream marshals this. It is the
// display projection of a Message row — the assembly-only columns
// (delivered / window_state / seq) never cross the wire. ConversationID
// links the row to its owning conversation.
type MessageDTO struct {
	ID                  int            `json:"id"`
	ConversationID      string         `json:"conversation_id"`
	Role                string         `json:"role"`
	Subtype             string         `json:"subtype"`
	Content             string         `json:"content"`
	ToolCalls           []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID          string         `json:"tool_call_id,omitempty"`
	IsError             bool           `json:"is_error,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	Model               string         `json:"model,omitempty"`
	InputTokens         *int           `json:"input_tokens,omitempty"`
	OutputTokens        *int           `json:"output_tokens,omitempty"`
	CacheReadTokens     *int           `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int           `json:"cache_creation_tokens,omitempty"`
	// CostUSD carries the row's settlement stamp (see Message.CostUSD): absent
	// = not a settlement row, 0 = genuinely free. A runtime that stamps as it
	// streams makes the wire rows an accumulating spend signal, which is how a
	// live surface keeps a cost readout current between reads of the
	// conversation's SUM.
	CostUSD   *float64  `json:"cost_usd,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// DurationMs is how long this row's own work took (see
	// Message.DurationMs). Pointer + omitempty so an unmeasured row is absent
	// from the JSON rather than zero: a consumer has to be able to tell "took
	// 0ms" from "nobody measured", and every row written before the runtime
	// stamped timing is the latter.
	DurationMs *int `json:"duration_ms,omitempty"`
	// StopReason is the provider's own reason for ending this turn (see
	// Message.StopReason). Omitted rather than empty when the stream reported
	// none, so a consumer can't read "not reported" as a reason of its own.
	StopReason    string            `json:"stop_reason,omitempty"`
	Reasoning     []ReasoningDetail `json:"reasoning,omitempty"`
	ContentBlocks []ContentBlock    `json:"content_blocks,omitempty"`
}

// ToDTO projects a transcript row onto the shared snake_case wire shape.
func (m Message) ToDTO() MessageDTO {
	return MessageDTO{
		ID:                  m.ID,
		ConversationID:      m.ConversationID,
		Role:                m.Role,
		Subtype:             m.Subtype,
		Content:             m.Content,
		ToolCalls:           m.ToolCalls,
		ToolCallID:          m.ToolCallID,
		IsError:             m.IsError,
		Metadata:            m.Metadata,
		Model:               m.Model,
		InputTokens:         m.InputTokens,
		OutputTokens:        m.OutputTokens,
		CacheReadTokens:     m.CacheReadTokens,
		CacheCreationTokens: m.CacheCreationTokens,
		CostUSD:             m.CostUSD,
		CreatedAt:           m.CreatedAt,
		DurationMs:          m.DurationMs,
		StopReason:          m.StopReason,
		Reasoning:           m.Reasoning,
		ContentBlocks:       m.ContentBlocks,
	}
}

// MessageDTOs projects a slice of rows onto the wire shape, preserving order.
func MessageDTOs(msgs []Message) []MessageDTO {
	out := make([]MessageDTO, len(msgs))
	for i, m := range msgs {
		out[i] = m.ToDTO()
	}
	return out
}

// ToolCall represents a single tool invocation within a message.
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ReasoningDetailType discriminates a ReasoningDetail entry, mirroring
// bifrost's BifrostReasoningDetailsType wire values.
type ReasoningDetailType string

// ReasoningDetail is one entry in Message.Reasoning: a reference-only
// replay unit for a slice of the model's persisted chain-of-thought,
// mirroring bifrost's ChatReasoningDetails shape (the subset a replay needs:
// index/type/text/signature/data). The provider keeps the canonical
// reasoning; Signature and Data are opaque tokens the API validates
// per-block on the next turn — never reconstructed or reordered here.
// Defined in domain, not imported from bifrost, so this package stays free
// of a bifrost dependency until the P1 inference package wires the real
// thing in.
type ReasoningDetail struct {
	Index     int                 `json:"index"`
	Type      ReasoningDetailType `json:"type"`
	Text      string              `json:"text,omitempty"`
	Signature string              `json:"signature,omitempty"`
	Data      string              `json:"data,omitempty"`
}

// ContentBlockType discriminates a ContentBlock's payload, mirroring
// bifrost's ChatContentBlockType wire values.
type ContentBlockType string

const (
	ContentBlockText  ContentBlockType = "text"
	ContentBlockImage ContentBlockType = "image_url"
	ContentBlockFile  ContentBlockType = "file"
)

// ContentBlock is one entry in Message.ContentBlocks: non-text content
// (an image or a file), neutral-shaped after bifrost's ChatContentBlock.
// Applies to any role — a tool row uses this for an image result the flat
// Content string can't carry. Defined in domain, not imported from bifrost,
// for the same zero-bifrost-import reason as ReasoningDetail.
type ContentBlock struct {
	Type     ContentBlockType `json:"type"`
	Text     string           `json:"text,omitempty"`
	ImageURL *ContentImageURL `json:"image_url,omitempty"`
	File     *ContentFile     `json:"file,omitempty"`
}

// ContentImageURL is a ContentBlock's image payload — a remote URL or a
// data: URI, mirroring bifrost's ChatInputImage.
type ContentImageURL struct {
	URL    string `json:"url,omitempty"`
	FileID string `json:"file_id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ContentFile is a ContentBlock's file payload, mirroring bifrost's
// ChatInputFile.
type ContentFile struct {
	FileData string `json:"file_data,omitempty"`
	FileURL  string `json:"file_url,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	Filename string `json:"filename,omitempty"`
	FileType string `json:"file_type,omitempty"`
}

// MessageWindowState is messages.window_state's app-validated vocabulary
// (see ConversationStore's doc comment for the full assembly contract each value
// implies).
type MessageWindowState string

const (
	MessageWindowActive   MessageWindowState = "active"
	MessageWindowElided   MessageWindowState = "elided"
	MessageWindowInactive MessageWindowState = "inactive"
)

// Claim is one executor engagement with a conversation: who drove it, when,
// with what sealed credentials. At most one active (ReleasedAt == nil) claim
// exists per conversation. Dollars and tokens live on Message (the ledger),
// never here.
type Claim struct {
	ID             string `json:"id"`
	OrgID          string `json:"org_id,omitempty"`
	ConversationID string `json:"conversation_id"`
	ExecutorID     string `json:"executor_id"`
	BootEpoch      int64  `json:"boot_epoch,omitempty"`
	// CredPubKey is the per-engagement credential sidecar's X25519 public
	// key (multi-mode only; empty locally).
	CredPubKey string `json:"-"`
	// Phase is the setup/parked sub-state of a LIVE engagement — one of the
	// ClaimPhase* names in conversation_status.go. Empty = the agent process is live
	// (or the claim is released — a released claim's phase is inert history).
	// Display reads coalesce it over the conversation's stored status.
	Phase     string    `json:"phase,omitempty"`
	ClaimedAt time.Time `json:"claimed_at"`
	// ReleasedAt nil = this claim is live. Stamped exactly once.
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	// Outcome is how the engagement ended: "completed" | "failed" |
	// "cancelled" | "requeued" | "parked" | "reaped". Empty while live.
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
	// Engagement telemetry the runtime reports per invocation — not
	// derivable from messages.
	DurationMs *int      `json:"duration_ms,omitempty"`
	NumTurns   *int      `json:"num_turns,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ExecutorClaim is one engagement projected for the operator fleet console's
// per-executor sandbox breakdown: the claim's identity and lifetime, its
// measured sandbox cost, and the driven conversation's terminal state.
//
// A separate projection rather than fields on Claim, deliberately. Claim is a
// wire type an org-scoped surface can serialize directly to a customer; the
// measured cost is operator-only until a pricing model exists, and hanging it
// off Claim would publish raw compute numbers to customers as a side effect of
// a fleet feature.
//
// Every measurement is a pointer because NULL means "not measured", never
// "measured zero" — a local-mode claim never had a sandbox, a pre-5.19 kernel
// has no memory.peak, and a crashed teardown recorded neither. Consumers
// render a dash, not a zero.
type ExecutorClaim struct {
	ID             string
	OrgID          string
	ConversationID string

	// ClaimedAt/ReleasedAt bound the engagement. ReleasedAt nil = still live,
	// which is also the only shape whose sandbox may still be growing.
	ClaimedAt  time.Time
	ReleasedAt *time.Time
	// Outcome is how the engagement ended ("completed" | "failed" |
	// "cancelled" | "requeued" | "parked" | "reaped"); empty while live.
	Outcome string

	// PeakMemMB / CPUUsec are the claim's end-state actuals, read from the
	// jail's cgroup at teardown. These are the billing-grade record; a
	// sampled series over the same engagement legitimately reads under them, and the
	// two are never reconciled numerically.
	PeakMemMB *int
	CPUUsec   *int64

	// Status / FailureKind come from the driven conversation (one join) — what
	// the engagement was ultimately spent on succeeding or failing at.
	Status      string
	FailureKind string
}
