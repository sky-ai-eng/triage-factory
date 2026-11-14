package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
	"github.com/sky-ai-eng/triage-factory/internal/db/workkinds"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pendingFiringsStore is the Postgres impl of db.PendingFiringsStore — the
// router's per-task auto-delegation queue, on the shared work-item contract.
// Wired against the admin pool in postgres.New: the router and the firing
// worker are system services with no per-user identity, so impersonating a
// user via the app pool would be wrong. The pending_firings_all RLS policy
// is defense-in-depth (admin bypasses it) and org_id is bound in every
// statement.
//
// q is the admission and read side, the caller's transaction when the store
// is transaction-bound. conn is the pool the package's own transactions open
// on, nil on a transaction-bound store: the verbs that need it answer
// db.ErrTxBoundStore there rather than nesting a transaction the caller
// cannot see.
type pendingFiringsStore struct {
	q    queryer
	conn *sql.DB
	kind workitem.Kind
}

func newPendingFiringsStore(q queryer, conn *sql.DB) db.PendingFiringsStore {
	return &pendingFiringsStore{q: q, conn: conn, kind: workkinds.PendingFirings(workitem.Postgres)}
}

var _ db.PendingFiringsStore = (*pendingFiringsStore)(nil)

// unsettledFiringStatuses is the predicate for a firing that still holds its
// key and keeps the task's gate closed. Parked is in it: a parked firing is
// redriven or cancelled by an operator, never replaced by a fresh row.
const unsettledFiringStatuses = "'ready','leased','parked'"

// Enqueue admits the firing through the package and stamps the task's agent
// claim in one transaction — see db.AgentClaimStamp for why the two writes
// are inseparable. A stamp refusal is not an error and leaves the firing
// committed.
func (s *pendingFiringsStore) Enqueue(ctx context.Context, orgID, entityID, taskID, triggerID, triggeringEventID string, claim db.AgentClaimStamp) (bool, bool, error) {
	return s.admitAndStamp(ctx, orgID, workkinds.PendingFiringKey(taskID, triggerID),
		db.PendingFiringRowCols(entityID, taskID, triggerID, triggeringEventID), taskID, claim)
}

// EnqueueHanded admits the held firing with no unique key, so the package
// inserts it outright, and stamps the claim in the same transaction as
// Enqueue does.
func (s *pendingFiringsStore) EnqueueHanded(ctx context.Context, orgID, entityID, taskID, triggerID, triggeringEventID, noteID string, claim db.AgentClaimStamp) (bool, error) {
	if noteID == "" {
		return false, errHandedFiringNeedsNote
	}
	cols := db.PendingFiringRowCols(entityID, taskID, triggerID, triggeringEventID)
	cols["handed_note_id"] = noteID
	_, claimed, err := s.admitAndStamp(ctx, orgID, "", cols, taskID, claim)
	return claimed, err
}

