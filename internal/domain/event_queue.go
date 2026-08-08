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
	// Traceparent is the W3C trace context of whoever enqueued this row —
	// the envelope header that lets the routing of an event be tied back to
	// the poll cycle that emitted it. Empty is the normal state (tracing
	// off, an untraced producer) and reads back as NULL; the consumer LINKS
	// to it rather than descending from it, because one cycle emits many
	// events that route later and possibly elsewhere.
	Traceparent string `json:"traceparent,omitempty"`
}

// Status values for QueuedEvent.Status.
const (
	QueuedEventStatusPending    = "pending"
	QueuedEventStatusProcessing = "processing"
	QueuedEventStatusDone       = "done"
	QueuedEventStatusFailed     = "failed"
)

// FailedEvent is one parked 'failed' queue row as the operator surface
// renders it: enough to recognize what was dropped and decide whether to
// put it back, and nothing more.
//
// A parked row is not a retry still in flight. The event is durably
// recorded — its audit row committed in the same transaction as the queue
// row — but the routing obligations it carried (task mint/bump, owner
// resolution, trigger firing, close processing) burned the whole attempt
// budget and will never run on their own. Nothing re-drives it: the
// tracker's snapshot advanced when the event was minted, so the transition
// that produced it does not re-emit. An operator requeue is the only path
// back, which is why these rows are retained rather than pruned.
type FailedEvent struct {
	ID        int64  `json:"id"`
	EventType string `json:"event_type"`
	// Entity* denormalize the entity the event was about, for display —
	// "PR owner/repo#18" reads as something an operator can act on where a
	// bare uuid does not. All four are empty when the row carries no
	// entity, or when the entity row is gone: a diagnostics list that
	// dropped a row because its entity vanished would hide exactly the
	// case worth looking at.
	EntityID       string `json:"entity_id,omitempty"`
	EntitySource   string `json:"entity_source,omitempty"`
	EntitySourceID string `json:"entity_source_id,omitempty"`
	EntityTitle    string `json:"entity_title,omitempty"`
	// Attempts is the budget the row burned before parking, and LastError
	// the reason its final attempt gave.
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"last_error,omitempty"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}
