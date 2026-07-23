package domain

import "time"

// CuratorRequest is a WIRE DTO only: the frozen JSON shape of one curator
// chat turn as the frontend consumes it (GET history, WS status updates).
// There is no curator_requests table behind it — a turn is a plain user
// messages row on the curator conversation, and the server synthesizes this
// shape from that row plus the claim that executed it. ID is the user
// message's integer id rendered as a decimal string.
type CuratorRequest struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Status    string `json:"status"` // queued | running | done | cancelled | failed
	UserInput string `json:"user_input"`
	// CreatorUserID is the requesting user — the user message row's user_id.
	CreatorUserID string  `json:"creator_user_id,omitempty"`
	ErrorMsg      string  `json:"error_msg,omitempty"`
	CostUSD       float64 `json:"cost_usd"`
	DurationMs    int     `json:"duration_ms"`
	NumTurns      int     `json:"num_turns"`
	// Token breakdown, from the executing claim's per-engagement accounting
	// (the SUM over the claim's messages, stamped at release).
	InputTokens         int        `json:"input_tokens"`
	OutputTokens        int        `json:"output_tokens"`
	CacheReadTokens     int        `json:"cache_read_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens"`
	StartedAt           *time.Time `json:"started_at,omitempty"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

// IsTerminal reports whether the turn has reached a final status.
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

// CuratorContextChange is one queued "the world changed since the agent
// last saw it" delta: an undelivered 'injection:context' messages row on a
// curator conversation, with the change payload riding the row's metadata
// ({"change_type","baseline_value"}). The dispatch consumes every
// undelivered row at turn start (delivered flips true), renders a hidden
// [system note] diffing each baseline against the project's current value,
// and flips the rows back to undelivered when the turn fails or is
// cancelled so the deltas aren't lost.
//
// BaselineValue is JSON-encoded — an array for pinned_repos, a scalar (or
// JSON null) for the tracker keys. The renderer diffs the baseline against
// the project's current value at consume time, so A→B→A round-trip PATCHes
// naturally collapse to "no change."
type CuratorContextChange struct {
	// MessageID is the injection row's messages id — remembered by the
	// dispatch so a failed/cancelled turn can flip exactly these rows back.
	MessageID     int64  `json:"message_id"`
	ChangeType    string `json:"change_type"`
	BaselineValue string `json:"baseline_value"`
}

// CuratorMessage is a WIRE DTO only: the frozen JSON shape of one
// agent-side chat row as the frontend consumes it (GET history, the
// curator_message WS push). Behind it is a plain messages row on the
// curator conversation; RequestID carries the owning turn's id (the user
// message id as a decimal string), which the server/sink stamps at
// synthesis time.
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

	// Reasoning and ContentBlocks mirror domain.Message's fields of the
	// same name — see that type's doc comment. Curator turns run through the
	// same SDK stream parser as delegated runs, so they carry the same
	// fidelity data; nil when the message has none.
	Reasoning     []ReasoningDetail `json:"reasoning,omitempty"`
	ContentBlocks []ContentBlock    `json:"content_blocks,omitempty"`
}