// admitAndStamp admits one firing under uniqueKey ("" for none) and stamps
// the task's agent claim in the same transaction, pending_firings before
// tasks. The stamp is skipped when the admission collapsed onto an unsettled
// row: that row already carries the commitment, and its own admission
// stamped the claim.
func (s *pendingFiringsStore) admitAndStamp(ctx context.Context, orgID, uniqueKey string, cols map[string]any, taskID string, claim db.AgentClaimStamp) (inserted, claimed bool, err error) {
	err = inTx(ctx, s.q, func(q queryer) error {
		_, deduplicated, err := workitem.Admit(ctx, q, s.kind, orgID, uniqueKey, cols)
		if err != nil {
			return err
		}
		inserted = !deduplicated
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

// errHandedFiringNeedsNote refuses a held firing that names no note.
var errHandedFiringNeedsNote = errors.New("a held firing must name the note it is held behind")

// pgPendingFiringSelect is the projection every PendingFiring read answers
// with: the shared block and the kind's own columns.
const pgPendingFiringSelect = `
	SELECT id, org_id,
	       entity_id, task_id, trigger_id, triggering_event_id,
	       COALESCE(skip_reason, ''), fired_run_id::text, COALESCE(handed_note_id, ''),
	       status, attempt, max_attempts, next_attempt_at,
	       lease_generation, COALESCE(lease_owner, ''), lease_epoch, leased_at, lease_expires_at,
	       cancel_requested_at, COALESCE(cancel_requested_by, ''), COALESCE(cancel_reason, ''),
	       COALESCE(last_error, ''), COALESCE(last_outcome, ''), COALESCE(unique_key, ''), superseded_by,
	       first_enqueued_at, created_at, done_at
	FROM public.pending_firings`

func (s *pendingFiringsStore) Claim(ctx context.Context, owner workitem.Owner, n int) (db.FiringClaim, error) {
	if s.conn == nil {
		return db.FiringClaim{}, db.ErrTxBoundStore
	}
	res, claimErr := workitem.Claim(ctx, s.conn, s.kind, owner, "", n)
	out := db.FiringClaim{Cancelled: res.Cancelled, Parked: res.Parked, Reclaimed: res.Reclaimed}
	if len(res.Claimed) == 0 {
		return out, claimErr
	}
	// The identity columns are immutable after admission, so reading them
	// by id after the claim reads the values the row was admitted with.
	ids := make([]int64, len(res.Claimed))
	for i, r := range res.Claimed {
		ids[i] = r.ItemID
	}
	rows, err := s.conn.QueryContext(ctx, pgPendingFiringSelect+`
		WHERE id = ANY($1)`, ids)
	if err != nil {
		return out, errors.Join(claimErr, err)
	}
	defer rows.Close()
	byID := make(map[int64]domain.PendingFiring, len(res.Claimed))
	for rows.Next() {
		f, err := scanPgPendingFiring(rows)
		if err != nil {
			return out, errors.Join(claimErr, err)
		}
		byID[f.ID] = f
	}
	if err := rows.Err(); err != nil {
		return out, errors.Join(claimErr, err)
	}
	out.Firings = make([]db.ClaimedFiring, 0, len(res.Claimed))
	for _, r := range res.Claimed {
		f, ok := byID[r.ItemID]
		if !ok {
			return out, errors.Join(claimErr, fmt.Errorf("pending_firings row %d leased but not readable", r.ItemID))
		}
		out.Firings = append(out.Firings, db.ClaimedFiring{Receipt: r, Firing: f})
	}
	return out, claimErr
}

func (s *pendingFiringsStore) RenewLease(ctx context.Context, r workitem.Receipt) (workitem.Receipt, error) {
	if s.conn == nil {
		return workitem.Receipt{}, db.ErrTxBoundStore
	}
	return workitem.RenewLease(ctx, s.conn, s.kind, r)
}

// MarkFired and MarkSkipped each run one transaction: the package's terminal
// flip first, then the kind's own column. The second statement needs no
// guard of its own — the first proved this holder's authority inside the
// same transaction, and its row lock holds until commit, so nothing can move
// the row in between.
func (s *pendingFiringsStore) MarkFired(ctx context.Context, r workitem.Receipt, blueprintRunID string) error {
	return s.markDoneWith(ctx, r, "fired_run_id", blueprintRunID)
}

func (s *pendingFiringsStore) MarkSkipped(ctx context.Context, r workitem.Receipt, reason string) error {
	return s.markDoneWith(ctx, r, "skip_reason", reason)
}

func (s *pendingFiringsStore) markDoneWith(ctx context.Context, r workitem.Receipt, column, value string) error {
	if s.conn == nil {
		return db.ErrTxBoundStore
	}
	return db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
		if err := workitem.MarkDone(ctx, tx, s.kind, r); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE public.pending_firings SET `+column+` = $1 WHERE id = $2 AND org_id = $3`, value, r.ItemID, r.OrgID)
		return err
	})
}

func (s *pendingFiringsStore) Requeue(ctx context.Context, r workitem.Receipt, outcome workitem.Outcome, cause error) (bool, error) {
	if s.conn == nil {
		return false, db.ErrTxBoundStore
	}
	return workitem.Requeue(ctx, s.conn, s.kind, r, outcome, cause)
}

// DeferWhileTaskBusy defers under the condition the claim filter negates,
// rendered from the same text and read on the row inside the deferral's own
// transaction, so the refund is granted only while the task really is busy
// and the row it returns to ready is one the filter holds.
func (s *pendingFiringsStore) DeferWhileTaskBusy(ctx context.Context, r workitem.Receipt) error {
	if s.conn == nil {
		return db.ErrTxBoundStore
	}
	return workitem.Defer(ctx, s.conn, s.kind, r, workkinds.PendingFiringDeferTaskBusy, time.Now().UTC(), func(tx *sql.Tx) (bool, error) {
		var busy bool
		err := tx.QueryRowContext(ctx, `SELECT `+workkinds.PendingFiringsTaskBusy(workitem.Postgres)+`
			FROM public.pending_firings t WHERE t.id = $1 AND t.org_id = $2`, r.ItemID, r.OrgID).Scan(&busy)
		return busy, err
	})
}

func (s *pendingFiringsStore) HasUnsettledForTask(ctx context.Context, orgID, taskID string) (bool, error) {
	if !isValidUUID(taskID) {
		return false, nil
	}
	var one int
	err := s.q.QueryRowContext(ctx, `
		SELECT 1 FROM public.pending_firings
		WHERE org_id = $1 AND task_id = $2 AND status IN (`+unsettledFiringStatuses+`)
		LIMIT 1
	`, orgID, taskID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *pendingFiringsStore) ListReadyForTask(ctx context.Context, orgID, taskID string) ([]domain.PendingFiring, error) {
	if !isValidUUID(taskID) {
		return []domain.PendingFiring{}, nil
	}
	rows, err := s.q.QueryContext(ctx, pgPendingFiringSelect+`
		WHERE org_id = $1 AND task_id = $2 AND status = 'ready'
		ORDER BY id
	`, orgID, taskID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PendingFiring{}
	for rows.Next() {
		f, err := scanPgPendingFiring(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// errFiringNotHandable rolls a hand-off back when its row cannot take one,
// so a note staged in the same transaction does not outlive the refusal.
var errFiringNotHandable = errors.New("firing is not ready for a hand-off")

func (s *pendingFiringsStore) HandOffToConversation(ctx context.Context, orgID string, firingID int64, note *domain.StagedInjection, sharedNoteID string) (string, bool, error) {
	if s.conn == nil {
		return "", false, db.ErrTxBoundStore
	}
	noteID := sharedNoteID
	err := db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
		if note != nil {
			stored, err := newStagedInjectionStore(tx).AppendSystem(ctx, orgID, *note)
			if err != nil {
				return err
			}
			noteID = stored.ID
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE public.pending_firings SET handed_note_id = $1, unique_key = NULL
			WHERE id = $2 AND org_id = $3 AND status = 'ready'
			  AND handed_note_id IS NULL AND cancel_requested_at IS NULL
		`, noteID, firingID, orgID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errFiringNotHandable
		}
		return nil
	})
	if errors.Is(err, errFiringNotHandable) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return noteID, true, nil
}

