package postgres

import (
	"context"
	"database/sql"
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

	var entityID any
	if evt.EntityID != nil && *evt.EntityID != "" {
		entityID = *evt.EntityID
	}
	// NULLIF on traceparent: an untraced producer stores NULL rather than
	// an empty string, so "no context to link" is one value in the column,
	// not two.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO public.event_queue (org_id, event_id, entity_id, event_type, status, traceparent)
		VALUES ($1, $2, $3, $4, 'pending', NULLIF($5, ''))
	`, orgID, id, entityID, evt.EventType, traceparent); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

func (s *eventQueueStore) ClaimNext(ctx context.Context, executorID string, bootEpoch int64) (*domain.QueuedEvent, error) {
	// FOR UPDATE SKIP LOCKED on the inner select so multiple router
	// workers can drain concurrently without ever claiming the same row
	// — the horizontal-routing groundwork (running N workers is a
	// non-goal for now). The single-statement UPDATE ... RETURNING is
	// atomic; an empty queue matches no row and the scan reports
	// ErrNoRows -> (nil, nil).
	//
	// executor_id + boot_epoch are stamped in this same statement (TFAC-578),
	// mirroring RunQueueStore.ClaimNextRun — see ResetProcessing.
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
	// Ownership-scoped (TFAC-578), mirroring RunQueueStore.ResetProcessingRuns:
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
