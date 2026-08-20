package domain

import "time"

// ConversationTiming is the narrow projection of a conversations row the fleet
// dashboard reads for its wait/duration percentiles and failure-kind rates
// (TFAC-589). Queue wait is ClaimedAt − StartedAt (StartedAt is the enqueue
// stamp); conversation duration is DurationMS (populated on terminal).
// ExecutorID/FailureKind are "" when absent;
// ClaimedAt/CompletedAt/DurationMS are nil until the conversation reaches those
// stages. A cross-org system read — the fleet console is operator-gated,
// not org-scoped.
type ConversationTiming struct {
	OrgID       string
	ExecutorID  string
	Status      string
	FailureKind string
	StartedAt   time.Time
	ClaimedAt   *time.Time
	CompletedAt *time.Time
	DurationMS  *int
}

// QueuedConversation is one currently-queued conversation's org + enqueue time, for the fleet
// queue view's oldest-waiting age and per-org share. Deliberately unwindowed
// (a long-waiting conversation is exactly the interesting one), so it is a separate read
// from the windowed ConversationTiming history.
type QueuedConversation struct {
	OrgID             string
	EnqueuedAt        time.Time
	PreferredExecutor string
}
