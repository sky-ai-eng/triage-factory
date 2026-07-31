package domain

import "time"

// ConversationPermission is one tool-approval prompt a conversation raised:
// which call was gated, what the agent wanted to do, and how the question was
// answered. It is the durable half of the browser permission round-trip — the
// in-memory broker still parks the agent's turn and still unblocks it, and
// this row is the record kept alongside that, never the transport.
//
// It exists because the prompt previously had no address. A pending approval
// lived in a map on one process plus one fire-once websocket frame, so a
// refresh, a second tab, or a cold board load could not learn that a healthy,
// parked agent was waiting on a human. It is also the only place an approval
// is recorded at all: an allow left no trace whatsoever, and a deny survived
// only as prose in a tool_result body.
type ConversationPermission struct {
	ID             string `json:"id"`
	OrgID          string `json:"org_id,omitempty"`
	ConversationID string `json:"conversation_id"`

	// ClaimID is the engagement that asked. Pending is derived against the
	// conversation's ACTIVE claim rather than trusted from State (see
	// PermissionStore.ListPending), so a prompt left behind by a process that
	// no longer exists reads as not-pending whether or not anything ever wrote
	// its expiry.
	//
	// Required when creating a pending prompt — a row with no claim is
	// invisible to ListPending and untouched by ExpireForClaim, so it would sit
	// at 'pending' forever with nothing able to surface or settle it. The store
	// refuses the write rather than accepting one.
	ClaimID string `json:"-"`

	// MessageID is the assistant row carrying the gated tool_use block, or nil
	// when it isn't known. It is always nil on the SDK path: the wrapper's
	// canUseTool fires from inside the SDK's tool dispatch, so the prompt is
	// raised before the assistant row it belongs to has been streamed.
	MessageID *int64 `json:"-"`

	// ToolCallID is the SDK's toolUseID for the gated call — the same id the
	// assistant message's tool_calls carry and the answering tool row echoes,
	// so a decision ties back to the exact call it authorized.
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`

	// Input is the tool input the agent asked to run with. This — not Title —
	// is what a user is actually approving, so a surface that renders the
	// prompt must render this.
	Input map[string]any `json:"input,omitempty"`

	// Title is the prompt copy the SDK already rendered ("Claude wants to read
	// foo.txt"), empty when it rendered none. A consumer must still be able to
	// build a headline from ToolName + Input.
	Title string `json:"title,omitempty"`

	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`

	RequestedAt time.Time  `json:"requested_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	DecidedBy   string     `json:"decided_by,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`

	// WaitedMs is how long the prompt stood open — milliseconds from
	// RequestedAt to DecidedAt, stamped once at resolution and nil while
	// pending. Stored rather than derived: the two timestamps can come from
	// different clocks, so read-time subtraction is wrong by whatever they
	// disagree by.
	WaitedMs *int `json:"waited_ms,omitempty"`
}

// The permission states. A prompt is minted 'pending' and reaches exactly one
// terminal: 'allowed' / 'denied' when a decision was made about it (by a human
// or by a deadline), 'expired' when nothing decided it and the engagement that
// asked went away.
const (
	PermissionStatePending = "pending"
	PermissionStateAllowed = "allowed"
	PermissionStateDenied  = "denied"
	PermissionStateExpired = "expired"
)

