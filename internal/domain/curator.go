package domain

import "time"

// CuratorRequest is one user→agent exchange with a project's Curator.
// One row per posted message: status flips from queued → running →
// terminal; cost / duration / num_turns are stamped at termination
// (mirrors what AgentRun does for delegated runs).
//
// The user's own input lives on UserInput here rather than as a row
// in curator_messages — that table holds only the agent's side of the
// exchange, same way run_messages does.
type CuratorRequest struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Status    string `json:"status"` // queued | running | done | cancelled | failed
	UserInput string `json:"user_input"`
	// CreatorUserID is the requesting user (curator_requests.creator_user_id).
	// The per-project goroutine reads this when dequeuing a row so each turn's
	// writes attribute to the user that sent the message.
	CreatorUserID string  `json:"creator_user_id,omitempty"`
	ErrorMsg      string  `json:"error_msg,omitempty"`
	CostUSD       float64 `json:"cost_usd"`
	DurationMs    int     `json:"duration_ms"`
	NumTurns      int     `json:"num_turns"`
	// Token breakdown captured at completion via agentproc.UsageSink
	// (TFAC-473) and aggregated onto the request — mirrors
	// system_llm_runs so the unified spend view (TFAC-472) reads tokens
	// natively for curator turns. INTEGER NOT NULL DEFAULT 0.
	InputTokens         int        `json:"input_tokens"`
	OutputTokens        int        `json:"output_tokens"`
	CacheReadTokens     int        `json:"cache_read_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// IsTerminal reports whether the request has reached a final status.
// Used by the cancel endpoint to 404 when the in-flight slot is empty
// and by the per-project goroutine to skip already-finalized rows.
func (r *CuratorRequest) IsTerminal() bool {
	switch r.Status {
	case "done", "cancelled", "failed":
		return true
	}
	return false
}

// Curator pending-context change_type vocabulary. New change types are
// added here, the PATCH handler that emits them, and the curator
// renderer that turns baseline-vs-current into a human-readable diff.
const (
	ChangeTypePinnedRepos      = "pinned_repos"
	ChangeTypeJiraProjectKey   = "jira_project_key"
	ChangeTypeLinearProjectKey = "linear_project_key"
)

// CuratorPendingContext is a queued "the world changed since the agent
// last saw it" delta — pinned-repos changed, tracker key changed, etc.
// The Curator dispatch loop drains pending rows for the
// active session at the start of each turn, renders them as a hidden
// [system note] block prepended to the user's message, and then either
// finalizes (deletes) them on a successful run or reverts (un-consumes)
// them on cancel/fail so the user's deltas are not lost on retry.
//
// BaselineValue is JSON-encoded — an array for pinned_repos, a scalar
// (or JSON null) for the tracker key columns. The renderer diffs the
// baseline against the project row's *current* value at consume time,
// so A→B→A round-trip PATCHes naturally collapse to "no change."
type CuratorPendingContext struct {
	ID                  int64      `json:"id"`
	ProjectID           string     `json:"project_id"`
	CuratorSessionID    string     `json:"curator_session_id"`
	ChangeType          string     `json:"change_type"`
	BaselineValue       string     `json:"baseline_value"`
	ConsumedAt          *time.Time `json:"consumed_at,omitempty"`
	ConsumedByRequestID string     `json:"consumed_by_request_id,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// CuratorMessage mirrors run_messages but is keyed by request_id
// instead of run_id. The on-the-wire shape is otherwise identical so
// the frontend's existing message-rendering can be reused without
// branching on which table the row came from.
type CuratorMessage struct {
	ID                  int            `json:"id"`
	RequestID           string         `json:"request_id"`
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
	CreatedAt           time.Time      `json:"created_at"`

	// Reasoning and ContentBlocks mirror domain.AgentMessage's fields of the
	// same name — see that type's doc comment. Curator turns run through the
	// same SDK stream parser as delegated runs, so they carry the same
	// fidelity data; nil when the message has none.
	Reasoning     []ReasoningDetail `json:"reasoning,omitempty"`
	ContentBlocks []ContentBlock    `json:"content_blocks,omitempty"`
}
