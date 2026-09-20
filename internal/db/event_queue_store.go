package db

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EventQueueStore owns the event_queue table — the durable, DB-backed
// queue the router drains instead of riding the lossy in-memory bus.
// It is a transactional-outbox: Enqueue writes the events
// audit row and the queue row in one transaction, so a recorded event is
// always routable and a queued event always has its audit row.
//
// The queue's lifecycle is the shared work-item contract
// (internal/db/workitem), declared for this table by workkinds.EventQueue:
// Claim leases rows and hands back receipts, RenewLease / MarkDone / Requeue
// are the holder's fenced writes, and Redrive is the operator control over a
// parked row. The store adds nothing to that lifecycle; it composes the
// package's verbs with this kind's own columns and its admission rules.
//
// This is a system-service store: the ingestor (poller/tracker) and the
// drain worker run as background goroutines with no per-user identity, so
// the Postgres impl wires against the admin pool (BYPASSRLS) and keeps
// org_id bound in every statement as defense in depth. SQLite collapses
// onto its single connection and asserts the local sentinel org on the
// org-scoped methods.
//
// The queue's bookkeeping writes — the holder verbs, the prune, the
// redrive — are exempt from the returned-row rule: each is fire-and-forget
// from its caller's side, and the next claim reads the state, not this
// caller.

// MaxRedriveIDs bounds how many queue ids one redrive call may name. A
// selection is made from a page, and a page is bounded by the list contract,
// so this only rejects a hand-rolled request — and rejecting it keeps the
// per-id statement loop bounded.
const MaxRedriveIDs = 500

// TraceparentAt reads the i-th entry of a batch's parallel traceparent
// slice, returning "" (stored as NULL) when the slice doesn't cover i.
// Shared by both dialects' EnqueueBatchWithSnapshotCAS so a nil or short
// slice means the same thing in each: those events enqueue untraced, the
// way every untraced producer's row does.
func TraceparentAt(traceparents []string, i int) string {
	if i < len(traceparents) {
		return traceparents[i]
	}
	return ""
}

// EventQueueRowCols is the kind's own columns for one admission, shared by
// both dialects so the two produce identical rows. An untraced producer and
// an entity-less event store NULL rather than an empty string, so "no
// context to link" and "no entity" are each one value in the column, and a
// nil entityPollSeq is the ingest path's NULL.
func EventQueueRowCols(eventID string, evt domain.Event, traceparent string, entityPollSeq *int64) map[string]any {
	var entityID any
	if evt.EntityID != nil && *evt.EntityID != "" {
		entityID = *evt.EntityID
	}
	var tp any
	if traceparent != "" {
		tp = traceparent
	}
	var pollSeq any
	if entityPollSeq != nil {
		pollSeq = *entityPollSeq
	}
	return map[string]any{
		"event_id":        eventID,
		"entity_id":       entityID,
		"event_type":      evt.EventType,
		"traceparent":     tp,
		"entity_poll_seq": pollSeq,
	}
}

// ClaimedEvent is one leased queue row: the receipt that authorizes its
// terminal write, and the row as claimed.
type ClaimedEvent struct {
	Receipt workitem.Receipt
	Event   domain.QueuedEvent
}

// EventQueueClaim is one Claim call's work. Cancelled, Parked and Reclaimed
// are workitem.ClaimResult's counts, passed through for the worker's log.
type EventQueueClaim struct {
	Events    []ClaimedEvent
	Cancelled int
	Parked    int
	Reclaimed int
}

