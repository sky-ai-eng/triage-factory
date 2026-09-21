package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventQueueStore is the Postgres impl of db.EventQueueStore — the durable
// router queue, on the shared work-item contract. Wired against the admin
// pool in postgres.New: the ingestor and drain worker are system services
// with no per-user identity, so impersonating a user via the app pool would
// be wrong. The event_queue_all RLS policy is defense-in-depth (admin
// bypasses it) and org_id is bound in every statement.
//
// Holds the admin *sql.DB directly (not the shared queryer) because Enqueue
// runs the events-row insert and the queue-row admission in one transaction
// — the outbox atomicity guarantee — which needs BeginTx, and because the
// package's claim opens its own transaction per round.
type eventQueueStore struct {
	conn *sql.DB
	kind workitem.Kind
}

func newEventQueueStore(conn *sql.DB) db.EventQueueStore {
	return &eventQueueStore{conn: conn, kind: workkinds.EventQueue(workitem.Postgres)}
}

var _ db.EventQueueStore = (*eventQueueStore)(nil)

func (s *eventQueueStore) Enqueue(ctx context.Context, orgID string, evt domain.Event, traceparent string) (string, error) {
	var id string
	if err := db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
		// recordEvent (postgres/events.go) is the canonical events insert —
		// reused so the queue's audit row matches every other event row
		// (jsonb metadata cast, nullable occurred_at, org_id bind).
		var err error
		id, err = recordEvent(ctx, tx, orgID, evt)
		if err != nil {
			return err
		}
		_, err = s.admitQueueRow(ctx, tx, orgID, id, evt, traceparent, nil, "")
		return err
	}); err != nil {
		return "", err
	}
	return id, nil
}

// admitQueueRow admits the ready queue row for an already-recorded event
// through the package, so the shared block is filled the one way the
// contract fills it. Shared by Enqueue and EnqueueBatchWithSnapshotCAS so
// both produce byte-identical rows.
//
// entityPollSeq is the version the event was judged at — the CAS path passes
// the poll_seq it just advanced to, the ingest path passes nil and the column
// stores NULL. uniqueKey is set only for the close obligation. deduplicated
// reports the package's answer, which only a keyed admission can return.
func (s *eventQueueStore) admitQueueRow(ctx context.Context, tx *sql.Tx, orgID, eventID string, evt domain.Event, traceparent string, entityPollSeq *int64, uniqueKey string) (bool, error) {
	_, deduplicated, err := workitem.Admit(ctx, tx, s.kind, orgID, uniqueKey, db.EventQueueRowCols(eventID, evt, traceparent, entityPollSeq))
	return deduplicated, err
}

// unsettledCloseStatuses is the predicate for a close still deciding an
// entity's fate. Parked is in it: a parked obligation holds the entity's key
// under the kind's uniqueness index, so the tracker mints no replacement
// and the checker does not count the entity — the parked row itself is the
// alarm.
const unsettledCloseStatuses = "'ready','leased','parked'"

