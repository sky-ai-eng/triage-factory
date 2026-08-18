package domain

import "time"

// ConversationSignalKind discriminates the five cross-pod conversation-control operations
// (TFAC-585). See docs/for-agents/specs/horizontal-scaling/README.md §5.2.
type ConversationSignalKind string

const (
	// ConversationSignalCancel hastens a live kill; the DB-only park
	// write is already the source of truth and already works cross-pod, so
	// this kind is fire-and-forget — never waited on.
	ConversationSignalCancel ConversationSignalKind = "cancel"
	// ConversationSignalInterrupt stops the conversation's current turn, leaving the process
	// alive for further input.
	ConversationSignalInterrupt ConversationSignalKind = "interrupt"
	// ConversationSignalSteer delivers a free-form user message to a live conversation.
	// Payload: {"text": "..."}.
	ConversationSignalSteer ConversationSignalKind = "steer"
	// ConversationSignalPermission answers a pending tool-permission prompt. Payload:
	// {"request_id","behavior","message","updated_input"}.
	ConversationSignalPermission ConversationSignalKind = "permission"
	// ConversationSignalInject delivers TFAC-594's additive-event injection into a
	// live conversation executing on a remote executor. Payload:
	// {"producer","body","entity_id","task_id","trigger_id","triggering_event_id"}.
	ConversationSignalInject ConversationSignalKind = "inject"
)

// Ack-result vocabulary the owner writes when it applies (or fails to
// apply) a signal. Free text, no DB CHECK — kept as named constants so
// call sites don't hand-spell the strings.
const (
	// ConversationSignalAckOK is the generic success result for cancel/interrupt/
	// steer/permission: the local RunController op was applied (or, for
	// cancel, the DB-only fallback already covers correctness and this
	// just records that the signal was seen).
	ConversationSignalAckOK = "ok"
	// ConversationSignalAckDelivered means an inject signal was steered into the
	// still-live process.
	ConversationSignalAckDelivered = "delivered"
	// ConversationSignalAckGone means the owner found the conversation no longer live by the
	// time it looked — an interrupt/steer/permission signal simply lost the
	// race (the caller sees this as "no live process" and 409s); an inject
	// signal additionally enqueues the pending_firing itself so the intent
	// is never dropped.
	ConversationSignalAckGone = "gone"
	// ConversationSignalAckStale means the signal was a legal duplicate whose target
	// state was already reached: a second cancel/interrupt for an already-
	// dead proc, or a permission answer for an already-resolved request_id.
	ConversationSignalAckStale = "stale"
)

// ConversationSignal is one row of the conversation_signals cross-pod control outbox
// (Postgres only — see internal/db.ConversationSignalStore). A control pod inserts
// one when a control request (cancel/interrupt/steer/permission/inject)
// can't be served by its own local process registry; the executor named by
// Target owns the conversation's live process, applies the operation through its
// own local RunController, and acks with a result.
type ConversationSignal struct {
	ID             int64
	OrgID          string
	ConversationID string
	Kind           ConversationSignalKind
	Payload        string // raw JSON, "" when the kind carries no payload (cancel/interrupt)
	Target         string // the executor id (internal/instance) that owns the conversation
	CreatedAt      time.Time
	AckedAt        *time.Time
	AckResult      string
}
