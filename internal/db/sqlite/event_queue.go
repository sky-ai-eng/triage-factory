package sqlite

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventQueueStore is the SQLite impl of db.EventQueueStore —
// the durable router queue. SQLite/local is single-worker (N=1), so
// ClaimNext doesn't need the FOR UPDATE SKIP LOCKED the Postgres impl
// uses; the UPDATE ... WHERE id = (oldest pending) RETURNING form is
// atomic enough with one drainer.
//
// Holds the *sql.DB directly (not the shared queryer) because Enqueue
// runs the events-row insert and the queue-row insert in one transaction
// — the outbox atomicity guarantee — which needs BeginTx.
type eventQueueStore struct {
	conn *sql.DB
}

func newEventQueueStore(conn *sql.DB) db.EventQueueStore {
	return &eventQueueStore{conn: conn}
}

var _ db.EventQueueStore = (*eventQueueStore)(nil)

func (s *eventQueueStore) Enqueue(ctx context.Context, orgID string, evt domain.Event, traceparent string) (string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return "", err
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	// recordEvent (sqlite/events.go) is the canonical events insert —
	// reused so the queue's audit row matches every other event row
	// (ns-resolution created_at, nullable occurred_at, generated id).
	id, err := recordEvent(ctx, tx, evt)
	if err != nil {
		return "", err
	}

	if err := insertQueueRow(ctx, tx, id, evt, traceparent); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// insertQueueRow writes the pending queue row for an already-recorded
// event. Shared by Enqueue and EnqueueBatchWithSnapshotCAS so both produce
// identical rows.
func insertQueueRow(ctx context.Context, q queryer, eventID string, evt domain.Event, traceparent string) error {
	var entityID any
	if evt.EntityID != nil && *evt.EntityID != "" {
		entityID = *evt.EntityID
	}
	// NULLIF on traceparent: an untraced producer stores NULL rather than
	// an empty string, so "no context to link" is one value in the column,
	// not two.
	_, err := q.ExecContext(ctx, `
		INSERT INTO event_queue (event_id, entity_id, event_type, status, traceparent)
		VALUES (?, ?, ?, 'pending', NULLIF(?, ''))
	`, eventID, entityID, evt.EventType, traceparent)
	return err
}

// EnqueueBatchWithSnapshotCAS runs the snapshot CAS and the batch's
// events + event_queue writes in one transaction — see the interface doc
// for why they belong together. A CAS that matches zero rows returns
// ok=false having written nothing: the rollback is what makes a losing
// writer invisible rather than half-applied. SQLite/local is N=1 and has
// no concurrent leader to lose to, but the transaction is what closes the
// crash window, which is not a multi-mode-only concern.
func (s *eventQueueStore) EnqueueBatchWithSnapshotCAS(ctx context.Context, orgID, entityID, snapshotJSON string, expectedPollSeq int64, events []domain.Event, traceparents []string) (bool, []string, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return false, nil, err
	}
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	won, err := updateSnapshotCAS(ctx, tx, entityID, snapshotJSON, expectedPollSeq)
	if err != nil {
		return false, nil, err
	}
	if !won {
		return false, nil, nil
	}

	ids := make([]string, 0, len(events))
	for i, evt := range events {
		id, err := recordEvent(ctx, tx, evt)
		if err != nil {
			return false, nil, err
		}
		if err := insertQueueRow(ctx, tx, id, evt, db.TraceparentAt(traceparents, i)); err != nil {
			return false, nil, err
		}
		ids = append(ids, id)
	}
	if err := tx.Commit(); err != nil {
		return false, nil, err
	}
	return true, ids, nil
}

func (s *eventQueueStore) ClaimNext(ctx context.Context, executorID string, bootEpoch int64) (*domain.QueuedEvent, error) {
	// Single-statement claim: flip the oldest pending row to processing
	// and return it. With one worker there's no contention, so the
	// scalar-subquery form is sufficient (Postgres adds SKIP LOCKED for
	// the multi-worker case). A NULL subquery (empty queue) matches no
	// row, RETURNING yields nothing, and the scan reports ErrNoRows.
	//
	// executor_id + boot_epoch are stamped in this same statement,
	// mirroring the Postgres impl — see ResetProcessing.
	row := s.conn.QueryRowContext(ctx, `
		UPDATE event_queue
		SET status = 'processing', claimed_at = ?, attempts = attempts + 1,
		    executor_id = NULLIF(?, ''),
		    boot_epoch  = CASE WHEN ? = '' THEN NULL ELSE ? END
		WHERE id = (SELECT id FROM event_queue WHERE status = 'pending' ORDER BY id LIMIT 1)
		RETURNING id, org_id, event_id, COALESCE(entity_id, ''), event_type,
		          status, attempts, COALESCE(last_error, ''), enqueued_at, claimed_at, processed_at,
		          COALESCE(traceparent, '')
	`, time.Now(), executorID, executorID, bootEpoch)
	return scanSqliteQueuedEvent(row)
}