// unsettledCloseExists reports whether the entity already has an unsettled
// row whose settlement decides its fate — a terminating transition or an
// earlier close obligation. Evaluated inside the enqueue transaction, so the
// answer is the one the insert commits against.
func unsettledCloseExists(ctx context.Context, q queryer, orgID, entityID string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM public.event_queue
		WHERE org_id = $1 AND entity_id = $2
		  AND status IN (`+unsettledCloseStatuses+`)
		  AND event_type = ANY($3)
		LIMIT 1
	`, orgID, entityID, domain.EntityCloseSettlingEventTypes()).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// EnqueueBatchWithSnapshotCAS runs the snapshot CAS and the batch's
// events + event_queue writes in one transaction — see the interface doc
// for why they belong together. A CAS that matches zero rows returns
// ok=false having written nothing: the loser skips the batch inside the
// same transaction, so it is invisible rather than half-applied.
func (s *eventQueueStore) EnqueueBatchWithSnapshotCAS(ctx context.Context, orgID, entityID, snapshotJSON string, expectedPollSeq int64, events []domain.Event, traceparents []string) (bool, []string, error) {
	var won bool
	var ids []string
	if err := db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
		var err error
		won, err = updateSnapshotCAS(ctx, tx, orgID, entityID, snapshotJSON, expectedPollSeq)
		if err != nil {
			return err
		}
		if !won {
			return nil
		}
		// The version every row in this batch was judged at: the poll_seq
		// the CAS above just advanced to.
		judgedAt := expectedPollSeq + 1
		ids = make([]string, 0, len(events))
		for i, evt := range events {
			uniqueKey := ""
			if evt.EventType == domain.EventSystemEntityCloseOwed {
				// One obligation per entity while one is unsettled — checked
				// here, on the transaction that would insert it, so two cycles
				// cannot both find the queue empty. A skipped obligation leaves
				// an empty id in its slot.
				owed, err := unsettledCloseExists(ctx, tx, orgID, entityID)
				if err != nil {
					return err
				}
				if owed {
					ids = append(ids, "")
					continue
				}
				uniqueKey = workkinds.EventQueueCloseOwedKey(entityID)
			}
			id, err := recordEvent(ctx, tx, orgID, evt)
			if err != nil {
				return err
			}
			deduplicated, err := s.admitQueueRow(ctx, tx, orgID, id, evt, db.TraceparentAt(traceparents, i), &judgedAt, uniqueKey)
			if err != nil {
				return err
			}
			if deduplicated {
				// The check above ran on this transaction, and the entity row
				// lock the CAS took serializes every writer of this key, so
				// a duplicate here is an invariant failing. Erroring rolls
				// the events row back rather than orphaning it.
				return errors.New("close obligation admission raced its own check")
			}
			ids = append(ids, id)
		}
		return nil
	}); err != nil {
		return false, nil, err
	}
	if !won {
		return false, nil, nil
	}
	return true, ids, nil
}

// pgQueuedEventSelect is the projection every QueuedEvent read answers
// with: the shared block and the kind's own columns.
const pgQueuedEventSelect = `
	SELECT id, org_id, event_id, COALESCE(entity_id::text, ''), event_type,
	       status, attempt, max_attempts, next_attempt_at,
	       lease_generation, COALESCE(lease_owner, ''), lease_epoch, leased_at, lease_expires_at,
	       cancel_requested_at, COALESCE(cancel_requested_by, ''), COALESCE(cancel_reason, ''),
	       COALESCE(last_error, ''), COALESCE(last_outcome, ''), COALESCE(unique_key, ''), superseded_by,
	       first_enqueued_at, created_at, done_at,
	       COALESCE(traceparent, ''), entity_poll_seq
	FROM public.event_queue`

func (s *eventQueueStore) Claim(ctx context.Context, owner workitem.Owner, n int) (db.EventQueueClaim, error) {
	res, claimErr := workitem.Claim(ctx, s.conn, s.kind, owner, "", n)
	out := db.EventQueueClaim{Cancelled: res.Cancelled, Parked: res.Parked, Reclaimed: res.Reclaimed}
	if len(res.Claimed) == 0 {
		return out, claimErr
	}
	// The kind's columns are immutable after admission, so reading them by
	// id after the claim reads the values the row was admitted with.
	ids := make([]int64, len(res.Claimed))
	for i, r := range res.Claimed {
		ids[i] = r.ItemID
	}
	rows, err := s.conn.QueryContext(ctx, pgQueuedEventSelect+`
		WHERE id = ANY($1)`, ids)
	if err != nil {
		return out, errors.Join(claimErr, err)
	}
	defer rows.Close()
	byID := make(map[int64]domain.QueuedEvent, len(res.Claimed))
	for rows.Next() {
		qe, err := scanPgQueuedEvent(rows)
		if err != nil {
			return out, errors.Join(claimErr, err)
		}
		byID[qe.ID] = qe
	}
	if err := rows.Err(); err != nil {
		return out, errors.Join(claimErr, err)
	}
	out.Events = make([]db.ClaimedEvent, 0, len(res.Claimed))
	for _, r := range res.Claimed {
		qe, ok := byID[r.ItemID]
		if !ok {
			return out, errors.Join(claimErr, fmt.Errorf("event_queue row %d leased but not readable", r.ItemID))
		}
		out.Events = append(out.Events, db.ClaimedEvent{Receipt: r, Event: qe})
	}
	return out, claimErr
}

func (s *eventQueueStore) RenewLease(ctx context.Context, r workitem.Receipt) (workitem.Receipt, error) {
	return workitem.RenewLease(ctx, s.conn, s.kind, r)
}

func (s *eventQueueStore) MarkDone(ctx context.Context, r workitem.Receipt) error {
	return workitem.MarkDone(ctx, s.conn, s.kind, r)
}

func (s *eventQueueStore) Requeue(ctx context.Context, r workitem.Receipt, outcome workitem.Outcome, cause error) (bool, error) {
	return workitem.Requeue(ctx, s.conn, s.kind, r, outcome, cause)
}

func (s *eventQueueStore) PruneSettled(ctx context.Context, before time.Time) (int, error) {
	res, err := s.conn.ExecContext(ctx, `
		DELETE FROM public.event_queue
		WHERE status IN ('done', 'cancelled') AND done_at < $1
	`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// The store is also the event queue's WorkKindHandle: what the operator
// surface and the metrics depth observer see of this table. The surface runs
// the package's own reads and controls on Conn, org-scoped by argument on the
// admin pool, so the org-admin predicate in the handler is the authorization,
// not RLS.
var _ db.WorkKindHandle = (*eventQueueStore)(nil)

func (s *eventQueueStore) Name() string          { return workkinds.EventQueueName }
func (s *eventQueueStore) Label() string         { return workkinds.EventQueueLabel }
func (s *eventQueueStore) Kind() workitem.Kind   { return s.kind }
func (s *eventQueueStore) Conn() workitem.DBTX   { return s.conn }
func (s *eventQueueStore) Access() db.WorkAccess { return db.WorkAccessOrgAdmin }

// Controls: redrive and cancel, never supersede. A supersede records a
// replacement row, and a parked event has none — one whose work has since
// been done another way is redriven and converges to a no-op through the
// routing fences (the tasks dedup index, the (triggering_event_id,
// trigger_id) replay fence, the one-active-run index).
func (s *eventQueueStore) Controls() db.WorkControls {
	return db.WorkControls{Redrive: true, Cancel: true}
}

func (s *eventQueueStore) Objective() db.WorkObjective {
	return db.WorkObjective{OldestReadyAge: workkinds.EventQueueOldestReadyObjective}
}

// Describe names each row by the entity its event was about. LEFT JOIN, not
// JOIN: entity_id is nullable and a queue row can outlive the entity it named
// (a row enqueued without an entity, or read mid-cascade), and such a row is
// still described — by its event type — because omitting it would hide a
// parked event precisely because something unusual happened to it. The join
// is bound on org_id as well as id: the composite FK means the pair is what
// identifies an entity, and binding only id would let a cross-org id
// collision join the wrong title in.
func (s *eventQueueStore) Describe(ctx context.Context, orgID string, ids []int64) (map[int64]db.WorkSubject, error) {
	if len(ids) == 0 {
		return map[int64]db.WorkSubject{}, nil
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT q.id, q.event_id, q.event_type,
		       COALESCE(q.entity_id::text, ''), COALESCE(e.source, ''), COALESCE(e.source_id, ''), COALESCE(e.title, '')
		FROM public.event_queue q
		LEFT JOIN public.entities e ON e.id = q.entity_id AND e.org_id = q.org_id
		WHERE q.org_id = $1 AND q.id = ANY($2)
	`, orgID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]db.WorkSubject, len(ids))
	for rows.Next() {
		var (
			id                                                    int64
			eventID, eventType, entityID, source, sourceID, title string
		)
		if err := rows.Scan(&id, &eventID, &eventType, &entityID, &source, &sourceID, &title); err != nil {
			return nil, err
		}
		out[id] = db.EventQueueSubject(eventID, eventType, entityID, source, sourceID, title)
	}
	return out, rows.Err()
}

