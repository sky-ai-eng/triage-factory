package db

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ErrNotOnTransaction is returned by a verb that opens its own transaction
// through the work-item package when it is called on a store bound to a
// caller's transaction: it cannot nest there, and running it on the pool
// behind the caller's back would hide a write from the transaction the
// caller thinks it is in.
var ErrNotOnTransaction = errors.New("db: this verb opens its own transaction and is not available on a transaction-bound store")

// PendingFiringsStore owns the pending_firings table — the per-task queue of
// auto-delegation intents the router admits when a matched trigger cannot
// fire because its task is busy, on the shared work-item contract
// (internal/db/workitem) as workkinds.PendingFirings declares it.
//
// The lifecycle is the contract's: Enqueue admits a ready row, Claim leases
// rows and hands back receipts, and RenewLease / MarkFired / MarkSkipped /
// Requeue / DeferWhileTaskBusy are the holder's fenced writes. The per-task
// gate lives in the claim query itself, as the kind's claim filter: a firing
// is claimable only while its task holds no live top-level conversation, so
// a row behind a busy task is deferred rather than ready and becomes ripe
// when the task frees with no write of anyone's. The operator surface —
// listing, redrive, cancel — reaches the table through the WorkKindHandle
// the store also implements, with the package's own reads and controls.
//
// One firing per (task, trigger) while one is unsettled: admission is keyed
// under workkinds.PendingFiringKey, and a parked row holds its key. A later
// event for the same pair collapses onto the parked row rather than minting
// a new one, exactly as the close obligation behaves in the event queue; the
// parked row is on the operator panel, where it is redriven (it fires once,
// from its own triggering event, after the worker's validations) or
// cancelled.
//
// All methods take orgID; local mode passes runmode.LocalDefaultOrgID. The
// Postgres impl runs against the admin pool (the router and the firing
// worker are system services with no per-user identity) and binds org_id in
// every statement beside the RLS policy. The SQLite impl asserts the local
// sentinel on the org-scoped methods; the verbs reached through the
// WorkKindHandle bind org_id without asserting it, so the handler enforces
// the sentinel there.
//
// The bookkeeping writes — the holder verbs — are exempt from the
// returned-row rule: each is fire-and-forget from its caller's side, and the
// next claim reads the state, not this caller.
//
// A store bound to a caller's transaction answers Enqueue,
// HasUnsettledForTask and ListForEntity on that transaction, and refuses
// the verbs that open their own with ErrNotOnTransaction.
type PendingFiringsStore interface {
	// Enqueue admits a ready firing for (task, trigger) under
	// workkinds.PendingFiringKey and stamps the task's agent claim in the
	// same transaction (see AgentClaimStamp). inserted is false when the key
	// already has an unsettled row — ready, leased or parked — and the claim
	// stamp is then skipped, since that row's own admission made the
	// commitment. A stamp refusal is not an error and leaves the firing
	// committed. Runs on the store's own queryer: a store bound to a
	// transaction admits inside it. The transaction touches pending_firings
	// before tasks; every transaction that writes both keeps that order.
	Enqueue(ctx context.Context, orgID, entityID, taskID, triggerID, triggeringEventID string, claim AgentClaimStamp) (inserted, claimed bool, err error)

	// Claim leases up to n claimable rows across every org (org "" to
	// workitem.Claim) and reads each leased row's own columns by id in one
	// statement after the claim. Firings is in claim order. A ready row whose
	// task holds a live conversation is not claimable (the kind's
	// ClaimFilter). Rows the claim settled instead (a cancellation request,
	// a spent budget) are counted, not returned. A non-nil error still
	// returns the firings of rounds that committed before it.
	Claim(ctx context.Context, owner workitem.Owner, n int) (FiringClaim, error)

	// RenewLease pushes the lease out to fresh database time plus the kind's
	// lease, and is the point where a pending cancellation request is
	// observed and settled: workitem.ErrLeaseLost means the row's next
	// holder owns it, workitem.ErrCancelled means this call settled it.
	RenewLease(ctx context.Context, r workitem.Receipt) (workitem.Receipt, error)

	// MarkFired records the blueprint run the firing produced and flips the
	// row done, in one transaction. A stale receipt matches nothing
	// (workitem.ErrLeaseLost) and writes neither column.
	MarkFired(ctx context.Context, r workitem.Receipt, blueprintRunID string) error

	// MarkSkipped records why the firing did not fire and flips the row
	// done, in one transaction. reason is one of the
	// domain.PendingFiringSkip* constants. A stale receipt writes nothing.
	MarkSkipped(ctx context.Context, r workitem.Receipt, reason string) error

	// Requeue records a failed attempt under a typed outcome: the row returns
	// to ready with a backoff retry time, or parks when the outcome is
	// permanent or the budget is spent. parked reports which.
	Requeue(ctx context.Context, r workitem.Receipt, outcome workitem.Outcome, cause error) (parked bool, err error)

	// DeferWhileTaskBusy returns the row to ready with its attempt refunded,
	// through workitem.Defer under the predicate "the task holds a live
	// conversation" and a retry time of now: the row is ripe at once, and
	// the claim filter is what holds it until the task is free.
	// workitem.ErrDeferRefused when the predicate finds no live conversation.
	DeferWhileTaskBusy(ctx context.Context, r workitem.Receipt) error

	// HasUnsettledForTask reports whether the task has a ready, leased or
	// parked firing. The router's gate composes it with the live-conversation
	// read: a new firing queues behind older queued rows or a live
	// conversation. A parked row keeps the gate closed because it still holds
	// its key: a later event for the same trigger collapses onto it, and one
	// for another trigger queues behind it in order.
	HasUnsettledForTask(ctx context.Context, orgID, taskID string) (bool, error)

	// ListForEntity returns every row for an entity, oldest first, in any
	// status. Kept as the one entity-shaped read the router and delegate
	// tests assert through; no production caller.
	ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.PendingFiring, error)
}