// errHandOffNotReleasable rolls a release back when either of its writes
// matches no row, so a withdrawn note never outlives a firing that still
// names it.
var errHandOffNotReleasable = errors.New("hand-off cannot be released")

func (s *pendingFiringsStore) ReleaseHandOff(ctx context.Context, orgID string, firingID int64, noteID, conversationID string) (bool, error) {
	if s.conn == nil {
		return false, db.ErrTxBoundStore
	}
	noteRowID, err := strconv.ParseInt(noteID, 10, 64)
	if err != nil {
		// A note id that is not a messages row id names no note to withdraw.
		return false, nil
	}
	// matched answers errHandOffNotReleasable for a write that matched no
	// row, which rolls the release back.
	matched := func(res sql.Result, err error) error {
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errHandOffNotReleasable
		}
		return nil
	}
	err = db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
		// Withdrawn as DeleteSystem withdraws: retired in place, delivered
		// left false. A row already retired still matches, which is what
		// releases every firing sharing the note.
		res, err := tx.ExecContext(ctx, `
			UPDATE messages SET window_state = 'inactive'
			WHERE org_id = $1::uuid AND id = $2 AND delivered = false
			  AND conversation_id <> $3::uuid AND subtype = ANY($4::text[])
		`, orgID, noteRowID, conversationID, domain.StagedInjectionSubtypes)
		if err := matched(res, err); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `
			UPDATE public.pending_firings SET handed_note_id = NULL
			WHERE id = $1 AND org_id = $2 AND status = 'ready'
			  AND handed_note_id = $3 AND cancel_requested_at IS NULL
		`, firingID, orgID, noteID)
		return matched(res, err)
	})
	if errors.Is(err, errHandOffNotReleasable) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *pendingFiringsStore) WithdrawHandedNote(ctx context.Context, orgID, noteID string) (bool, error) {
	return newStagedInjectionStore(s.q).DeleteSystem(ctx, orgID, noteID)
}