func (s *eventQueueStore) UnsettledCloseExistsSystem(ctx context.Context, orgID, entityID string) (bool, error) {
	return unsettledCloseExists(ctx, s.conn, orgID, entityID)
}

func (s *eventQueueStore) ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.QueuedEvent, error) {
	rows, err := s.conn.QueryContext(ctx, pgQueuedEventSelect+`
		WHERE org_id = $1 AND entity_id = $2
		ORDER BY id
	`, orgID, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.QueuedEvent{}
	for rows.Next() {
		qe, err := scanPgQueuedEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, qe)
	}
	return out, rows.Err()
}

// scanPgQueuedEvent reads one pgQueuedEventSelect row.
func scanPgQueuedEvent(row interface{ Scan(...any) error }) (domain.QueuedEvent, error) {
	var (
		qe                                    domain.QueuedEvent
		nextAt, leasedAt, expiresAt, cancelAt sql.NullTime
		doneAt                                sql.NullTime
		leaseEpoch, supersededBy, pollSeq     sql.NullInt64
	)
	err := row.Scan(
		&qe.ID, &qe.OrgID, &qe.EventID, &qe.EntityID, &qe.EventType,
		&qe.Status, &qe.Attempt, &qe.MaxAttempts, &nextAt,
		&qe.LeaseGeneration, &qe.LeaseOwner, &leaseEpoch, &leasedAt, &expiresAt,
		&cancelAt, &qe.CancelRequestedBy, &qe.CancelReason,
		&qe.LastError, &qe.LastOutcome, &qe.UniqueKey, &supersededBy,
		&qe.FirstEnqueuedAt, &qe.CreatedAt, &doneAt,
		&qe.Traceparent, &pollSeq,
	)
	if err != nil {
		return domain.QueuedEvent{}, err
	}
	qe.NextAttemptAt = nullTimePtr(nextAt)
	qe.LeasedAt = nullTimePtr(leasedAt)
	qe.LeaseExpiresAt = nullTimePtr(expiresAt)
	qe.CancelRequestedAt = nullTimePtr(cancelAt)
	qe.DoneAt = nullTimePtr(doneAt)
	qe.LeaseEpoch = queueNullInt64(leaseEpoch)
	qe.SupersededBy = queueNullInt64(supersededBy)
	qe.EntityPollSeq = queueNullInt64(pollSeq)
	return qe, nil
}

func nullTimePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time.UTC()
	return &t
}

func queueNullInt64(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}
