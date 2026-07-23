package domain

import "time"

// RunOutcome is the single terminal vocabulary an agent emits in its
// completion envelope (the `outcome` field). It replaces the dual-channel
// design where single runs used a completion `status` and blueprint steps
// used a separate per-step verdict CLI:
//
//   - RunOutcomeContinue → hand off to the next blueprint stage. Non-terminal
//     steps only; the overwhelmingly common, correct choice when a step's
//     part is done.
//   - RunOutcomeFinish   → end the whole blueprint successfully and close the
//     task. On a single/terminal run this is normal completion.
//   - RunOutcomeAbort    → stop; leave the task open for a human. Carries a
//     natural-language `reason` (the old `unsolvable` folds into this).
//
// A turn that ends without one of these (prose / nothing) is not an outcome at
// all — the run is left `open` (not concluded, not executing); see
// internal/delegate. runs.outcome persists the parsed value; the blueprint
// orchestrator's step advancement and the later queue work read it.
type RunOutcome string

const (
	RunOutcomeContinue RunOutcome = "continue"
	RunOutcomeFinish   RunOutcome = "finish"
	RunOutcomeAbort    RunOutcome = "abort"
)

// Valid reports whether o is one of the recognized outcomes.
func (o RunOutcome) Valid() bool {
	switch o {
	case RunOutcomeContinue, RunOutcomeFinish, RunOutcomeAbort:
		return true
	}
	return false
}

// RunFailureKind is the machine-readable discriminator for WHY a run
// reached status='failed' — the same pattern as
// repo_profiles.clone_error_kind. It exists so the UI can render
// specific failures specifically (a memory-limit kill points at the
// knob to turn) without anything in the chain matching on message
// text: the backend classifies with errors.Is, the enum rides the
// wire, and free text stays where it always was (run_messages /
// result_summary).
//
// Persisted to runs.failure_kind (NULL === RunFailureUnclassified).
// Closed set — extend it here when a new failure genuinely needs
// distinct rendering; an unrecognized value renders as a generic
// failure, so additions are backward compatible.
type RunFailureKind string

const (
	// RunFailureUnclassified is the zero value: a failure with no
	// specific classification (infra/setup errors, legacy rows).
	// Stored as NULL; renders as today's generic failed state.
	RunFailureUnclassified RunFailureKind = ""
	// RunFailureMemoryLimit — the sandbox's per-run memory ceiling
	// killed the agent process (agentproc.ErrRunMemoryLimit in the
	// error chain). The UI pairs this with TF_RUN_MEMORY_LIMIT_MB
	// guidance.
	RunFailureMemoryLimit RunFailureKind = "memory_limit"
	// RunFailureCrash — the agent runtime process errored out
	// (nonzero exit, stream failure) for any reason other than the
	// memory ceiling.
	RunFailureCrash RunFailureKind = "crash"
	// RunFailureNoResult — the agent ended without a usable result:
	// a clean exit that never produced a result event, or an
	// envelope attempt that exhausted validation.
	RunFailureNoResult RunFailureKind = "no_result"
	// RunFailureAgentError — the agent itself reported an error
	// result (IsError terminal).
	RunFailureAgentError RunFailureKind = "agent_error"
	// RunFailureExecutorLost — the run's owning executor's registry
	// heartbeat went stale past the leader reaper's threshold and the
	// run had already exhausted TF_RUN_MAX_ATTEMPTS, so the reaper
	// terminal-failed it instead of requeuing (TFAC-586, spec §4.3).
	// A run that still had attempts left is requeued and re-claimed
	// instead — this kind only marks the case that ran out of retries.
	RunFailureExecutorLost RunFailureKind = "executor_lost"
	// RunFailureSessionLost — a resume could not continue because the
	// run's Claude session transcript was not on disk after the workspace
	// rehydrate: the parking executor snapshotted without it, or nothing
	// restored it (e.g. the executor was rebuilt and its warm copy was on
	// wiped ephemeral storage). The run is failed with an actionable reason
	// rather than handed an opaque "No conversation found" from the SDK.
	RunFailureSessionLost RunFailureKind = "session_lost"
)