func (s *pendingFiringsStore) RequestCancelForTask(ctx context.Context, orgID, taskID, by, reason string) (int, error) {
	if !isValidUUID(taskID) {
		return 0, nil
	}
	rows, err := s.q.QueryContext(ctx, `
		SELECT id FROM public.pending_firings
		WHERE org_id = $1 AND task_id = $2
		  AND status IN ('ready','leased') AND cancel_requested_at IS NULL
		ORDER BY id
	`, orgID, taskID)
	if err != nil {
		return 0, err
	}
	// The ids are read out before the first write: a transaction's one
	// connection cannot write while a result set is open on it.
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return workitem.RequestCancelEach(ctx, s.q, s.kind, orgID, ids, by, reason)
}

func (s *pendingFiringsStore) TaskRerouted(ctx context.Context, orgID, taskID string) (bool, error) {
	if !isValidUUID(taskID) {
		return false, nil
	}
	var one int
	err := s.q.QueryRowContext(ctx, `
		SELECT 1 FROM public.pending_firings
		WHERE org_id = $1 AND task_id = $2 AND status = 'done' AND skip_reason = $3
		LIMIT 1
	`, orgID, taskID, domain.PendingFiringSkipRerouted).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *pendingFiringsStore) ListForEntity(ctx context.Context, orgID, entityID string) ([]domain.PendingFiring, error) {
	if !isValidUUID(entityID) {
		return []domain.PendingFiring{}, nil
	}
	rows, err := s.q.QueryContext(ctx, pgPendingFiringSelect+`
		WHERE org_id = $1 AND entity_id = $2
		ORDER BY id
	`, orgID, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PendingFiring{}
	for rows.Next() {
		f, err := scanPgPendingFiring(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// The store is also the kind's WorkKindHandle: what the operator surface and
// the metrics depth observer see of this table. As the event queue's: the
// surface runs the package's own reads and controls on Conn, org-scoped by
// argument on the admin pool, so the org-admin predicate in the handler is
// the authorization, not RLS. A transaction-bound store answers these too,
// on its transaction.
var _ db.WorkKindHandle = (*pendingFiringsStore)(nil)

func (s *pendingFiringsStore) Name() string        { return workkinds.PendingFiringsName }
func (s *pendingFiringsStore) Label() string       { return workkinds.PendingFiringsLabel }
func (s *pendingFiringsStore) Kind() workitem.Kind { return s.kind }
func (s *pendingFiringsStore) Conn() workitem.DBTX {
	if s.conn != nil {
		return s.conn
	}
	return s.q
}
func (s *pendingFiringsStore) Access() db.WorkAccess { return db.WorkAccessOrgAdmin }

// Controls: redrive and cancel, never supersede. A supersede records a
// replacement row, and a parked firing has none: a redrive fires it once,
// from its own triggering event, or the worker's validations decline it.
func (s *pendingFiringsStore) Controls() db.WorkControls {
	return db.WorkControls{Redrive: true, Cancel: true}
}

func (s *pendingFiringsStore) Objective() db.WorkObjective {
	return db.WorkObjective{OldestReadyAge: workkinds.PendingFiringsOldestReadyObjective}
}

// Describe names each row by the entity its firing is about and the handler
// that would fire. LEFT JOINs, because a parked row must be described
// whether or not its entity or handler is still there. The joins are bound
// on org_id as well as id: the composite FKs mean the pair is what
// identifies each, and binding only id would let a cross-org id collision
// join the wrong title in.
func (s *pendingFiringsStore) Describe(ctx context.Context, orgID string, ids []int64) (map[int64]db.WorkSubject, error) {
	if len(ids) == 0 {
		return map[int64]db.WorkSubject{}, nil
	}
	rows, err := s.Conn().QueryContext(ctx, `
		SELECT f.id, f.entity_id::text, f.task_id::text, f.trigger_id::text, f.triggering_event_id::text,
		       COALESCE(f.skip_reason, ''), COALESCE(f.fired_run_id::text, ''),
		       COALESCE(e.source_id, ''), COALESCE(e.title, ''),
		       (h.id IS NOT NULL), COALESCE(h.name, '')
		FROM public.pending_firings f
		LEFT JOIN public.entities e ON e.id = f.entity_id AND e.org_id = f.org_id
		LEFT JOIN public.event_handlers h ON h.id = f.trigger_id AND h.org_id = f.org_id
		WHERE f.org_id = $1 AND f.id = ANY($2)
	`, orgID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]db.WorkSubject, len(ids))
	for rows.Next() {
		var (
			f                                  domain.PendingFiring
			firedRunID, sourceID, title, tname string
			found                              bool
		)
		if err := rows.Scan(&f.ID, &f.EntityID, &f.TaskID, &f.TriggerID, &f.TriggeringEventID,
			&f.SkipReason, &firedRunID, &sourceID, &title, &found, &tname); err != nil {
			return nil, err
		}
		if firedRunID != "" {
			f.FiredBlueprintRunID = &firedRunID
		}
		out[f.ID] = db.PendingFiringSubject(f, sourceID, title, tname, found)
	}
	return out, rows.Err()
}

// scanPgPendingFiring reads one pgPendingFiringSelect row.
func scanPgPendingFiring(row interface{ Scan(...any) error }) (domain.PendingFiring, error) {
	var (
		f                                     domain.PendingFiring
		firedRunID                            sql.NullString
		nextAt, leasedAt, expiresAt, cancelAt sql.NullTime
		doneAt                                sql.NullTime
		leaseEpoch, supersededBy              sql.NullInt64
	)
	err := row.Scan(
		&f.ID, &f.OrgID,
		&f.EntityID, &f.TaskID, &f.TriggerID, &f.TriggeringEventID,
		&f.SkipReason, &firedRunID, &f.HandedNoteID,
		&f.Status, &f.Attempt, &f.MaxAttempts, &nextAt,
		&f.LeaseGeneration, &f.LeaseOwner, &leaseEpoch, &leasedAt, &expiresAt,
		&cancelAt, &f.CancelRequestedBy, &f.CancelReason,
		&f.LastError, &f.LastOutcome, &f.UniqueKey, &supersededBy,
		&f.FirstEnqueuedAt, &f.CreatedAt, &doneAt,
	)
	if err != nil {
		return domain.PendingFiring{}, err
	}
	if firedRunID.Valid {
		v := firedRunID.String
		f.FiredBlueprintRunID = &v
	}
	f.FirstEnqueuedAt = f.FirstEnqueuedAt.UTC()
	f.CreatedAt = f.CreatedAt.UTC()
	f.NextAttemptAt = nullTimePtr(nextAt)
	f.LeasedAt = nullTimePtr(leasedAt)
	f.LeaseExpiresAt = nullTimePtr(expiresAt)
	f.CancelRequestedAt = nullTimePtr(cancelAt)
	f.DoneAt = nullTimePtr(doneAt)
	f.LeaseEpoch = queueNullInt64(leaseEpoch)
	f.SupersededBy = queueNullInt64(supersededBy)
	return f, nil
}
