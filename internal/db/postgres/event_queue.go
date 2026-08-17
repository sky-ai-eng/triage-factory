package postgres

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// eventQueueStore is the Postgres impl of db.EventQueueStore —
// the durable router queue. Wired against the admin pool in postgres.New:
// the ingestor and drain worker are system services with no per-user
// identity, so impersonating a user via the app pool would be wrong. The
// event_queue_all RLS policy is defense-in-depth (admin bypasses it) and
// org_id is bound in every statement.
//
// Holds the admin *sql.DB directly (not the shared queryer) because
// Enqueue runs the events-row insert and the queue-row insert in one
// transaction — the outbox atomicity guarantee — which needs BeginTx.
type eventQueueStore struct {
	conn *sql.DB
}

func newEventQueueStore(conn *sql.DB) db.EventQueueStore {
	return &eventQueueStore{conn: conn}
}

var _ db.EventQueueStore = (*eventQueueStore)(nil)

func (s *eventQueueStore) Enqueue(ctx context.Context, orgID string, evt domain.Event, traceparent string) (string, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	// recordEvent (postgres/events.go) is the canonical events insert —
	// reused so the queue's audit row matches every other event row
	// (jsonb metadata cast, nullable occurred_at, org_id bind).
	id, err := recordEvent(ctx, tx, orgID, evt)
	if err != nil {
		return "", err
	}

	if err := insertQueueRow(ctx, tx, orgID, id, evt, traceparent); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// insertQueueRow writes the pending queue row for an already-recorded
// event. Shared by Enqueue and EnqueueBatchWithSnapshotCAS so both produce
// byte-identical rows.
func insertQueueRow(ctx context.Context, q queryer, orgID, eventID string, evt domain.Event, traceparent string) error {
	var entityID any
	if evt.EntityID != nil && *evt.EntityID != "" {
		entityID = *evt.EntityID
	}
	// NULLIF on traceparent: an untraced producer stores NULL rather than
	// an empty string, so "no context to link" is one value in the column,
	// not two.
	_, err := q.ExecContext(ctx, `
		INSERT INTO public.event_queue (org_id, event_id, entity_id, event_type, status, traceparent)
		VALUES ($1, $2, $3, $4, 'pending', NULLIF($5, ''))
	`, orgID, eventID, entityID, evt.EventType, traceparent)
	return err
}

// EnqueueBatchWithSnapshotCAS runs the snapshot CAS and the batch's
// events + event_queue writes in one transaction — see the interface doc
// for why they belong together. A CAS that matches zero rows returns
// ok=false having written nothing: the rollback is what makes a losing
// writer invisible rather than half-applied.
func (s *eventQueueStore) EnqueueBatchWithSnapshotCAS(ctx context.Context, orgID, entityID, snapshotJSON string, expectedPollSeq int64, events []domain.Event, traceparents []string) (bool, []string, error) {
	tx, err := s.conn.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit

	won, err := updateSnapshotCAS(ctx, tx, orgID, entityID, snapshotJSON, expectedPollSeq)
	if err != nil {
		return false, nil, err
	}
	if !won {
		return false, nil, nil
	}

	ids := make([]string, 0, len(events))
	for i, evt := range events {
		id, err := recordEvent(ctx, tx, orgID, evt)
		if err != nil {
			return false, nil, err
		}
		if err := insertQueueRow(ctx, tx, orgID, id, evt, db.TraceparentAt(traceparents, i)); err != nil {
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
	// FOR UPDATE SKIP LOCKED on the inner select so multiple router
	// workers can drain concurrently without ever claiming the same row
	// — the horizontal-routing groundwork (running N workers is a
	// non-goal for now). The single-statement UPDATE ... RETURNING is
	// atomic; an empty queue matches no row and the scan reports
	// ErrNoRows -> (nil, nil).
	//
	// executor_id + boot_epoch are stamped in this same statement,
	// mirroring ConversationQueueStore.ClaimNextConversation — see ResetProcessing.
	row := s.conn.QueryRowContext(ctx, `
		UPDATE public.event_queue
		SET status = 'processing', claimed_at = now(), attempts = attempts + 1,
		    executor_id = NULLIF($1, ''),
		    boot_epoch  = CASE WHEN $1 = '' THEN NULL ELSE $2::bigint END
		WHERE id = (
			SELECT id FROM public.event_queue
			WHERE status = 'pending'
			ORDER BY id
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING id, org_id, event_id, entity_id, event_type,
		          status, attempts, COALESCE(last_error, ''), enqueued_at, claimed_at, processed_at,
		          COALESCE(traceparent, '')
	`, executorID, bootEpoch)
	return scanPgQueuedEvent(row)
}

func (s *eventQueueStore) MarkDone(ctx context.Context, orgID string, id int64) error {
	_, err := s.conn.ExecContext(ctx, `
		UPDATE public.event_queue SET status = 'done', processed_at = now()
		WHERE org_id = $1 AND id = $2 AND status = 'processing'
	`, orgID, id)
	return err
}

func (s *eventQueueStore) MarkFailed(ctx context.Context, orgID string, id int64, lastErr string) error {
	_, err := s.conn.ExecContext(ctx, `
		UPDATE public.event_queue SET status = 'failed', processed_at = now(), last_error = $3
		WHERE org_id = $1 AND id = $2 AND status = 'processing'
	`, orgID, id, lastErr)
	return err
}

func (s *eventQueueStore) Requeue(ctx context.Context, orgID string, id int64, lastErr string) error {
	// attempts left as-is (the claim counted this try); claimed_at
	// cleared so the row reads clean for the next claim. The ownership
	// stamp is cleared too: a pending row has no owner.
	_, err := s.conn.ExecContext(ctx, `
		UPDATE public.event_queue SET status = 'pending', last_error = $3, claimed_at = NULL,
			executor_id = NULL, boot_epoch = NULL
		WHERE org_id = $1 AND id = $2 AND status = 'processing'
	`, orgID, id, lastErr)
	return err
}

func (s *eventQueueStore) ResetProcessing(ctx context.Context, executorID string, bootEpoch int64) (int, error) {
	// Ownership-scoped, mirroring ConversationQueueStore.ResetProcessingConversations:
	// only rows this instance itself claimed (executor_id = $1) during a
	// strictly earlier boot (boot_epoch < $2) are reset. A live sibling's
	// still-processing row carries a different executor_id and is never
	// touched. `boot_epoch IS NULL` is a narrow defensive catch (this
	// instance's persistent id, no epoch — a state only pre-epoch
	// from-source builds could produce); it does NOT cover pre-registry
	// rows (random per-boot uuid or NULL executor_id) — moot on Postgres
	// (fresh-installs-only posture), normalized on SQLite by migration
	// 202607080003_pre_registry_orphan_normalization. The reset clears
	// the stamp: a pending row has no owner.
	res, err := s.conn.ExecContext(ctx, `
		UPDATE public.event_queue SET status = 'pending', claimed_at = NULL,
			executor_id = NULL, boot_epoch = NULL
		WHERE status = 'processing'
		  AND executor_id = $1
		  AND (boot_epoch IS NULL OR boot_epoch < $2)
	`, executorID, bootEpoch)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *eventQueueStore) RequeueStaleProcessing(ctx context.Context, olderThan time.Duration) (int, error) {
	// The staleness backstop under ResetProcessing — see the interface doc
	// for why it errs toward reclaiming. Unscoped by ownership on purpose:
	// the row this exists for belongs to an instance id that is never coming
	// back, so scoping it to any live identity would skip exactly the case
	// it covers.
	//
	// The cutoff is computed from now() — the same server clock that stamped
	// claimed_at — rather than from a caller-supplied timestamp, so pod clock
	// skew can't turn a fresh claim into a stale one. A NULL claimed_at
	// 'processing' row is reclaimed unconditionally (mirroring
	// PendingFirings.RequeueStaleDraining): the claim stamps both columns in
	// one statement so it should not exist, and if it somehow does it is
	// stranded by every other predicate.
	res, err := s.conn.ExecContext(ctx, `
		UPDATE public.event_queue SET status = 'pending', last_error = $2, claimed_at = NULL,
			executor_id = NULL, boot_epoch = NULL
		WHERE status = 'processing'
		  AND (claimed_at IS NULL OR claimed_at < now() - make_interval(secs => $1::double precision))
	`, olderThan.Seconds(), db.StaleProcessingReclaimReason)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *eventQueueStore) PruneDone(ctx context.Context, before time.Time) (int, error) {
	res, err := s.conn.ExecContext(ctx, `
		DELETE FROM public.event_queue WHERE status = 'done' AND processed_at < $1
	`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// pgFailedEventSelect is the projection both the list and the single read
// answer with, so a row read one way is byte-identical to the same row read
// the other.
//
// LEFT JOIN, not JOIN: entity_id is nullable and a queue row can outlive the
// entity it named (the FK cascades, but a parked row read mid-cascade or one
// enqueued without an entity must still list). The join is bound on org_id as
// well as id — the composite FK means the pair is what identifies an entity,
// and binding only id would let a future cross-org id collision join the wrong
// title in.
const pgFailedEventSelect = `
	SELECT q.id, q.event_type,
	       COALESCE(q.entity_id::text, ''), COALESCE(e.source, ''), COALESCE(e.source_id, ''), COALESCE(e.title, ''),
	       q.attempts, COALESCE(q.last_error, ''), q.enqueued_at
	FROM public.event_queue q
	LEFT JOIN public.entities e ON e.id = q.entity_id AND e.org_id = q.org_id
	WHERE q.org_id = $1 AND q.status = 'failed'`

func (s *eventQueueStore) ListFailedEvents(ctx context.Context, orgID string, opts db.ListOpts) ([]domain.FailedEvent, int, error) {
	var total int
	if err := s.conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM public.event_queue WHERE org_id = $1 AND status = 'failed'
	`, orgID).Scan(&total); err != nil {
		return nil, 0, err
	}

	// id DESC is enqueue order reversed: the most recently dropped work is
	// what an operator is looking for, and id is monotonic per insert so the
	// order is total and stable across pages.
	query := pgFailedEventSelect + `
		ORDER BY q.id DESC`
	args := []any{orgID}
	if opts.Limit > 0 {
		query += `
		LIMIT $2 OFFSET $3`
		args = append(args, opts.Limit, opts.Offset)
	}
	rows, err := s.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []domain.FailedEvent{}
	for rows.Next() {
		fe, err := scanFailedEvent(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, fe)
	}
	return out, total, rows.Err()
}

func (s *eventQueueStore) GetFailedEvent(ctx context.Context, orgID string, id int64) (*domain.FailedEvent, error) {
	fe, err := scanFailedEvent(s.conn.QueryRowContext(ctx, pgFailedEventSelect+`
		AND q.id = $2`, orgID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &fe, nil
}

// scanFailedEvent reads one pgFailedEventSelect row. It takes the narrow Scan
// interface so *sql.Row and *sql.Rows share it — the two reads must not drift
// on column order.
func scanFailedEvent(row interface{ Scan(...any) error }) (domain.FailedEvent, error) {
	var fe domain.FailedEvent
	err := row.Scan(
		&fe.ID, &fe.EventType,
		&fe.EntityID, &fe.EntitySource, &fe.EntitySourceID, &fe.EntityTitle,
		&fe.Attempts, &fe.LastError, &fe.EnqueuedAt,
	)
	return fe, err
}

func (s *eventQueueStore) RequeueFailedEvents(ctx context.Context, orgID string, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	// status = 'failed' is both the selector and the guard: it makes the
	// operation idempotent (a second requeue matches nothing) and keeps an
	// operator from resetting the attempts of a row another worker is
	// actively driving. last_error is untouched on purpose — see the
	// interface doc.
	res, err := s.conn.ExecContext(ctx, `
		UPDATE public.event_queue
		SET status = 'pending', attempts = 0, claimed_at = NULL, processed_at = NULL,
			executor_id = NULL, boot_epoch = NULL
		WHERE org_id = $1 AND status = 'failed' AND id = ANY($2)
	`, orgID, ids)
	if err != nil {
		return 0, err
	}
	// Unlike the sweeps above, the count here is the operator's answer — "2 of
	// the 3 you picked moved" — so a driver that cannot report it must say so
	// rather than let a discarded error render as "nothing moved" over rows
	// that did.
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *eventQueueStore) ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.QueuedEvent, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT id, org_id, event_id, entity_id, event_type,
		       status, attempts, COALESCE(last_error, ''), enqueued_at, claimed_at, processed_at,
		       COALESCE(traceparent, '')
		FROM public.event_queue
		WHERE org_id = $1 AND entity_id = $2
		ORDER BY id
	`, orgID, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.QueuedEvent{}
	for rows.Next() {
		qe, err := scanPgQueuedEventRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *qe)
	}
	return out, rows.Err()
}

// scanPgQueuedEvent scans a sql.Row into *domain.QueuedEvent. (nil, nil)
// on sql.ErrNoRows so callers treat "empty queue" as a non-error empty
// result.
func scanPgQueuedEvent(row *sql.Row) (*domain.QueuedEvent, error) {
	var (
		qe          domain.QueuedEvent
		entityID    sql.NullString
		claimedAt   sql.NullTime
		processedAt sql.NullTime
	)
	err := row.Scan(
		&qe.ID, &qe.OrgID, &qe.EventID, &entityID, &qe.EventType,
		&qe.Status, &qe.Attempts, &qe.LastError, &qe.EnqueuedAt, &claimedAt, &processedAt,
		&qe.Traceparent,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	applyPgQueuedEventNulls(&qe, entityID, claimedAt, processedAt)
	return &qe, nil
}

// scanPgQueuedEventRow is the sql.Rows variant.
func scanPgQueuedEventRow(rows *sql.Rows) (*domain.QueuedEvent, error) {
	var (
		qe          domain.QueuedEvent
		entityID    sql.NullString
		claimedAt   sql.NullTime
		processedAt sql.NullTime
	)
	err := rows.Scan(
		&qe.ID, &qe.OrgID, &qe.EventID, &entityID, &qe.EventType,
		&qe.Status, &qe.Attempts, &qe.LastError, &qe.EnqueuedAt, &claimedAt, &processedAt,
		&qe.Traceparent,
	)
	if err != nil {
		return nil, err
	}
	applyPgQueuedEventNulls(&qe, entityID, claimedAt, processedAt)
	return &qe, nil
}

func applyPgQueuedEventNulls(qe *domain.QueuedEvent, entityID sql.NullString, claimedAt, processedAt sql.NullTime) {
	if entityID.Valid {
		qe.EntityID = entityID.String
	}
	if claimedAt.Valid {
		t := claimedAt.Time
		qe.ClaimedAt = &t
	}
	if processedAt.Valid {
		t := processedAt.Time
		qe.ProcessedAt = &t
	}
}