// The permission reasons — why the prompt reached the state it did. This
// vocabulary is the point of the table: it is what finally makes "denied by
// you" distinguishable from "timed out" from "nobody was watching", which
// prose in a tool_result cannot do (and which the agent-facing deny copy is
// deliberately free to reword without breaking anything).
const (
	// PermissionReasonUser: a human answered — allow or deny. The only reason
	// that carries a DecidedBy.
	PermissionReasonUser = "user"
	// PermissionReasonTimeout: surfaced and left unanswered for the full
	// permission window. Someone may well have been watching the whole time.
	PermissionReasonTimeout = "timeout"
	// PermissionReasonAbsent: denied after the presence-gated grace because no
	// answer-capable, focused tab was open in the run's org — nobody COULD
	// have answered.
	PermissionReasonAbsent = "absent"
	// PermissionReasonClosing: the run ended underneath the prompt — an
	// orderly teardown, settled by the process that asked, on its way out.
	//
	// Unproduced today, and not for want of the event happening: on the SDK
	// path the close-time deny happens inside the wrapper, which settles its
	// own parked promises on drain and never tells the Go broker, so a torn-
	// down run's prompt sits out the rest of its window and records a
	// timeout it didn't really have. A gate TF owns end to end is what gives
	// this a producer — there the parked wait is our goroutine on our channel
	// under our context, so a cancelled or draining run cancels it and that
	// branch IS this reason.
	//
	// Kept distinct from ClaimLost rather than folded into it: this is the
	// asker answering its own question as it shuts down, ClaimLost is a later
	// pass finding a question nobody ever answered. "We tear runs down
	// mid-prompt" and "we lose prompts on the way down" are different
	// problems with different fixes, and one reason column that can tell them
	// apart is cheaper than reconstructing the difference from timestamps.
	PermissionReasonClosing = "closing"
	// PermissionReasonClaimLost: the engagement that asked released its claim
	// with the prompt still open. Written best-effort by ExpireForClaim —
	// ListPending derives the same conclusion without it, so nothing depends
	// on it landing. Only ever stamped on a row still 'pending', so an
	// orderly Closing above wins the race with it by construction.
	PermissionReasonClaimLost = "claim_lost"
)

// ValidPermissionState reports whether s is a storable state.
func ValidPermissionState(s string) bool {
	switch s {
	case PermissionStatePending, PermissionStateAllowed, PermissionStateDenied, PermissionStateExpired:
		return true
	}
	return false
}

// ValidPermissionReason reports whether r is a storable reason. The empty
// string is valid — a pending row has no reason yet.
func ValidPermissionReason(r string) bool {
	switch r {
	case "", PermissionReasonUser, PermissionReasonTimeout, PermissionReasonAbsent,
		PermissionReasonClosing, PermissionReasonClaimLost:
		return true
	}
	return false
}

// PendingPermissionDTO is one open prompt as a surface renders it. Only what
// answering the question needs: the id to answer with, what the agent wants to
// do, and how long is left. The audit columns (who decided, why, how long they
// took) stay off this shape — they exist for a history read, and a pending row
// has none of them by definition.
type PendingPermissionDTO struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	// Input is always present, never omitted and never null — a client renders
	// the prompt by indexing it, so an absent key would be a render crash
	// rather than a thinner prompt. Unknown input is an empty object.
	Input map[string]any `json:"input"`
	Title string         `json:"title,omitempty"`
	// TimeoutMs is the deadline REMAINING, not the window the prompt was
	// granted. Sent relative for the reason the websocket frame sent it
	// relative — the client derives its dismiss TTL from the payload rather
	// than mirroring a server constant, and clock skew between the two never
	// enters into it. A prompt reconstructed after a refresh gets the time it
	// actually has left, not a fresh full window it doesn't.
	TimeoutMs   int64     `json:"timeout_ms"`
	RequestedAt time.Time `json:"requested_at"`
}

// PendingPermissionDTOs projects open prompts onto the wire shape, measuring
// each remaining deadline against now. A row with no stored expiry sends 0, so
// the client falls back to its own default rather than treating the prompt as
// already dead.
func PendingPermissionDTOs(ps []ConversationPermission, now time.Time) []PendingPermissionDTO {
	out := make([]PendingPermissionDTO, 0, len(ps))
	for _, p := range ps {
		input := p.Input
		if input == nil {
			// A nil map marshals to null, which a client indexing into would
			// throw on. Send an object it can safely read nothing out of.
			input = map[string]any{}
		}
		dto := PendingPermissionDTO{
			ToolCallID:  p.ToolCallID,
			ToolName:    p.ToolName,
			Input:       input,
			Title:       p.Title,
			RequestedAt: p.RequestedAt,
		}
		if p.ExpiresAt != nil {
			if remaining := p.ExpiresAt.Sub(now).Milliseconds(); remaining > 0 {
				dto.TimeoutMs = remaining
			}
		}
		out = append(out, dto)
	}
	return out
}