func (s *eventQueueStore) MarkDone(ctx context.Context, orgID string, id int64) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.conn.ExecContext(ctx, `
		UPDATE event_queue SET status = 'done', processed_at = ?
		WHERE id = ? AND status = 'processing'
	`, time.Now(), id)
	return err
}

func (s *eventQueueStore) MarkFailed(ctx context.Context, orgID string, id int64, lastErr string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.conn.ExecContext(ctx, `
		UPDATE event_queue SET status = 'failed', processed_at = ?, last_error = ?
		WHERE id = ? AND status = 'processing'
	`, time.Now(), lastErr, id)
	return err
}

func (s *eventQueueStore) Requeue(ctx context.Context, orgID string, id int64, lastErr string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// attempts is intentionally left as-is — the claim already counted
	// this try, so the worker can fail the row out once attempts crosses
	// its budget. claimed_at is cleared so the row reads clean for the
	// next claim. The ownership stamp is cleared too: a pending row has
	// no owner (mirrors the Postgres impl).
	_, err := s.conn.ExecContext(ctx, `
		UPDATE event_queue SET status = 'pending', last_error = ?, claimed_at = NULL,
			executor_id = NULL, boot_epoch = NULL
		WHERE id = ? AND status = 'processing'
	`, lastErr, id)
	return err
}

func (s *eventQueueStore) ResetProcessing(ctx context.Context, executorID string, bootEpoch int64) (int, error) {
	// Ownership-scoped, mirroring the Postgres impl. SQLite is
	// N=1, so this is trivially self-scoped, but the predicate keeps the two
	// backends' semantics identical. `boot_epoch IS NULL` is a narrow
	// defensive catch (this instance's persistent id, no epoch); pre-registry
	// rows (random per-boot uuid or NULL executor_id) are normalized once by
	// migration 202607080003_pre_registry_orphan_normalization, not by this
	// sweep. The reset clears the stamp: a pending row has no owner.
	res, err := s.conn.ExecContext(ctx, `
		UPDATE event_queue SET status = 'pending', claimed_at = NULL,
			executor_id = NULL, boot_epoch = NULL
		WHERE status = 'processing'
		  AND executor_id = ?
		  AND (boot_epoch IS NULL OR boot_epoch < ?)
	`, executorID, bootEpoch)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *eventQueueStore) RequeueStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error) {
	// The staleness backstop under ResetProcessing — see the interface doc.
	// SQLite/local is N=1, so the case that motivated it (an owner replaced
	// rather than rebooted) can't arise here; what it still catches is a row
	// left 'processing' by a hard crash wearing an ownership stamp the boot
	// reset's predicate doesn't match. The method exists so both backends
	// hold one contract and the drain worker sweeps without branching on
	// mode. The cutoff is computed in Go because claimed_at is written from
	// this same process's clock, so there is no second clock to skew
	// against. A NULL claimed_at 'processing' row is reclaimed
	// unconditionally, mirroring the Postgres impl.
	res, err := s.conn.ExecContext(ctx, `
		UPDATE event_queue SET status = 'pending', last_error = ?, claimed_at = NULL,
			executor_id = NULL, boot_epoch = NULL
		WHERE status = 'processing'
		  AND (claimed_at IS NULL OR claimed_at < ?)
	`, db.StaleProcessingReclaimReason, time.Now().Add(-olderThan))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *eventQueueStore) PruneDone(ctx context.Context, before time.Time) (int, error) {
	res, err := s.conn.ExecContext(ctx, `
		DELETE FROM event_queue WHERE status = 'done' AND processed_at < ?
	`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *eventQueueStore) ListFailedEvents(ctx context.Context, orgID string, limit int) ([]domain.FailedEvent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// LEFT JOIN for the same reason as the Postgres impl: entity_id is
	// nullable and a parked row must list whether or not its entity is still
	// there. No org_id term on the join — local is N=1, so every row in both
	// tables carries the one sentinel org and the extra predicate would only
	// restate assertLocalOrg above.
	//
	// id DESC is enqueue order reversed: most recently dropped work first.
	rows, err := s.conn.QueryContext(ctx, `
		SELECT q.id, q.event_type,
		       COALESCE(q.entity_id, ''), COALESCE(e.source, ''), COALESCE(e.source_id, ''), COALESCE(e.title, ''),
		       q.attempts, COALESCE(q.last_error, ''), q.enqueued_at
		FROM event_queue q
		LEFT JOIN entities e ON e.id = q.entity_id
		WHERE q.status = 'failed'
		ORDER BY q.id DESC
		LIMIT ?
	`, db.NormalizeFailedEventsLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.FailedEvent{}
	for rows.Next() {
		var fe domain.FailedEvent
		if err := rows.Scan(
			&fe.ID, &fe.EventType,
			&fe.EntityID, &fe.EntitySource, &fe.EntitySourceID, &fe.EntityTitle,
			&fe.Attempts, &fe.LastError, &fe.EnqueuedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, fe)
	}
	return out, rows.Err()
}

func (s *eventQueueStore) RequeueFailedEvents(ctx context.Context, orgID string, ids []int64) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	// SQLite has no array bind, so the id set becomes a ?-placeholder IN list,
	// chunked to stay inside the variable limit (inListChunkSize) the way
	// every other caller-supplied id slice in this package is. Each id falls
	// in exactly one chunk, so summing the per-chunk RowsAffected gives the
	// same count Postgres returns from its single statement.
	//
	// status = 'failed' is both selector and guard, and last_error is
	// untouched — see the interface doc.
	total := 0
	for start := 0; start < len(ids); start += inListChunkSize {
		end := min(start+inListChunkSize, len(ids))
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]any, 0, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args = append(args, id)
		}
		res, err := s.conn.ExecContext(ctx, `
			UPDATE event_queue
			SET status = 'pending', attempts = 0, claimed_at = NULL, processed_at = NULL,
				executor_id = NULL, boot_epoch = NULL
			WHERE status = 'failed' AND id IN (`+strings.Join(placeholders, ", ")+`)
		`, args...)
		if err != nil {
			return total, err
		}
		// Unlike the sweeps above, the count here is the operator's answer —
		// "2 of the 3 you picked moved" — so a driver that cannot report it
		// must say so rather than let a discarded error render as "nothing
		// moved" over rows that did. The partial total rides along with the
		// error: earlier chunks committed, and a caller logging the failure
		// should see that they did.
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += int(n)
	}
	return total, nil
}

