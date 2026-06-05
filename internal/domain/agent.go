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
//   - RunOutcomeYield    → hand to the user; the run parks in awaiting_input.
//
// runs.outcome persists the parsed value; the blueprint orchestrator's step
// advancement and the later queue work read it.
type RunOutcome string

const (
	RunOutcomeContinue RunOutcome = "continue"
	RunOutcomeFinish   RunOutcome = "finish"
	RunOutcomeAbort    RunOutcome = "abort"
	RunOutcomeYield    RunOutcome = "yield"
)

// Valid reports whether o is one of the four recognized outcomes.
func (o RunOutcome) Valid() bool {
	switch o {
	case RunOutcomeContinue, RunOutcomeFinish, RunOutcomeAbort, RunOutcomeYield:
		return true
	}
	return false
}

// AgentRun represents a delegated agent execution.
type AgentRun struct {
	ID            string
	TaskID        string
	PromptID      string // FK to prompts.id — which prompt was used for this run
	Status        string // lifecycle: "initializing" | "cloning" | "fetching" | "worktree_created" | "agent_starting" | "running"; terminal: "completed" | "failed" | "cancelled" | "task_unsolvable" | "pending_approval" | "taken_over"
	Model         string
	StartedAt     time.Time
	CompletedAt   *time.Time
	TotalCostUSD  *float64
	DurationMs    *int
	NumTurns      *int
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

	// CreatorUserID is the users.id of the human who initiated this
	// run (SKY-261 D-Claims). Set for manual runs (swipe-delegate /
	// drag-to-Agent / factory drop); empty / NULL for trigger-
	// spawned runs where no human asked for the work. The schema
	// CHECK pairs this with trigger_type: 'manual' ↔ non-NULL,
	// 'event' ↔ NULL. Same shape SKY-262's system_rows_nullable
	// migration introduced for prompts / task_rules / etc.
	CreatorUserID string

	BlueprintRunID     string `json:"blueprint_run_id,omitempty"`     // FK to blueprint_runs.id — populated for runs that are a step inside a multi-step blueprint
	BlueprintStepIndex *int   `json:"blueprint_step_index,omitempty"` // 0-based step index within the blueprint; nil for non-blueprint-step runs

	// Attempts is the run-queue claim counter: how many times the dispatcher
	// has claimed this run row (mirrors event_queue.attempts). Bumped by
	// RunQueueStore.ClaimNextRun; the dispatcher reads it to fail a poison run
	// out of the queue once it crosses the retry budget. 0 for never-queued runs.
	Attempts int `json:"attempts,omitempty"`
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
