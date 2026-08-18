package postgres

import (
	"context"
	"database/sql"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// pendingFiringsStore is the Postgres impl of db.PendingFiringsStore.
// Wired against the admin pool in postgres.New: the router has no per-
// user identity and the drain sweeper runs as a background goroutine,
// so impersonating any one user via the app pool would be wrong.
// Defense-in-depth org_id filters fire in every WHERE/INSERT clause
// alongside the RLS policy (which gates by an EXISTS subquery against
// tasks rather than a bare current_org_id() check — the policy is
// kept honest by the explicit filter here in case the policy is ever
// loosened).
//
// The per-task firing gate's conversation-shaped half lives on
// ConversationStore —
// strict ownership. The router composes the gate from this store's
// HasPendingForTask + ConversationStore's HasActiveAutoConversationForTask.
type pendingFiringsStore struct{ q queryer }

func newPendingFiringsStore(q queryer) db.PendingFiringsStore {
	return &pendingFiringsStore{q: q}
}

var _ db.PendingFiringsStore = (*pendingFiringsStore)(nil)

// Enqueue inserts the firing and stamps the task's agent claim in one
// transaction — see db.AgentClaimStamp for why the two writes are
// inseparable. A stamp refusal is not an error and leaves the firing
// committed.
func (s *pendingFiringsStore) Enqueue(ctx context.Context, orgID, userID, entityID, taskID, triggerID, triggeringEventID string, claim db.AgentClaimStamp) (bool, bool, error) {
	// creator_user_id is NOT NULL in the Postgres schema. Resolution
	// mirrors blueprintStore.createRunManual: prefer the caller-supplied
	// userID, fall back to the org owner. tf.current_user_id() is
	// intentionally skipped — admin-pool inserts run without JWT
	// claims, so the helper would return NULL and the COALESCE would
	// walk straight to org owner anyway.
	//
	// LocalDefaultUserID sentinel handling: the router still passes
	// runmode.LocalDefaultUserID until D9 retrofits handler-
	// level claims. That sentinel UUID has no FK target in a multi-
	// mode users table, so binding it directly would trip
	// pending_firings_creator_user_id_fkey on every busy-entity
	// enqueue. Normalize to empty here so NULLIF collapses to NULL
	// and COALESCE walks to the org-owner fallback. Same shape as
	// blueprintStore.createRunManual.
	//
	// queued_at uses the schema default (now()) so the insert and
	// the index agree on the timestamp source — no clock skew between
	// app-side time.Now() and the partial index's FIFO ordering.
	creatorBind := userID
	if creatorBind == runmode.LocalDefaultUserID {
		creatorBind = ""
	}
	inserted, claimed := false, false
	err := inTx(ctx, s.q, func(q queryer) error {
		// The dedup target includes 'draining': a firing mid-drain is still
		// queued intent for (task, trigger), and a duplicate enqueued during
		// the drain window would fire a second blueprint run for the same intent
		// as soon as the first one's conversation terminates. The predicate must
		// stay textually equivalent to idx_pending_firings_dedup for conflict
		// inference.
		res, err := q.ExecContext(ctx, `
			INSERT INTO pending_firings
			  (org_id, creator_user_id, entity_id, task_id, trigger_id, triggering_event_id, status)
			VALUES
			  ($1,
			   COALESCE(NULLIF($2, '')::uuid, (SELECT owner_user_id FROM orgs WHERE id = $1)),
			   $3, $4, $5, $6, 'pending')
			ON CONFLICT (task_id, trigger_id) WHERE status IN ('pending', 'draining') DO NOTHING
		`, orgID, creatorBind, entityID, taskID, triggerID, triggeringEventID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		inserted = n > 0
		// Nothing was committed on the collapse path — the duplicate already
		// queued carries the commitment, and its own enqueue stamped the claim.
		if !inserted || claim.AgentID == "" {
			return nil
		}
		claimed, err = stampAgentClaimIfUnclaimed(ctx, q, orgID, taskID, claim.AgentID, claim.ActingTeamID)
		return err
	})
	if err != nil {
		return false, false, err
	}
	return inserted, claimed, nil
}

// PopForTask is a claiming pop: the subquery locks the oldest pending row
// under FOR UPDATE SKIP LOCKED (so a concurrent claimant skips it rather
// than blocking on it) and the outer UPDATE flips it to 'draining' in the
// same statement — there is no window between "read the candidate" and
// "claim it" for a second drain to observe. Two concurrent PopForTask
// calls against the same task therefore never return the same row: the
// loser's subquery either skips the now-locked row and returns the next
// candidate, or finds none.
func (s *pendingFiringsStore) PopForTask(ctx context.Context, orgID, taskID string) (*domain.PendingFiring, error) {
	if !isValidUUID(taskID) {
		return nil, nil
	}
	row := s.q.QueryRowContext(ctx, `
		UPDATE pending_firings
		SET status = 'draining', claimed_at = now()
		WHERE id = (
			SELECT id FROM pending_firings
			WHERE org_id = $1 AND task_id = $2 AND status = 'pending'
			ORDER BY queued_at ASC, id ASC
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, entity_id, task_id, trigger_id, triggering_event_id,
		          status, COALESCE(skip_reason, ''), queued_at, drained_at, fired_run_id
	`, orgID, taskID)
	return scanPgPendingFiring(row)
}

func (s *pendingFiringsStore) Release(ctx context.Context, orgID string, firingID int64) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'pending', claimed_at = NULL
		WHERE org_id = $1 AND id = $2 AND status = 'draining'
	`, orgID, firingID)
	return err
}

// RequeueStaleDraining is the claiming pop's crash recovery — see the
// interface doc. NULL claimed_at 'draining' rows (claimed before the
// column existed) are recovered unconditionally.
func (s *pendingFiringsStore) RequeueStaleDraining(ctx context.Context, orgID string, before time.Time) (int, error) {
	res, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'pending', claimed_at = NULL
		WHERE org_id = $1 AND status = 'draining'
		  AND (claimed_at IS NULL OR claimed_at < $2)
	`, orgID, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *pendingFiringsStore) MarkFired(ctx context.Context, orgID string, firingID int64, blueprintRunID string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'fired', drained_at = now(), fired_run_id = $1
		WHERE org_id = $2 AND id = $3 AND status = 'draining'
	`, blueprintRunID, orgID, firingID)
	return err
}

func (s *pendingFiringsStore) MarkSkipped(ctx context.Context, orgID string, firingID int64, reason string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE pending_firings
		SET status = 'skipped_stale', drained_at = now(), skip_reason = $1
		WHERE org_id = $2 AND id = $3 AND status = 'draining'
	`, reason, orgID, firingID)
	return err
}

