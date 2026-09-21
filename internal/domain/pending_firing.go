package domain

import "time"

// PendingFiring is an intent to auto-delegate that could not run when its
// triggering event arrived because its task was busy — a live conversation,
// or older firings queued ahead of it. The router admits it, and the firing
// worker claims it under the shared work-item contract (internal/db/workitem)
// once the task holds no live conversation and no run still marked running,
// validates it against the world now, and fires it or skips it with a reason.
//
// The row is the work-item block plus this kind's own columns: the four
// identity columns are fixed at admission, and exactly one of SkipReason and
// FiredBlueprintRunID is set on a done row, by the terminal write.
type PendingFiring struct {
	ID    int64  `json:"id"`
	OrgID string `json:"org_id"`

	EntityID          string `json:"entity_id"`
	TaskID            string `json:"task_id"`
	TriggerID         string `json:"trigger_id"`
	TriggeringEventID string `json:"triggering_event_id"`
	// SkipReason is set on a done row that did not fire: one of the
	// PendingFiringSkip* constants.
	SkipReason string `json:"skip_reason,omitempty"`
	// FiredBlueprintRunID is the blueprint run a done row fired into.
	FiredBlueprintRunID *string `json:"fired_run_id,omitempty"`

	Status        string     `json:"status"` // workitem.StatusReady | StatusLeased | StatusDone | StatusParked | StatusCancelled
	Attempt       int        `json:"attempt"`
	MaxAttempts   int        `json:"max_attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`

	LeaseGeneration int64      `json:"lease_generation"`
	LeaseOwner      string     `json:"lease_owner,omitempty"`
	LeaseEpoch      *int64     `json:"lease_epoch,omitempty"`
	LeasedAt        *time.Time `json:"leased_at,omitempty"`
	LeaseExpiresAt  *time.Time `json:"lease_expires_at,omitempty"`

	CancelRequestedAt *time.Time `json:"cancel_requested_at,omitempty"`
	CancelRequestedBy string     `json:"cancel_requested_by,omitempty"`
	CancelReason      string     `json:"cancel_reason,omitempty"`

	LastError    string `json:"last_error,omitempty"`
	LastOutcome  string `json:"last_outcome,omitempty"`
	UniqueKey    string `json:"unique_key,omitempty"`
	SupersededBy *int64 `json:"superseded_by,omitempty"`

	FirstEnqueuedAt time.Time  `json:"first_enqueued_at"`
	CreatedAt       time.Time  `json:"created_at"`
	DoneAt          *time.Time `json:"done_at,omitempty"`
}

// PendingFiring statuses: the work-item block's vocabulary, restated here so
// a consumer of this type need not import the package that owns it.
const (
	PendingFiringStatusReady     = "ready"
	PendingFiringStatusLeased    = "leased"
	PendingFiringStatusDone      = "done"
	PendingFiringStatusParked    = "parked"
	PendingFiringStatusCancelled = "cancelled"
)

// PendingFiring skip reasons, set on a done row that did not fire. Each is a
// definitive answer the worker's validation reached: retrying could not come
// out differently, so the row is settled rather than requeued. A failure that
// could come out differently — a store read, a spawner refusal — is a typed
// requeue outcome on the block instead, never a skip reason.
const (
	// PendingFiringSkipTaskClosed: the task is done, dismissed or snoozed. A
	// snooze is on the same axis as a close — the person said "not now" — and
	// a snooze wake mints a new event and a new firing if the trigger still
	// matches, so the queued one is the wrong path to wake it.
	PendingFiringSkipTaskClosed      = "task_closed"
	PendingFiringSkipTriggerDisabled = "trigger_disabled"
	PendingFiringSkipBreakerTripped  = "breaker_tripped"
	// PendingFiringSkipClaimChanged: the task is no longer bot-claimed at
	// firing time — a user took it over or requeued it — so the commitment
	// the queued row carried is no longer current, and firing would put a
	// phantom bot run on a task a person holds.
	PendingFiringSkipClaimChanged = "claim_changed"
	// PendingFiringSkipAlreadyFired: the fenced run insert found a run for
	// this firing's (triggering event, trigger) already committed — a
	// previous holder fired it and lost its lease before the terminal write,
	// or the immediate path fired it before this row was claimed. The
	// existing run is the materialization; firing again would duplicate it.
	PendingFiringSkipAlreadyFired = "already_fired"
)
