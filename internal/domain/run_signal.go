package domain

import "time"

// RunSignalKind discriminates the five cross-pod run-control operations
// (TFAC-585). See docs/for-agents/specs/horizontal-scaling/README.md §5.2.
type RunSignalKind string

const (
	// RunSignalCancel hastens a live kill; the DB-only MarkCancelledIfActive
	// write is already the source of truth and already works cross-pod, so
	// this kind is fire-and-forget — never waited on.
	RunSignalCancel RunSignalKind = "cancel"
	// RunSignalInterrupt stops the run's current turn, leaving the process
	// alive for further input.
	RunSignalInterrupt RunSignalKind = "interrupt"
	// RunSignalSteer delivers a free-form user message to a live run.
	// Payload: {"text": "..."}.
	RunSignalSteer RunSignalKind = "steer"
	// RunSignalPermission answers a pending tool-permission prompt. Payload:
	// {"request_id","behavior","message","updated_input"}.
	RunSignalPermission RunSignalKind = "permission"
	// RunSignalInject delivers TFAC-594's additive-event injection into a
	// live run executing on a remote executor. Payload:
	// {"producer","body","entity_id","task_id","trigger_id","triggering_event_id"}.
	RunSignalInject RunSignalKind = "inject"
)

// Ack-result vocabulary the owner writes when it applies (or fails to
// apply) a signal. Free text, no DB CHECK — kept as named constants so
// call sites don't hand-spell the strings.
const (
	// RunSignalAckOK is the generic success result for cancel/interrupt/
	// steer/permission: the local RunController op was applied (or, for
	// cancel, the DB-only fallback already covers correctness and this
	// just records that the signal was seen).
	RunSignalAckOK = "ok"
	// RunSignalAckDelivered means an inject signal was steered into the
	// still-live process.
	RunSignalAckDelivered = "delivered"
	// RunSignalAckGone means the owner found the run no longer live by the
	// time it looked — an interrupt/steer/permission signal simply lost the
	// race (the caller sees this as "no live process" and 409s); an inject
	// signal additionally enqueues the pending_firing itself so the intent
	// is never dropped.
	RunSignalAckGone = "gone"
	// RunSignalAckStale means the signal was a legal duplicate whose target
	// state was already reached: a second cancel/interrupt for an already-
	// dead proc, or a permission answer for an already-resolved request_id.
	RunSignalAckStale = "stale"
)

// RunSignal is one row of the conversation_signals cross-pod control outbox
// (Postgres only — see internal/db.RunSignalStore). A control pod inserts
// one when a control request (cancel/interrupt/steer/permission/inject)
// can't be served by its own local process registry; the executor named by
// Target owns the run's live process, applies the operation through its
// own local RunController, and acks with a result.
type RunSignal struct {
	ID        int64
	OrgID     string
	RunID     string
	Kind      RunSignalKind
	Payload   string // raw JSON, "" when the kind carries no payload (cancel/interrupt)
	Target    string // the executor id (internal/instance) that owns the run
	CreatedAt time.Time
	AckedAt   *time.Time
	AckResult string
}
