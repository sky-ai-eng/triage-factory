package domain

import "time"

// QueuedEvent is one row of the durable router event queue.
//
// The in-memory event bus drops events for slow subscribers under burst;
// the router — which persists event rows and creates tasks — must not lose
// work that way. Every github:/jira: event is enqueued here at ingest,
// atomically with its events audit row (transactional-outbox style), and the
// router's worker drains the queue under the shared work-item contract
// (internal/db/workitem): a claim leases a row for a bounded time and
// charges its attempt budget, every later write on the row presents the
// lease's receipt, and an expired lease is claimable again by the next claim
// with no reset or sweeper. A failed attempt returns the row to ready with a
// retry time so the rest of the queue proceeds past it, or parks it for an
// operator once the budget is spent or the failure is permanent.
//
// Delivery is therefore at-least-once, never exactly-once: an expired lease
// replays the event, so routing is fenced to make a replay a no-op — the
// tasks dedup index, the firing replay fence, the entity's version guard.
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
	EntityID  string `json:"entity_id,omitempty"`
	EventType string `json:"event_type"`

	// The shared work-item block. Status is one of the QueuedEventStatus*
	// values; Attempt counts charged claims against MaxAttempts, the budget
	// copied onto the row at admission; NextAttemptAt is the earliest retry
	// after a requeue. The lease columns describe the current holder, and
	// LeaseGeneration is the write fence every holder operation presents.
	Status          string     `json:"status"`
	Attempt         int        `json:"attempt"`
	MaxAttempts     int        `json:"max_attempts"`
	NextAttemptAt   *time.Time `json:"next_attempt_at,omitempty"`
	LeaseGeneration int64      `json:"lease_generation"`
	LeaseOwner      string     `json:"lease_owner,omitempty"`
	LeaseEpoch      *int64     `json:"lease_epoch,omitempty"`
	LeasedAt        *time.Time `json:"leased_at,omitempty"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`
	// Cancellation intent: recorded without changing status, settled by
	// the next claim or the holder's next fenced write.
	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
	CancelRequestedBy string     `json:"cancel_requested_by,omitempty"`
	CancelReason      string     `json:"cancel_reason,omitempty"`
	// LastError and LastOutcome describe the most recent attempt: the
	// error text, and the typed outcome (transient, deadline, permanent,
	// …) it was requeued or parked under.
	LastError   string `json:"last_error,omitempty"`
	LastOutcome string `json:"last_outcome,omitempty"`
	// UniqueKey is set only on a close obligation, which one entity holds
	// while one is ready, leased or parked. SupersededBy names the row that
	// replaced a cancelled one.
	UniqueKey    string `json:"unique_key,omitempty"`
	SupersededBy *int64 `json:"superseded_by,omitempty"`
	// FirstEnqueuedAt is the original enqueue, preserved across an operator
	// redrive; CreatedAt is the row's insert; DoneAt is when the row
	// settled (done, parked or cancelled).
	FirstEnqueuedAt time.Time  `json:"first_enqueued_at"`
	CreatedAt       time.Time  `json:"created_at"`
	DoneAt          *time.Time `json:"done_at,omitempty"`

	// Traceparent is the W3C trace context of whoever enqueued this row —
	// the envelope header that lets the routing of an event be tied back to
	// the poll cycle that emitted it. Empty is the normal state (tracing
	// off, an untraced producer) and reads back as NULL; the consumer LINKS
	// to it rather than descending from it, because one cycle emits many
	// events that route later and possibly elsewhere.
	Traceparent string `json:"traceparent,omitempty"`
	// EntityPollSeq is the entity's poll_seq at the moment this row's event
	// was judged — the value the tracker's snapshot CAS advanced TO when it
	// committed the batch — so a terminating close can refuse to land
	// against any other version of the entity. Nil for a row that arrived
	// through the ingest path rather than the CAS path: those carry no
	// version, and the close they imply is guarded on state alone.
	EntityPollSeq *int64 `json:"entity_poll_seq,omitempty"`
}

// Status values for QueuedEvent.Status: the work-item contract's vocabulary.
// A ready row is claimable once its retry time has arrived; a leased row has
// a holder; done, parked and cancelled are terminal, and only an operator
// redrive moves a parked row again.
const (
	QueuedEventStatusReady     = "ready"
	QueuedEventStatusLeased    = "leased"
	QueuedEventStatusDone      = "done"
	QueuedEventStatusParked    = "parked"
	QueuedEventStatusCancelled = "cancelled"
)

// ParkedEvent is one parked queue row as the operator surface renders it:
// enough to recognize what stopped and decide whether to redrive it, and
// nothing more.
//
// A parked row is not a retry still in flight. The event is durably
// recorded — its audit row committed in the same transaction as the queue
// row — but the routing obligations it carried (task mint/bump, owner
// resolution, trigger firing, close processing) spent the whole attempt
// budget, or failed permanently, and will never run on their own. Nothing
// re-drives it: the tracker's snapshot advanced when the event was minted,
// so the transition that produced it does not re-emit. An operator redrive
// is the only path back, which is why these rows are retained rather than
// pruned.
type ParkedEvent struct {
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
	// Attempt is the budget the row spent before parking, of MaxAttempts;
	// LastOutcome the typed reason its final attempt gave and LastError
	// that attempt's error text.
	Attempt     int    `json:"attempt"`
	MaxAttempts int    `json:"max_attempts"`
	LastOutcome string `json:"last_outcome,omitempty"`
	LastError   string `json:"last_error,omitempty"`
	// FirstEnqueuedAt is the original enqueue, preserved across a redrive,
	// and ParkedAt is when the row settled parked.
	FirstEnqueuedAt time.Time `json:"first_enqueued_at"`
	ParkedAt        time.Time `json:"parked_at"`
}
