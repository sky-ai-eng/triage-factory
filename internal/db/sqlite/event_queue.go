package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventQueueStore is the SQLite impl of db.EventQueueStore — the durable
// router queue, on the shared work-item contract. SQLite/local is a single
// process, so the package's claim needs no SKIP LOCKED: the handle's
// IMMEDIATE begin serializes claimers across processes and the pool's one
// connection serializes them within this one.
//
// Holds the *sql.DB directly (not the shared queryer) because Enqueue runs
// the events-row insert and the queue-row admission in one transaction —
// the outbox atomicity guarantee — which needs BeginTx, and because the
// package's claim opens its own transaction per round.
type eventQueueStore struct {
	conn *sql.DB
	kind workitem.Kind
}

func newEventQueueStore(conn *sql.DB) db.EventQueueStore {
	return &eventQueueStore{conn: conn, kind: workkinds.EventQueue(workitem.SQLite)}
}

var _ db.EventQueueStore = (*eventQueueStore)(nil)

func (s *eventQueueStore) Enqueue(ctx context.Context, orgID string, evt domain.Event, traceparent string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	var id string
	if err := db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
		// recordEvent (sqlite/events.go) is the canonical events insert —
		// reused so the queue's audit row matches every other event row
		// (ns-resolution created_at, nullable occurred_at, generated id).
		var err error
		id, err = recordEvent(ctx, tx, evt)
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
// both produce identical rows.
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
func unsettledCloseExists(ctx context.Context, q queryer, entityID string) (bool, error) {
	settling := domain.EntityCloseSettlingEventTypes()
	placeholders := make([]string, len(settling))
	args := []any{entityID}
	for i, et := range settling {
		placeholders[i] = "?"
		args = append(args, et)
	}
	var one int
	err := q.QueryRowContext(ctx, `
		SELECT 1 FROM event_queue
		WHERE entity_id = ?
		  AND status IN (`+unsettledCloseStatuses+`)
		  AND event_type IN (`+strings.Join(placeholders, ", ")+`)
		LIMIT 1
	`, args...).Scan(&one)
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
// SQLite/local is N=1 and has no concurrent leader to lose to, but the
// transaction is what closes the crash window, which is not a
// multi-mode-only concern.
func (s *eventQueueStore) EnqueueBatchWithSnapshotCAS(ctx context.Context, orgID, entityID, snapshotJSON string, expectedPollSeq int64, events []domain.Event, traceparents []string) (bool, []string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, nil, err
	}
	var won bool
	var ids []string
	if err := db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
		var err error
		won, err = updateSnapshotCAS(ctx, tx, entityID, snapshotJSON, expectedPollSeq)
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
				owed, err := unsettledCloseExists(ctx, tx, entityID)
				if err != nil {
					return err
				}
				if owed {
					ids = append(ids, "")
					continue
				}
				uniqueKey = workkinds.EventQueueCloseOwedKey(entityID)
			}
			id, err := recordEvent(ctx, tx, evt)
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

// sqliteQueuedEventSelect is the projection every QueuedEvent read answers
// with: the shared block and the kind's own columns.
const sqliteQueuedEventSelect = `
	SELECT id, org_id, event_id, COALESCE(entity_id, ''), event_type,
	       status, attempt, max_attempts, next_attempt_at,
	       lease_generation, COALESCE(lease_owner, ''), lease_epoch, leased_at, lease_expires_at,
	       cancel_requested_at, COALESCE(cancel_requested_by, ''), COALESCE(cancel_reason, ''),
	       COALESCE(last_error, ''), COALESCE(last_outcome, ''), COALESCE(unique_key, ''), superseded_by,
	       first_enqueued_at, created_at, done_at,
	       COALESCE(traceparent, ''), entity_poll_seq
	FROM event_queue`

func (s *eventQueueStore) Claim(ctx context.Context, owner workitem.Owner, n int) (db.EventQueueClaim, error) {
	res, claimErr := workitem.Claim(ctx, s.conn, s.kind, owner, "", n)
	out := db.EventQueueClaim{Cancelled: res.Cancelled, Parked: res.Parked, Reclaimed: res.Reclaimed}
	if len(res.Claimed) == 0 {
		return out, claimErr
	}
	// The kind's columns are immutable after admission, so reading them by
	// id after the claim reads the values the row was admitted with.
	placeholders := make([]string, len(res.Claimed))
	args := make([]any, 0, len(res.Claimed))
	for i, r := range res.Claimed {
		placeholders[i] = "?"
		args = append(args, r.ItemID)
	}
	rows, err := s.conn.QueryContext(ctx, sqliteQueuedEventSelect+`
		WHERE id IN (`+strings.Join(placeholders, ", ")+`)`, args...)
	if err != nil {
		return out, errors.Join(claimErr, err)
	}
	defer rows.Close()
	byID := make(map[int64]domain.QueuedEvent, len(res.Claimed))
	for rows.Next() {
		qe, err := scanSqliteQueuedEvent(rows)
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
	if err := assertLocalOrg(r.OrgID); err != nil {
		return workitem.Receipt{}, err
	}
	return workitem.RenewLease(ctx, s.conn, s.kind, r)
}

func (s *eventQueueStore) MarkDone(ctx context.Context, r workitem.Receipt) error {
	if err := assertLocalOrg(r.OrgID); err != nil {
		return err
	}
	return workitem.MarkDone(ctx, s.conn, s.kind, r)
}

func (s *eventQueueStore) Requeue(ctx context.Context, r workitem.Receipt, outcome workitem.Outcome, cause error) (bool, error) {
	if err := assertLocalOrg(r.OrgID); err != nil {
		return false, err
	}
	return workitem.Requeue(ctx, s.conn, s.kind, r, outcome, cause)
}

func (s *eventQueueStore) PruneSettled(ctx context.Context, before time.Time) (int, error) {
	// done_at is block text in the package's layout, so the cutoff is bound
	// in that layout for a plain text comparison.
	res, err := s.conn.ExecContext(ctx, `
		DELETE FROM event_queue
		WHERE status IN ('done', 'cancelled') AND done_at < ?
	`, before.UTC().Format(blockTimeLayout))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// sqliteParkedEventSelect is the projection both the list and the single read
// answer with, so a row read one way is byte-identical to the same row read
// the other. LEFT JOIN for the same reason as the Postgres impl: entity_id is
// nullable and a parked row must list whether or not its entity is still
// there. No org_id term on the join — local is N=1, so every row in both
// tables carries the one sentinel org and the extra predicate would only
// restate assertLocalOrg.
const sqliteParkedEventSelect = `
	SELECT q.id, q.event_type,
	       COALESCE(q.entity_id, ''), COALESCE(e.source, ''), COALESCE(e.source_id, ''), COALESCE(e.title, ''),
	       q.attempt, q.max_attempts, COALESCE(q.last_outcome, ''), COALESCE(q.last_error, ''),
	       q.first_enqueued_at, q.done_at
	FROM event_queue q
	LEFT JOIN entities e ON e.id = q.entity_id
	WHERE q.status = 'parked'`

func (s *eventQueueStore) ListParked(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.ParkedEvent, int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, 0, err
	}
	var total int
	if err := s.conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM event_queue WHERE status = 'parked'
	`).Scan(&total); err != nil {
		return nil, 0, err
	}
	if opts.CountOnly {
		return []domain.ParkedEvent{}, total, nil
	}

	// id DESC is enqueue order reversed — the most recently parked work
	// first — and it is a total order, so offset paging can neither drop nor
	// repeat a row between pages.
	query := sqliteParkedEventSelect + `
		ORDER BY q.id DESC`
	args := []any{}
	if opts.Limit > 0 {
		query += `
		LIMIT ? OFFSET ?`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.ParkedEvent{}
	for rows.Next() {
		pe, err := scanParkedEvent(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, pe)
	}
	return out, total, rows.Err()
}

func (s *eventQueueStore) GetParked(ctx context.Context, orgID string, id int64) (*domain.ParkedEvent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	pe, err := scanParkedEvent(s.conn.QueryRowContext(ctx, sqliteParkedEventSelect+`
		AND q.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pe, nil
}

// scanParkedEvent reads one sqliteParkedEventSelect row. It takes the narrow
// Scan interface so *sql.Row and *sql.Rows share it — the two reads must not
// drift on column order.
func scanParkedEvent(row interface{ Scan(...any) error }) (domain.ParkedEvent, error) {
	var (
		pe              domain.ParkedEvent
		firstEnqueuedAt blockTime
		parkedAt        blockTime
	)
	err := row.Scan(
		&pe.ID, &pe.EventType,
		&pe.EntityID, &pe.EntitySource, &pe.EntitySourceID, &pe.EntityTitle,
		&pe.Attempt, &pe.MaxAttempts, &pe.LastOutcome, &pe.LastError,
		&firstEnqueuedAt, &parkedAt,
	)
	pe.FirstEnqueuedAt = firstEnqueuedAt.Time
	pe.ParkedAt = parkedAt.Time
	return pe, err
}

func (s *eventQueueStore) Redrive(ctx context.Context, orgID string, ids []int64, by string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	moved := 0
	for _, id := range ids {
		switch err := workitem.Redrive(ctx, s.conn, s.kind, orgID, id, by); {
		case err == nil:
			moved++
		case errors.Is(err, workitem.ErrNotParked):
		default:
			return moved, err
		}
	}
	return moved, nil
}

func (s *eventQueueStore) UnsettledCloseExistsSystem(ctx context.Context, orgID, entityID string) (bool, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, err
	}
	return unsettledCloseExists(ctx, s.conn, entityID)
}

func (s *eventQueueStore) ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.QueuedEvent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx, sqliteQueuedEventSelect+`
		WHERE entity_id = ?
		ORDER BY id
	`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.QueuedEvent{}
	for rows.Next() {
		qe, err := scanSqliteQueuedEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, qe)
	}
	return out, rows.Err()
}