func (s *eventQueueStore) ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.QueuedEvent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id, org_id, event_id, COALESCE(entity_id, ''), event_type,
		       status, attempts, COALESCE(last_error, ''), enqueued_at, claimed_at, processed_at,
		       COALESCE(traceparent, '')
		FROM event_queue
		WHERE entity_id = ?
		ORDER BY id
	`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.QueuedEvent{}
	for rows.Next() {
		qe, err := scanSqliteQueuedEventRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *qe)
	}
	return out, rows.Err()
}

// scanSqliteQueuedEvent scans a sql.Row into *domain.QueuedEvent.
// (nil, nil) on sql.ErrNoRows so callers can treat "empty queue" as a
// non-error empty result.
func scanSqliteQueuedEvent(row *sql.Row) (*domain.QueuedEvent, error) {
	var (
		qe          domain.QueuedEvent
		claimedAt   sql.NullTime
		processedAt sql.NullTime
	)
	err := row.Scan(
		&qe.ID, &qe.OrgID, &qe.EventID, &qe.EntityID, &qe.EventType,
		&qe.Status, &qe.Attempts, &qe.LastError, &qe.EnqueuedAt, &claimedAt, &processedAt,
		&qe.Traceparent,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	applyQueuedEventNullTimes(&qe, claimedAt, processedAt)
	return &qe, nil
}

// scanSqliteQueuedEventRow is the sql.Rows variant.
func scanSqliteQueuedEventRow(rows *sql.Rows) (*domain.QueuedEvent, error) {
	var (
		qe          domain.QueuedEvent
		claimedAt   sql.NullTime
		processedAt sql.NullTime
	)
	err := rows.Scan(
		&qe.ID, &qe.OrgID, &qe.EventID, &qe.EntityID, &qe.EventType,
		&qe.Status, &qe.Attempts, &qe.LastError, &qe.EnqueuedAt, &claimedAt, &processedAt,
		&qe.Traceparent,
	)
	if err != nil {
		return nil, err
	}
	applyQueuedEventNullTimes(&qe, claimedAt, processedAt)
	return &qe, nil
}

func applyQueuedEventNullTimes(qe *domain.QueuedEvent, claimedAt, processedAt sql.NullTime) {
	if claimedAt.Valid {
		t := claimedAt.Time
		qe.ClaimedAt = &t
	}
	if processedAt.Valid {
		t := processedAt.Time
		qe.ProcessedAt = &t
	}
}