// Conversation is the durable agent-context row: one row per transcript,
// regardless of surface (a delegated task run, a curator chat, a future
// interactive session or subagent). Per-engagement execution state lives on
// Claim. Field names are the legacy wire shape (direct json.Marshal,
// PascalCase) — the conversation-vocabulary wire rename is a separate,
// later change, so several fields keep run-era names on purpose.
type Conversation struct {
	// Type names the owning surface: "delegation" | "curator" |
	// "interactive" (reserved) | namespaced "subagent:<kind>" (reserved).
	// Empty on legacy hydration paths that don't select it.
	Type string `json:"type,omitempty"`
	// Runtime is the executing engine: "sdk" | "native" — a one-way
	// ratchet per conversation (the SDK can never continue a transcript
	// that ran native).
	Runtime string `json:"runtime,omitempty"`
	// Visibility mirrors conversations.visibility ("private" | "team" |
	// "org"). Curator conversations mint "private" — creator-scoped by the
	// same RLS arms delegation already used.
	Visibility string `json:"visibility,omitempty"`
	// ProjectID anchors a curator conversation to its project; empty for
	// every other type.
	ProjectID string `json:"project_id,omitempty"`
	// ParentConversationID links a subagent conversation to its spawner;
	// empty otherwise.
	ParentConversationID string `json:"parent_conversation_id,omitempty"`
	// LastRequestAt is the KV-cache warmth watermark (time of the most
	// recent provider request). ArchivedAt retires the conversation from
	// its surface's current view (the curator reset mechanism).
	LastRequestAt *time.Time `json:"last_request_at,omitempty"`
	ArchivedAt    *time.Time `json:"archived_at,omitempty"`

	ID        string
	TaskID    string
	PromptID  string // FK to prompts.id — which prompt was used for this run
	Status    string // lifecycle: "queued" | "initializing" | "cloning" | "fetching" | "worktree_created" | "agent_starting" | "running" | "open" (a turn ended without a conclusion — not executing, not concluded); terminal: "completed" | "failed" | "cancelled" | "task_unsolvable". (pending_approval was removed — approval is a derived view over the unresolved-artifact set, not a stored status.)
	Model     string
	StartedAt time.Time
	// QueuedAt is when the run last entered the queue; ClaimedAt is when the
	// dispatcher last claimed it (work actually began). Together they carry
	// the latest queue episode's dwell — the UI's queue timer — while
	// StartedAt stays the mint stamp and DurationMs stays pure working time
	// (the SDK-reported per-turn duration, never wall clock across the
	// queue). Both nil on legacy rows that predate the queue columns.
	QueuedAt     *time.Time
	ClaimedAt    *time.Time
	CompletedAt  *time.Time
	TotalCostUSD *float64
	DurationMs   *int
	NumTurns     *int

	// Token breakdown denormalized onto the run at completion — SET to the
	// full SUM over run_messages by AgentRunStore.Complete (absolute, so
	// idempotent across resumes; not additive like total_cost_usd). Plain
	// ints because the columns are INTEGER NOT NULL DEFAULT 0 (0 for a run
	// that never streamed a usage-bearing message). Mirrors
	// system_llm_runs' columns so the unified spend view (TFAC-472) reads
	// tokens natively for delegated runs. snake_case json tags match the
	// curator_requests token fields and AgentRun's own recent additions
	// (blueprint_run_id, attempts), so a direct json.Marshal stays
	// consistent. TFAC-473.
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`

	StopReason    string
	WorktreePath  string
	ResultSummary string

	// Outcome is the parsed terminal envelope `outcome` (RunOutcome
	// vocabulary), persisted by processCompletion. Empty string === SQL
	// NULL: an infra-error run (status='failed') or a blueprint step whose
	// outcome gate exhausted its retries without a valid envelope. The
	// orchestrator reads this to advance.
	Outcome string
	// OutcomeReason is the natural-language "why I stopped / what a human
	// needs to do" populated only on an abort outcome. Distinct from
	// ResultSummary (the always-present "what I did"). Empty === NULL.
	OutcomeReason string
	// FailureKind is the machine-readable failure discriminator
	// (RunFailureKind vocabulary) for a status='failed' run — see the
	// type's doc. Empty string === SQL NULL: non-failed runs, legacy
	// failed rows, and failures nothing classified.
	FailureKind   RunFailureKind
	SessionID     string // Claude Code session_id captured from `claude -p --output-format json`, used for --resume
	MemoryMissing bool   // true if the pre-complete memory-file gate was exhausted without the agent writing a memory file
	TriggerType   string // "manual" | "event" (matches runs.trigger_type / blueprint_runs.trigger_type vocabulary)
	TriggerID     string // FK to event_handlers.id — the firing trigger, inherited from the parent blueprint_run onto every step run so the llm_spend view can attribute autonomous spend by rule (TFAC-478); empty/NULL for manual runs

	// TriggeringEventID is the event instance that auto-fired this run.
	// Set only on the event path; empty (→ SQL NULL) for manual
	// runs and blueprint-step runs. Paired with TriggerID it forms the
	// runs_event_trigger_fence partial unique index, which makes
	// event-triggered auto-delegation exactly-once under the at-least-once
	// router queue: a replayed event whose first run already committed
	// conflicts on the fence and is skipped. Forward-only provenance —
	// written via AgentRunStore.CreateIfNotFiredSystem, not hydrated by Get.
	TriggeringEventID string

	// ActorAgentID is the agents.id the spawner stamped at run start.
	// Immutable audit pointer — survives later
	// config edits and agent-row deletion (SET NULL on delete). Empty
	// string = NULL on the row (run was spawned before the agent
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
	// run. Set for manual runs (swipe-delegate /
	// drag-to-Agent / factory drop); empty / NULL for trigger-
	// spawned runs where no human asked for the work. The schema
	// CHECK pairs this with trigger_type: 'manual' ↔ non-NULL,
	// 'event' ↔ NULL. Same shape the system_rows_nullable
	// migration introduced for prompts / task_rules / etc.
	CreatorUserID string

	// TeamID is the run's owning team — runs.team_id, NOT NULL (the
	// LocalDefaultTeamID sentinel in local mode, the task-derived team in
	// multi mode; EnqueueRun / Create denormalize it from the parent task
	// at insert, so it carries the team without a task hop at read time).
	// Surfaced onto RunInfo (TFAC-458) so the capture writers can stamp
	// artifacts.team_id (NOT NULL, per TFAC-455 F1) directly off the run.
	// Populated by the Get and ClaimNextRun scan paths; empty on rows
	// hydrated by projections that don't select it.
	TeamID string

	BlueprintRunID     string `json:"blueprint_run_id,omitempty"`     // FK to blueprint_runs.id — populated for runs that are a step inside a multi-step blueprint
	BlueprintStepIndex *int   `json:"blueprint_step_index,omitempty"` // 0-based step index within the blueprint; nil for non-blueprint-step runs

	// Attempts is the run-queue claim counter: how many times the dispatcher
	// has claimed this run row (mirrors event_queue.attempts). Bumped by
	// RunQueueStore.ClaimNextRun; the dispatcher reads it to fail a poison run
	// out of the queue once it crosses the retry budget. 0 for never-queued runs.
	Attempts int `json:"attempts,omitempty"`

	// OrgID is the run's owning tenant. Populated only by
	// RunQueueStore.ClaimNextRun (a cross-org system claim that returns the row
	// it reserved); the dispatcher reads it to scope every downstream store
	// call. Empty on rows hydrated by the per-org Get paths, which already
	// carry org in their call args.
	OrgID string `json:"-"`

	// ExecutorID names the executor instance that owns this run's live
	// process while it runs — stamped when the run goes live, NULL/empty
	// otherwise. At N=1 it's a single per-process instance id; the
	// forward-compat ownership hook horizontal scaling turns into the
	// lease the control plane signals an owning executor through. Empty
	// string === SQL NULL.
	ExecutorID string `json:"-"`

	// PreferredExecutorID is the placement affinity stamp (TFAC-587): the
	// capacity-weighted rendezvous winner for this run's (org, repo) key,
	// computed at enqueue and re-stamped on each blueprint-step advance. The
	// two-tier claim reads it (tier 1 = preferred equals the claiming
	// executor). Advisory: empty (→ SQL NULL) means "unowned, claimable by
	// anyone now" — placement disabled, local N=1, a non-repo key, or a
	// requeue that cleared it. Set on enqueue and while the row is queued
	// (unlike ExecutorID); read back only where placement needs it.
	PreferredExecutorID string `json:"-"`
}

// SnapshotReapKey identifies a parked workspace snapshot eligible for retention
// reaping: the owning org and the blueprint_run_id (which is the snapshot key
// id — every run is a blueprint step, so one blueprint_run shares one workspace
// blob). The retention reaper enumerates these from the DB and discards each.
type SnapshotReapKey struct {
	OrgID          string
	BlueprintRunID string
}

// Message is one transcript row: a neutral (OpenAI-shaped) API message
// owned by exactly one conversation. Field names are the frozen legacy
// wire shape — RunID carries the conversation id until the wire rename.
type Message struct {
	ID int
	// RunID is the owning conversation id (legacy field name — frozen
	// wire shape until the conversation-vocabulary wire rename).
	RunID string
	// UserID stamps the whole turn: the requesting user owns their user
	// row and the assistant/tool rows produced in response. Empty =
	// system-triggered.
	UserID string
	// ClaimID attributes the row to the executor engagement that produced
	// it; empty for rows produced outside any claim (a queued user
	// message, a pending injection). Never read by assembly.
	ClaimID             string
	Role                string // "assistant" | "tool" | "user" — see AgentRunStore's doc comment for the full allowed set incl. reserved subtypes
	Content             string
	Subtype             string // "text" | "thinking" | "tool_use" | "tool"
	ToolCalls           []ToolCall
	ToolCallID          string
	IsError             bool
	Metadata            map[string]any
	Model               string
	InputTokens         *int
	OutputTokens        *int
	CacheReadTokens     *int
	CacheCreationTokens *int
	CreatedAt           time.Time

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
	// empty-means-default convention mirrors AgentRun.TriggerType / origin
	// elsewhere in this package.
	WindowState MessageWindowState

	// Seq overrides assembly order: the effective sort key is
	// COALESCE(Seq, ID). nil for every normally-appended row — no backfill,
	// no insert-time dance. Only a synthetic insertion (a compaction result)
	// sets a value, to land between two existing rows without renumbering
	// either of them.
	Seq *float64
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

// ReasoningDetail is one entry in AgentMessage.Reasoning: a reference-only
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

// ContentBlock is one entry in AgentMessage.ContentBlocks: non-text content
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

// MessageWindowState is run_messages.window_state's app-validated vocabulary
// (see AgentRunStore's doc comment for the full assembly contract each value
// implies).
type MessageWindowState string

const (
	MessageWindowActive   MessageWindowState = "active"
	MessageWindowElided   MessageWindowState = "elided"
	MessageWindowInactive MessageWindowState = "inactive"
)

// AgentRun is the legacy name for Conversation, kept as an alias so the
// run-vocabulary consumer code (and the frozen wire shape it marshals)
// compiles unchanged until the wire rename retires it.
type AgentRun = Conversation

// AgentMessage is the legacy name for Message — same alias contract as
// AgentRun.
type AgentMessage = Message

// Claim is one executor engagement with a conversation: who drove it, when,
// with what sealed credentials, at what per-engagement cost. At most one
// active (ReleasedAt == nil) claim exists per conversation.
type Claim struct {
	ID             string `json:"id"`
	OrgID          string `json:"org_id,omitempty"`
	ConversationID string `json:"conversation_id"`
	ExecutorID     string `json:"executor_id"`
	BootEpoch      int64  `json:"boot_epoch,omitempty"`
	// CredPubKey is the per-engagement credential sidecar's X25519 public
	// key (multi-mode only; empty locally).
	CredPubKey string    `json:"-"`
	ClaimedAt  time.Time `json:"claimed_at"`
	// ReleasedAt nil = this claim is live. Stamped exactly once.
	ReleasedAt *time.Time `json:"released_at,omitempty"`
	// Outcome is how the engagement ended: "completed" | "failed" |
	// "cancelled" | "requeued" | "parked" | "reaped". Empty while live.
	Outcome string `json:"outcome,omitempty"`
	Error   string `json:"error,omitempty"`
	// Per-engagement accounting (the curator's former per-request numbers).
	CostUSD             *float64  `json:"cost_usd,omitempty"`
	DurationMs          *int      `json:"duration_ms,omitempty"`
	NumTurns            *int      `json:"num_turns,omitempty"`
	InputTokens         int       `json:"input_tokens"`
	OutputTokens        int       `json:"output_tokens"`
	CacheReadTokens     int       `json:"cache_read_tokens"`
	CacheCreationTokens int       `json:"cache_creation_tokens"`
	CreatedAt           time.Time `json:"created_at"`
}