// scanSqliteQueuedEvent reads one sqliteQueuedEventSelect row.
func scanSqliteQueuedEvent(row interface{ Scan(...any) error }) (domain.QueuedEvent, error) {
	var (
		qe                                    domain.QueuedEvent
		nextAt, leasedAt, expiresAt, cancelAt blockTime
		firstEnqueuedAt, createdAt, doneAt    blockTime
		leaseEpoch, supersededBy, pollSeq     sql.NullInt64
	)
	err := row.Scan(
		&qe.ID, &qe.OrgID, &qe.EventID, &qe.EntityID, &qe.EventType,
		&qe.Status, &qe.Attempt, &qe.MaxAttempts, &nextAt,
		&qe.LeaseGeneration, &qe.LeaseOwner, &leaseEpoch, &leasedAt, &expiresAt,
		&cancelAt, &qe.CancelRequestedBy, &qe.CancelReason,
		&qe.LastError, &qe.LastOutcome, &qe.UniqueKey, &supersededBy,
		&firstEnqueuedAt, &createdAt, &doneAt,
		&qe.Traceparent, &pollSeq,
	)
	if err != nil {
		return domain.QueuedEvent{}, err
	}
	qe.NextAttemptAt = nextAt.ptr()
	qe.LeasedAt = leasedAt.ptr()
	qe.LeaseExpiresAt = expiresAt.ptr()
	qe.CancelRequestedAt = cancelAt.ptr()
	qe.FirstEnqueuedAt = firstEnqueuedAt.Time
	qe.CreatedAt = createdAt.Time
	qe.DoneAt = doneAt.ptr()
	qe.LeaseEpoch = nullInt64Ptr(leaseEpoch)
	qe.SupersededBy = nullInt64Ptr(supersededBy)
	qe.EntityPollSeq = nullInt64Ptr(pollSeq)
	return qe, nil
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

// blockTimeLayout is the shape every work-item block timestamp carries on
// SQLite: what strftime('%Y-%m-%d %H:%M:%f') renders, in UTC.
const blockTimeLayout = "2006-01-02 15:04:05.000"

// blockTimeLayouts are the shapes a block timestamp can arrive in on a read:
// the layout the package writes, and the driver's own for a column another
// writer bound from Go.
var blockTimeLayouts = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05.999999999-07:00",
	time.RFC3339Nano,
}

// blockTime scans a work-item block timestamp. The block's columns are TEXT
// rather than DATETIME, so the driver hands back the string the package
// wrote instead of parsing it, and this is where it becomes a time.
type blockTime struct {
	Valid bool
	Time  time.Time
}

func (b *blockTime) Scan(src any) error {
	b.Valid, b.Time = false, time.Time{}
	switch v := src.(type) {
	case nil:
		return nil
	case time.Time:
		b.Valid, b.Time = true, v.UTC()
		return nil
	case []byte:
		return b.parse(string(v))
	case string:
		return b.parse(v)
	default:
		return fmt.Errorf("event_queue: cannot scan %T as a timestamp", src)
	}
}

func (b *blockTime) parse(s string) error {
	if s == "" {
		return nil
	}
	for _, layout := range blockTimeLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			b.Valid, b.Time = true, t.UTC()
			return nil
		}
	}
	return fmt.Errorf("event_queue: cannot parse %q as a timestamp", s)
}

func (b blockTime) ptr() *time.Time {
	if !b.Valid {
		return nil
	}
	t := b.Time
	return &t
}
