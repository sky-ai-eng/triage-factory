package domain

import "time"

// RunPendingInput is the durable half of resume-by-enqueue (TFAC-585): the
// message text a user sent to a parked/terminal-resumable run, recorded
// before the run's SAME row is re-queued as ordinary claimable work. The
// claiming executor's resume path consumes it (DELETE ... RETURNING) and
// delivers it as the turn's input, exactly once. See
// internal/db.RunPendingInputStore and
// docs/specs/horizontal-scaling/README.md §5.2.
type RunPendingInput struct {
	RunID     string
	OrgID     string
	Message   string
	UserID    string
	CreatedAt time.Time
}
