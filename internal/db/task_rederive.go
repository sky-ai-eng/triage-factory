package db

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
)

// ErrScoreMoved is Complete's answer when the locked row's requested_revision
// is above the receipt's frozen value: a newer score landed after the claim,
// nothing was written, and the row is still leased.
var ErrScoreMoved = errors.New("db: a newer score landed since this re-evaluation was claimed")

// TaskReDeriveStore owns the task_rederive_queue table — the post-scoring
// re-evaluation of a task's deferred triggers as a work kind, on the shared
// work-item contract (internal/db/workitem) as workkinds.TaskReDerive
// declares it.
//
// Admission is the score store's: UpdateTaskScores admits the row, keyed on
// the task id, and raises its requested_revision in the transaction that
// writes the scores, so the obligation and the scores it is owed for commit
// together or not at all. This store is the worker's side: Claim leases rows
// and freezes each row's requested_revision into its receipt; Complete runs
// the evaluation's effects — pending_firings admissions — against a
// PendingFiringsStore bound to the completion's own transaction, under the
// row lock, and only while the row still carries the frozen revision;
// DeferScoreMoved is the answer when it does not.
//
// Lock order: every transaction that touches both this table and tasks takes
// task_rederive_queue first. The score write does (admission, then the
// tasks statement); the completion locks its queue row first and reaches
// tasks only through PendingFiringsStore.Enqueue, which inserts into
// pending_firings and then stamps the task. Across the three kinds the order
// is task_rederive_queue → pending_firings → tasks, and nothing takes them in
// another order.
//
// UpdateTaskScores is the only writer of requested_revision; the package's
// claim, holder verbs and operator controls own the block's columns; task_id
// and unique_key are immutable after insert.
//
// All methods bind org_id through the receipt. The Postgres impl runs against
// the admin pool (the worker is a system service with no per-user identity);
// the SQLite impl asserts the local sentinel on every receipt it is handed.
// The holder verbs are exempt from the returned-row rule: each is
// fire-and-forget from its caller's side, and the next claim reads the
// state, not this caller. The store is never transaction-bound.
type TaskReDeriveStore interface {
	// Claim leases up to n claimable rows across every org (org "" to
	// workitem.Claim) and reads each leased row's task by id in one
	// statement after the claim. Items is in claim order. Rows the claim
	// settled instead (a cancellation request, a spent budget) are counted,
	// not returned. A non-nil error still returns the items of rounds that
	// committed before it.
	Claim(ctx context.Context, owner workitem.Owner, n int) (ReDeriveClaim, error)

	// RenewLease pushes the lease out to fresh database time plus the kind's
	// lease, and is the point where a pending cancellation request is
	// observed and settled: workitem.ErrLeaseLost means the row's next
	// holder owns it, workitem.ErrCancelled means this call settled it.
	RenewLease(ctx context.Context, r workitem.Receipt) (workitem.Receipt, error)

	// Complete is the SingleTx completion. Inside workitem.Complete's
	// closure, with the row locked, it reads the row's requested_revision;
	// above the receipt's frozen value it returns ErrScoreMoved and the
	// transaction rolls back. Otherwise it runs effects against a
	// PendingFiringsStore bound to the same transaction and flips the row
	// done with them. A closure error rolls everything back and is returned
	// wrapped, so errors.Is sees it. A stale receipt is workitem.ErrLeaseLost
	// and writes nothing; workitem.ErrCancelled means the request was
	// settled instead and effects never ran.
	Complete(ctx context.Context, r workitem.Receipt, effects func(firings PendingFiringsStore) error) error

	// DeferScoreMoved returns the row to ready with its attempt refunded,
	// through workitem.Defer under the predicate "requested_revision is
	// above the frozen value" and a retry time of now; the next claim
	// freezes the newer revision. workitem.ErrDeferRefused when the
	// revision has not moved.
	DeferScoreMoved(ctx context.Context, r workitem.Receipt) error

	// Requeue records a failed attempt under a typed outcome: the row returns
	// to ready with a backoff retry time, or parks when the outcome is
	// permanent or the budget is spent. parked reports which.
	Requeue(ctx context.Context, r workitem.Receipt, outcome workitem.Outcome, cause error) (parked bool, err error)
}

// ClaimedReDerive is one leased row: the receipt, the task, and the revision
// frozen into the receipt at claim.
type ClaimedReDerive struct {
	Receipt           workitem.Receipt
	TaskID            string
	RequestedRevision int64
}

// ReDeriveClaim is one Claim call's work. Cancelled, Parked and Reclaimed are
// workitem.ClaimResult's counts, passed through for the worker's log.
type ReDeriveClaim struct {
	Items     []ClaimedReDerive
	Cancelled int
	Parked    int
	Reclaimed int
}

// FrozenRequestedRevision reads the revision a claim froze into the receipt.
// The package hands frozen columns back as the driver scanned them, and both
// drivers scan an integer column into int64; anything else is a shape the
// store cannot compare against and is refused rather than converted.
func FrozenRequestedRevision(r workitem.Receipt) (int64, error) {
	v, ok := r.Frozen["requested_revision"]
	if !ok {
		return 0, errors.New("db: receipt froze no requested_revision")
	}
	rev, ok := v.(int64)
	if !ok {
		return 0, fmt.Errorf("db: receipt froze requested_revision as %T, want int64", v)
	}
	return rev, nil
}

// TaskReDeriveRowCols is the kind's own columns for one admission, shared by
// both dialects so the two produce identical rows. requested_revision starts
// at zero: the score write raises it in the statement that follows the
// admission, whether the admission inserted or deduplicated.
func TaskReDeriveRowCols(taskID string) map[string]any {
	return map[string]any{
		"task_id":            taskID,
		"requested_revision": int64(0),
	}
}

// TaskReDeriveSubject is the WorkSubject both dialects describe a
// re-evaluation with, so the two produce identical subjects. The label is
// what an operator recognizes: the entity's source id ("owner/repo#18",
// "SKY-123"), or the bare task id when the entity row is gone.
func TaskReDeriveSubject(taskID string, requestedRevision int64, eventType, sourceID, title string) WorkSubject {
	label := sourceID
	if label == "" {
		label = taskID
	}
	fields := map[string]string{
		"task_id":            taskID,
		"requested_revision": strconv.FormatInt(requestedRevision, 10),
	}
	if eventType != "" {
		fields["event_type"] = eventType
	}
	return WorkSubject{Label: label, Detail: title, Fields: fields}
}
