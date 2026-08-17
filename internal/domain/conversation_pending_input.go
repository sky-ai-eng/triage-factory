package domain

import "time"

// ConversationPendingInput is the durable half of resume-by-enqueue (TFAC-585): the
// message text a user sent to a parked/terminal-resumable run, recorded
// before the run's SAME row is re-queued as ordinary claimable work. The
// claiming executor's resume path consumes it (DELETE ... RETURNING) and
// delivers it as the turn's input, exactly once. See
// internal/db.ConversationPendingInputStore and
// docs/for-agents/specs/horizontal-scaling/README.md §5.2.
type ConversationPendingInput struct {
	ConversationID string
	OrgID          string
	Message        string
	UserID         string
	CreatedAt      time.Time
}
