package domain

import "time"

// QueuedEvent is one row of the durable router event queue.
//
// The in-memory event bus drops events for slow subscribers under burst;
// the router — which persists event rows and creates tasks — must not
// lose work that way. Every github:/jira: event is enqueued here at
// ingest, atomically with its events audit row (transactional-outbox
// style), and the router drains the queue with claim semantics, marking
// each row done. Buffered-but-unprocessed work survives process
// restarts.
//
// Status lifecycle: pending -> processing -> done | failed. A transient
// failure returns a claimed row to pending for retry; a row that
// exhausts its retry budget lands in failed (a poison pill, retained for
// debugging). 'processing' rows left by a crash are reset to pending at
// boot (single worker) and replayed.
//
// Delivery is therefore at-least-once, NOT exactly-once: a crash
// mid-process (or a transient retry) replays the event, so routing must
// be idempotent. The task partial-unique dedup index is what makes a
// replay a no-op rather than a double-create.
type QueuedEvent struct {
	ID    int64  `json:"id"`
	OrgID string `json:"org_id"`
	// EventID is the FK to events.id — the audit row enqueued alongside
	// this queue row in the same transaction. The drain worker loads the
	// full event by this id to route it.
	EventID string `json:"event_id"`
	// EntityID is denormalized from the event for per-entity grouping and
	// ops visibility. Empty only for entity-less events, which are not
	// router-bound and so never enqueued.
	EntityID    string     `json:"entity_id,omitempty"`
	EventType   string     `json:"event_type"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	LastError   string     `json:"last_error,omitempty"`
	EnqueuedAt  time.Time  `json:"enqueued_at"`
	ClaimedAt   *time.Time `json:"claimed_at,omitempty"`
	ProcessedAt *time.Time `json:"processed_at,omitempty"`
}

// Status values for QueuedEvent.Status.
const (
	QueuedEventStatusPending    = "pending"
	QueuedEventStatusProcessing = "processing"
	QueuedEventStatusDone       = "done"
	QueuedEventStatusFailed     = "failed"
)