type EventQueueStore interface {
	// Enqueue atomically records the event (the durable audit row) AND
	// admits its queue row in a single transaction, returning the generated
	// event id. Empty evt.ID is generated as a v4. The caller (ingestor)
	// stamps the returned id onto the event before publishing it to the
	// ephemeral bus for cosmetic subscribers, so the WS feed and the
	// queue agree on the event id.
	//
	// Only entity-bearing github:/jira: events are enqueued — the
	// router's domain. System events stay bus-only and are never queued.
	// The row is admitted ready with the kind's budget and no unique key:
	// the snapshot diff is an ordinary event's dedup, and the queue must
	// never suppress a transition.
	//
	// traceparent is the producer's W3C trace context, stamped onto the
	// queue row so the drain worker can link an event's routing back to
	// the cycle that emitted it. It is a parameter rather than something
	// read off ctx here because a store must not depend on an ambient
	// span: the caller (the ingestor) owns the propagator, and a test can
	// hand over a literal. Empty — the normal case, with tracing off or an
	// untraced producer — is stored as NULL.
	Enqueue(ctx context.Context, orgID string, evt domain.Event, traceparent string) (eventID string, err error)

	// EnqueueBatchWithSnapshotCAS is the tracker's emit: the entity
	// snapshot advance and the durable enqueue of the transitions diffed
	// against it, in ONE transaction.
	//
	// It exists because those two writes are the same fact. The
	// snapshot-diff is the sole re-emit prevention, so the snapshot must
	// not advance ahead of the transitions it retires: a process that dies
	// between a won CAS and the enqueue loses them permanently (the next
	// cycle diffs new-against-new and produces nothing). One transaction
	// deletes that window — either both land or neither does.
	//
	// The CAS itself is UpdateSnapshotCASSystem's, semantics unchanged: the
	// UPDATE pins expectedPollSeq alongside org_id/id and bumps poll_seq by
	// 1 on success. A miss (a straggler ex-leader, stale by the time it
	// lands) matches zero rows, and then the whole transaction commits
	// NOTHING — ok=false, no events rows, no queue rows — so a losing
	// writer is invisible in the log rather than half-applied.
	//
	// events is the batch to enqueue on a won CAS: same writes Enqueue
	// makes, one events audit row + one queue row per event, and the
	// returned eventIDs are parallel to it (ids the caller stamps onto the
	// copies it forwards to the bus). traceparents is parallel too; a nil
	// or short slice stores NULL for the entries it doesn't cover, matching
	// Enqueue's empty-traceparent case. An EMPTY batch is a pure CAS, so
	// the tracker keeps one call path whether or not its diff produced
	// events.
	//
	// Every queue row in the batch is stamped with the entity's poll_seq
	// AFTER the CAS (expectedPollSeq + 1): the version the batch was judged
	// at, which a terminating close reads back to refuse any other. Rows
	// enqueued through Enqueue carry NULL there.
	//
	// A domain.EventSystemEntityCloseOwed event in the batch is admitted
	// under the entity's close-obligation key, and only if the entity has
	// no unsettled row in domain.EntityCloseSettlingEventTypes — checked on
	// this same transaction, so two cycles cannot both find the queue
	// empty. A skipped obligation writes neither an events row nor a queue
	// row and leaves "" in its eventIDs slot; the caller forwards nothing
	// for it. Should the admission nevertheless report a duplicate past
	// that check, the batch errors and rolls back whole: the entity row
	// lock the CAS took serializes every writer of that key, so the case
	// cannot arise, and an invariant that cannot fail must fail loudly
	// rather than leave an orphan events row.
	//
	// System-scoped like the CAS it subsumes: the tracker is a background
	// job with no JWT claims, and org_id is bound by argument.
	EnqueueBatchWithSnapshotCAS(ctx context.Context, orgID, entityID, snapshotJSON string, expectedPollSeq int64, events []domain.Event, traceparents []string) (ok bool, eventIDs []string, err error)

	// Claim leases up to n rows across every org (org "" to workitem.Claim)
	// and reads each leased row's own columns by id in one statement after
	// the claim. Events is in claim order. Rows the claim settled instead
	// (a cancellation request, a spent budget) are counted, not returned.
	//
	// Cross-org by design: the drain worker is a single system service
	// draining every tenant, interleaved by the kind's fairness policy, so
	// this is one of the explicitly org-wide system reads — the claimed row
	// carries its org_id, which scopes all downstream processing.
	//
	// A non-nil error still returns the events of rounds that committed
	// before it, as workitem.Claim does with their receipts: they are real
	// leases, and the caller disposes of them before it escalates.
	Claim(ctx context.Context, owner workitem.Owner, n int) (EventQueueClaim, error)

	// RenewLease pushes the lease out to fresh database time plus the kind's
	// lease, and is the point where a pending cancellation request is
	// observed and settled. It is the worker's fence check before routing:
	// workitem.ErrLeaseLost means the row's next holder owns it, and
	// workitem.ErrCancelled means the row was settled by this call.
	RenewLease(ctx context.Context, r workitem.Receipt) (workitem.Receipt, error)

	// MarkDone is the terminal flip after the routing side effects
	// committed in their own transactions, each behind its own replay
	// fence. A stale receipt matches nothing (workitem.ErrLeaseLost) and
	// writes nothing.
	MarkDone(ctx context.Context, r workitem.Receipt) error

	// Requeue records a failed attempt under a typed outcome: the row
	// returns to ready with a backoff retry time, or parks when the outcome
	// is permanent or the budget is spent. parked reports which. A stale
	// receipt matches nothing and writes nothing.
	Requeue(ctx context.Context, r workitem.Receipt, outcome workitem.Outcome, cause error) (parked bool, err error)

	// PruneSettled deletes done and cancelled rows with done_at before the
	// cutoff, across orgs. Parked rows are never pruned: a parked row is the
	// only record of routing work that will not run on its own.
	PruneSettled(ctx context.Context, before time.Time) (int, error)

	// ListParked returns one page of the org's parked rows — the operator
	// surface over routing work the queue stopped on — newest first, plus
	// the unpaged total of parked rows. id DESC is a total order (id is
	// monotonic per insert), so the pages partition the result set.
	//
	// The entity fields are outer-joined for display and read empty when the
	// row carries no entity or the entity row is gone. That is deliberate: a
	// queue row can outlive its entity by a cascade, and omitting it would
	// hide a parked event precisely because something unusual happened to it.
	//
	// Org-scoped by argument on the admin pool, like every other method here
	// — the store is system-service wired, so the org-admin predicate in the
	// handler is the authorization, not RLS.
	ListParked(ctx context.Context, orgID string, opts ListOpts) ([]domain.ParkedEvent, int, error)

	// GetParked returns one parked row by queue id, or (nil, nil) when the
	// org has no parked row with that id. Same projection as ListParked — a
	// single read answers with the list's row shape, not a bespoke one.
	GetParked(ctx context.Context, orgID string, id int64) (*domain.ParkedEvent, error)

	// Redrive returns the named parked rows to ready with a fresh budget
	// through workitem.Redrive, one call per id, and reports how many moved.
	// An id that is not parked, belongs to another org, or does not exist is
	// counted out (workitem.ErrNotParked), never an error — a stale
	// selection is the normal case for a table an operator reads and then
	// acts on. On error the count is what moved before the failure.
	//
	// Replay safety is the same argument every other retry in this queue
	// rests on: a redriven row re-enters the identical at-least-once path,
	// where the tasks dedup index, the (triggering_event_id, trigger_id)
	// replay fence and the one-active-run index collapse anything that
	// already landed. That is also why this kind has no Supersede control:
	// a supersede records a replacement row, and a parked event has none —
	// one whose work has since been done another way is redriven and
	// converges to a no-op through its fences.
	Redrive(ctx context.Context, orgID string, ids []int64, by string) (int, error)

	// ListForEntity returns every queue row for an entity in id order
	// regardless of status.
	//
	// No production caller today: production reads the queue by status (the
	// claim) and by key (the close obligation's dedup). Kept as the
	// entity-shaped observation read — "everything queued against this
	// entity, in any status" — that the router, tracker, ingest and delegate
	// tests assert through, and that a debug view of a pull request's queue
	// would ask.
	ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.QueuedEvent, error)

	// UnsettledCloseExistsSystem reports whether the entity has a ready,
	// leased or parked row in domain.EntityCloseSettlingEventTypes — a
	// terminating transition or a close obligation the router has not
	// settled, where a parked one still holds the entity's key. The tracker
	// asks before it appends an obligation to a refresh's batch, so a cycle
	// that would only re-owe an already-owed close records nothing and, with
	// nothing else to record, leaves the entity's version where the close
	// in flight was judged. EnqueueBatchWithSnapshotCAS re-checks on its own
	// transaction; this read is advisory and never the dedup.
	UnsettledCloseExistsSystem(ctx context.Context, orgID, entityID string) (bool, error)
}
