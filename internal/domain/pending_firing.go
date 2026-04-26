package domain

import "time"

// PendingFiring is an "intent to delegate" that couldn't run when its
// triggering event arrived because the entity already had an active auto
// run. Firings are enqueued by the router's gate and drained in arrival
// order by the spawner's QueueDrainer hook on auto-run terminal events.
//
// Lifecycle: pending → (fired | skipped_stale). Soft-deleted via status
// transition rather than DELETE so the queue's history is auditable
// alongside the events log.
type PendingFiring struct {
	ID                int64      `json:"id"`
	EntityID          string     `json:"entity_id"`
	TaskID            string     `json:"task_id"`
	TriggerID         string     `json:"trigger_id"`
	TriggeringEventID string     `json:"triggering_event_id"`
	Status            string     `json:"status"`                // pending | fired | skipped_stale
	SkipReason        string     `json:"skip_reason,omitempty"` // task_closed | trigger_disabled | breaker_tripped
	QueuedAt          time.Time  `json:"queued_at"`
	DrainedAt         *time.Time `json:"drained_at,omitempty"`
	FiredRunID        *string    `json:"fired_run_id,omitempty"`
}

// PendingFiring statuses.
const (
	PendingFiringStatusPending      = "pending"
	PendingFiringStatusFired        = "fired"
	PendingFiringStatusSkippedStale = "skipped_stale"
)

// PendingFiring skip reasons (set when status='skipped_stale'). Reserved
// for definitive "no longer relevant" outcomes — task closed, trigger
// disabled, breaker tripped. Transient errors during validation or fire
// (DB read failures, spawner.Delegate errors) leave the firing in
// 'pending' state for retry by a future drain or the periodic sweeper;
// they do not become a skip reason.
const (
	PendingFiringSkipTaskClosed      = "task_closed"
	PendingFiringSkipTriggerDisabled = "trigger_disabled"
	PendingFiringSkipBreakerTripped  = "breaker_tripped"
)