// ClaimedFiring is one leased row: the receipt that authorizes its terminal
// write, and the row as claimed.
type ClaimedFiring struct {
	Receipt workitem.Receipt
	Firing  domain.PendingFiring
}

// FiringClaim is one Claim call's work. Cancelled, Parked and Reclaimed are
// workitem.ClaimResult's counts, passed through for the worker's log.
type FiringClaim struct {
	Firings   []ClaimedFiring
	Cancelled int
	Parked    int
	Reclaimed int
}

// PendingFiringRowCols is the kind's own columns for one admission, shared by
// both dialects so the two produce identical rows. skip_reason and
// fired_run_id are absent: only the terminal write sets them.
func PendingFiringRowCols(entityID, taskID, triggerID, triggeringEventID string) map[string]any {
	return map[string]any{
		"entity_id":           entityID,
		"task_id":             taskID,
		"trigger_id":          triggerID,
		"triggering_event_id": triggeringEventID,
	}
}

// PendingFiringSubject is the WorkSubject both dialects describe a firing
// with, so the two produce identical subjects. The label is what an operator
// recognizes: the entity's source id ("owner/repo#18", "SKY-123"), or the
// bare entity id when the entity row is gone. Fields carry the firing's
// identity, the handler's name when the join found it, and whichever of
// fired_run_id or skip_reason the terminal write set.
func PendingFiringSubject(f domain.PendingFiring, sourceID, title string, triggerName string, triggerFound bool) WorkSubject {
	label := sourceID
	if label == "" {
		label = f.EntityID
	}
	fields := map[string]string{
		"task_id":             f.TaskID,
		"trigger_id":          f.TriggerID,
		"triggering_event_id": f.TriggeringEventID,
	}
	if triggerFound {
		fields["trigger"] = triggerName
	}
	if f.FiredBlueprintRunID != nil && *f.FiredBlueprintRunID != "" {
		fields["fired_run_id"] = *f.FiredBlueprintRunID
	}
	if f.SkipReason != "" {
		fields["skip_reason"] = f.SkipReason
	}
	return WorkSubject{Label: label, Detail: title, Fields: fields}
}
