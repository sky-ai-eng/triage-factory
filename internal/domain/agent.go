package domain

import "time"

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
