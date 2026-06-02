package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventQueueStore is the SQLite impl of db.EventQueueStore (SKY-414) —
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

func (s *eventQueueStore) Enqueue(ctx context.Context, orgID string, evt domain.Event) (string, error) {
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

	var entityID any
	if evt.EntityID != nil && *evt.EntityID != "" {
		entityID = *evt.EntityID
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_queue (event_id, entity_id, event_type, status)
		VALUES (?, ?, ?, 'pending')
	`, id, entityID, evt.EventType); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *eventQueueStore) ClaimNext(ctx context.Context) (*domain.QueuedEvent, error) {
	// Single-statement claim: flip the oldest pending row to processing
	// and return it. With one worker there's no contention, so the
	// scalar-subquery form is sufficient (Postgres adds SKIP LOCKED for
	// the multi-worker case). A NULL subquery (empty queue) matches no
	// row, RETURNING yields nothing, and the scan reports ErrNoRows.
	row := s.conn.QueryRowContext(ctx, `
		UPDATE event_queue
		SET status = 'processing', claimed_at = ?, attempts = attempts + 1
		WHERE id = (SELECT id FROM event_queue WHERE status = 'pending' ORDER BY id LIMIT 1)
		RETURNING id, org_id, event_id, COALESCE(entity_id, ''), event_type,
		          status, attempts, COALESCE(last_error, ''), enqueued_at, claimed_at, processed_at
	`, time.Now())
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
	// next claim.
	_, err := s.conn.ExecContext(ctx, `
		UPDATE event_queue SET status = 'pending', last_error = ?, claimed_at = NULL
		WHERE id = ? AND status = 'processing'
	`, lastErr, id)
	return err
}

func (s *eventQueueStore) ResetProcessing(ctx context.Context) (int, error) {
	res, err := s.conn.ExecContext(ctx, `
		UPDATE event_queue SET status = 'pending', claimed_at = NULL
		WHERE status = 'processing'
	`)
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

func (s *eventQueueStore) ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.QueuedEvent, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id, org_id, event_id, COALESCE(entity_id, ''), event_type,
		       status, attempts, COALESCE(last_error, ''), enqueued_at, claimed_at, processed_at
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
