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
)

// AgentRun represents a delegated agent execution.
type AgentRun struct {
	ID           string
	TaskID       string
	PromptID     string // FK to prompts.id — which prompt was used for this run
	Status       string // lifecycle: "queued" | "initializing" | "cloning" | "fetching" | "worktree_created" | "agent_starting" | "running" | "open" (a turn ended without a conclusion — not executing, not concluded); terminal: "completed" | "failed" | "cancelled" | "task_unsolvable". (pending_approval was removed — approval is a derived view over the unresolved-artifact set, not a stored status.)
	Model        string
	StartedAt    time.Time
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
	// outcome gate exhausted its retries without a valid envelope. SKY-419
	// reads this to advance the orchestrator.
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
	TriggerType   string // "manual" | "event" (matches prompt_triggers.trigger_type vocabulary)
	TriggerID     string // FK to prompt_triggers.id — populated for auto runs only

	// TriggeringEventID is the event instance that auto-fired this run
	// (SKY-424). Set only on the event path; empty (→ SQL NULL) for manual
	// runs and blueprint-step runs. Paired with TriggerID it forms the
	// runs_event_trigger_fence partial unique index, which makes
	// event-triggered auto-delegation exactly-once under the at-least-once
	// router queue: a replayed event whose first run already committed
	// conflicts on the fence and is skipped. Forward-only provenance —
	// written via AgentRunStore.CreateIfNotFiredSystem, not hydrated by Get.
	TriggeringEventID string

	// ActorAgentID is the agents.id the spawner stamped at run start
	// (SKY-261 D-Claims). Immutable audit pointer — survives later
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
	// run (SKY-261 D-Claims). Set for manual runs (swipe-delegate /
	// drag-to-Agent / factory drop); empty / NULL for trigger-
	// spawned runs where no human asked for the work. The schema
	// CHECK pairs this with trigger_type: 'manual' ↔ non-NULL,
	// 'event' ↔ NULL. Same shape SKY-262's system_rows_nullable
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
}

// SnapshotReapKey identifies a parked workspace snapshot eligible for retention
// reaping: the owning org and the blueprint_run_id (which is the snapshot key
// id — every run is a blueprint step, so one blueprint_run shares one workspace
// blob). The retention reaper enumerates these from the DB and discards each.
type SnapshotReapKey struct {
	OrgID          string
	BlueprintRunID string
}

// AgentMessage represents a single message within an agent run.
type AgentMessage struct {
	ID                  int
	RunID               string
	Role                string // "assistant" | "tool"
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
}

// ToolCall represents a single tool invocation within a message.
type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}