func (s *pendingFiringsStore) HasPendingForTask(ctx context.Context, orgID, taskID string) (bool, error) {
	if !isValidUUID(taskID) {
		return false, nil
	}
	// 'draining' counts as queued intent (see the interface doc): a drain
	// mid-flight must keep the task gate closed or a fresh event in
	// that window jumps the queue.
	var count int
	err := s.q.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pending_firings
		WHERE org_id = $1 AND task_id = $2 AND status IN ('pending', 'draining')
	`, orgID, taskID).Scan(&count)
	return count > 0, err
}

func (s *pendingFiringsStore) ListTasksWithPending(ctx context.Context, orgID string) ([]string, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT DISTINCT task_id FROM pending_firings
		WHERE org_id = $1 AND status = 'pending'
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *pendingFiringsStore) ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.PendingFiring, error) {
	rows, err := s.q.QueryContext(ctx, `
		SELECT id, entity_id, task_id, trigger_id, triggering_event_id,
		       status, COALESCE(skip_reason, ''), queued_at, drained_at, fired_run_id
		FROM pending_firings
		WHERE org_id = $1 AND entity_id = $2
		ORDER BY queued_at ASC, id ASC
	`, orgID, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PendingFiring{}
	for rows.Next() {
		f, err := scanPgPendingFiringRow(rows)
		if err != nil {
			return nil, err
		}
		if f != nil {
			out = append(out, *f)
		}
	}
	return out, rows.Err()
}

// scanPgPendingFiring scans a sql.Row into *domain.PendingFiring.
// (nil, nil) on sql.ErrNoRows so callers can treat "no pending" as a
// non-error empty result. Mirrors the SQLite-side helper.
func scanPgPendingFiring(row *sql.Row) (*domain.PendingFiring, error) {
	var (
		f                   domain.PendingFiring
		drainedAt           sql.NullTime
		firedBlueprintRunID sql.NullString
	)
	err := row.Scan(
		&f.ID, &f.EntityID, &f.TaskID, &f.TriggerID, &f.TriggeringEventID,
		&f.Status, &f.SkipReason, &f.QueuedAt, &drainedAt, &firedBlueprintRunID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if drainedAt.Valid {
		t := drainedAt.Time
		f.DrainedAt = &t
	}
	if firedBlueprintRunID.Valid {
		s := firedBlueprintRunID.String
		f.FiredBlueprintRunID = &s
	}
	return &f, nil
}

// scanPgPendingFiringRow is the sql.Rows variant.
func scanPgPendingFiringRow(rows *sql.Rows) (*domain.PendingFiring, error) {
	var (
		f                   domain.PendingFiring
		drainedAt           sql.NullTime
		firedBlueprintRunID sql.NullString
	)
	err := rows.Scan(
		&f.ID, &f.EntityID, &f.TaskID, &f.TriggerID, &f.TriggeringEventID,
		&f.Status, &f.SkipReason, &f.QueuedAt, &drainedAt, &firedBlueprintRunID,
	)
	if err != nil {
		return nil, err
	}
	if drainedAt.Valid {
		t := drainedAt.Time
		f.DrainedAt = &t
	}
	if firedBlueprintRunID.Valid {
		s := firedBlueprintRunID.String
		f.FiredBlueprintRunID = &s
	}
	return &f, nil
}
