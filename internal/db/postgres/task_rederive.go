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
)

// taskReDeriveStore is the Postgres impl of db.TaskReDeriveStore — the
// post-scoring re-evaluation queue, on the shared work-item contract. Wired
// against the admin pool in postgres.New: the re-derive worker is a system
// service with no per-user identity, so impersonating a user via the app
// pool would be wrong. The task_rederive_queue_all RLS policy is
// defense-in-depth (admin bypasses it) and org_id is bound in every
// statement.
//
// Admission is not here: UpdateTaskScores admits and raises the row inside
// the score transaction (scores.go), which is what makes the obligation
// inseparable from the scores it is owed for.
type taskReDeriveStore struct {
	conn *sql.DB
	kind workitem.Kind
}

func newTaskReDeriveStore(conn *sql.DB) db.TaskReDeriveStore {
	return &taskReDeriveStore{conn: conn, kind: workkinds.TaskReDerive(workitem.Postgres)}
}

var _ db.TaskReDeriveStore = (*taskReDeriveStore)(nil)

func (s *taskReDeriveStore) Claim(ctx context.Context, owner workitem.Owner, n int) (db.ReDeriveClaim, error) {
	res, claimErr := workitem.Claim(ctx, s.conn, s.kind, owner, "", n)
	out := db.ReDeriveClaim{Cancelled: res.Cancelled, Parked: res.Parked, Reclaimed: res.Reclaimed}
	if len(res.Claimed) == 0 {
		return out, claimErr
	}
	// task_id is immutable after admission, so reading it by id after the
	// claim reads the value the row was admitted with. requested_revision
	// is the receipt's: the row's may already have moved on.
	ids := make([]int64, len(res.Claimed))
	for i, r := range res.Claimed {
		ids[i] = r.ItemID
	}
	rows, err := s.conn.QueryContext(ctx, `SELECT id, task_id::text FROM public.task_rederive_queue WHERE id = ANY($1)`, ids)
	if err != nil {
		return out, errors.Join(claimErr, err)
	}
	defer rows.Close()
	taskByID := make(map[int64]string, len(res.Claimed))
	for rows.Next() {
		var id int64
		var taskID string
		if err := rows.Scan(&id, &taskID); err != nil {
			return out, errors.Join(claimErr, err)
		}
		taskByID[id] = taskID
	}
	if err := rows.Err(); err != nil {
		return out, errors.Join(claimErr, err)
	}
	out.Items = make([]db.ClaimedReDerive, 0, len(res.Claimed))
	for _, r := range res.Claimed {
		taskID, ok := taskByID[r.ItemID]
		if !ok {
			return out, errors.Join(claimErr, fmt.Errorf("task_rederive_queue row %d leased but not readable", r.ItemID))
		}
		rev, err := db.FrozenRequestedRevision(r)
		if err != nil {
			return out, errors.Join(claimErr, err)
		}
		out.Items = append(out.Items, db.ClaimedReDerive{Receipt: r, TaskID: taskID, RequestedRevision: rev})
	}
	return out, claimErr
}

func (s *taskReDeriveStore) RenewLease(ctx context.Context, r workitem.Receipt) (workitem.Receipt, error) {
	return workitem.RenewLease(ctx, s.conn, s.kind, r)
}

// Complete compares the frozen revision with the row's under the lock
// workitem.Complete already holds, and runs the effects against a firings
// store bound to that same transaction. The firings store's Enqueue inserts
// into pending_firings and then stamps the task, which is the one path this
// transaction reaches tasks through — after the queue row, never before it.
func (s *taskReDeriveStore) Complete(ctx context.Context, r workitem.Receipt, effects func(firings db.PendingFiringsStore) error) error {
	frozen, err := db.FrozenRequestedRevision(r)
	if err != nil {
		return err
	}
	if effects == nil {
		return errors.New("postgres: task_rederive_queue complete has no effects closure")
	}
	return workitem.Complete(ctx, s.conn, s.kind, r, func(tx *sql.Tx) error {
		current, err := s.requestedRevision(ctx, tx, r)
		if err != nil {
			return err
		}
		if current > frozen {
			return db.ErrScoreMoved
		}
		return effects(newPendingFiringsStore(tx, nil))
	})
}

// DeferScoreMoved defers under the predicate the completion refused on, read
// on the row inside the deferral's own transaction, so the refund is granted
// only while a newer score really did land.
func (s *taskReDeriveStore) DeferScoreMoved(ctx context.Context, r workitem.Receipt) error {
	frozen, err := db.FrozenRequestedRevision(r)
	if err != nil {
		return err
	}
	return workitem.Defer(ctx, s.conn, s.kind, r, workkinds.TaskReDeriveDeferScoreMoved, time.Now().UTC(), func(tx *sql.Tx) (bool, error) {
		current, err := s.requestedRevision(ctx, tx, r)
		if err != nil {
			return false, err
		}
		return current > frozen, nil
	})
}

func (s *taskReDeriveStore) Requeue(ctx context.Context, r workitem.Receipt, outcome workitem.Outcome, cause error) (bool, error) {
	return workitem.Requeue(ctx, s.conn, s.kind, r, outcome, cause)
}

// requestedRevision reads the row's current requested_revision inside a
// transaction that already holds the row's lock.
func (s *taskReDeriveStore) requestedRevision(ctx context.Context, tx *sql.Tx, r workitem.Receipt) (int64, error) {
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT requested_revision FROM public.task_rederive_queue WHERE id = $1 AND org_id = $2`, r.ItemID, r.OrgID).Scan(&current); err != nil {
		return 0, fmt.Errorf("read requested_revision of task_rederive_queue row %d: %w", r.ItemID, err)
	}
	return current, nil
}

// The store is also the kind's WorkKindHandle: what the operator surface and
// the metrics depth observer see of this table. As the firing queue's: the
// surface runs the package's own reads and controls on Conn, org-scoped by
// argument on the admin pool, so the org-admin predicate in the handler is
// the authorization, not RLS.
var _ db.WorkKindHandle = (*taskReDeriveStore)(nil)

func (s *taskReDeriveStore) Name() string          { return workkinds.TaskReDeriveName }
func (s *taskReDeriveStore) Label() string         { return workkinds.TaskReDeriveLabel }
func (s *taskReDeriveStore) Kind() workitem.Kind   { return s.kind }
func (s *taskReDeriveStore) Conn() workitem.DBTX   { return s.conn }
func (s *taskReDeriveStore) Access() db.WorkAccess { return db.WorkAccessOrgAdmin }

// Controls: redrive and cancel, never supersede. A supersede records a
// replacement row, and a parked re-evaluation has none: a redrive evaluates
// the task once more, against whatever revision the row carries by then.
func (s *taskReDeriveStore) Controls() db.WorkControls {
	return db.WorkControls{Redrive: true, Cancel: true}
}

func (s *taskReDeriveStore) Objective() db.WorkObjective {
	return db.WorkObjective{OldestReadyAge: workkinds.TaskReDeriveOldestReadyObjective}
}

// Describe names each row by the entity its task is about. The task join is
// inner — the task's foreign key cascades, so a row without one does not
// exist — and the entity join is LEFT, because a parked row must be described
// whether or not its entity is still there. Both joins bind org_id beside the
// id, since the composite keys are what identify each.
func (s *taskReDeriveStore) Describe(ctx context.Context, orgID string, ids []int64) (map[int64]db.WorkSubject, error) {
	if len(ids) == 0 {
		return map[int64]db.WorkSubject{}, nil
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT q.id, q.task_id::text, q.requested_revision,
		       COALESCE(t.event_type, ''), COALESCE(e.source_id, ''), COALESCE(e.title, '')
		FROM public.task_rederive_queue q
		JOIN public.tasks t ON t.id = q.task_id AND t.org_id = q.org_id
		LEFT JOIN public.entities e ON e.id = t.entity_id AND e.org_id = t.org_id
		WHERE q.org_id = $1 AND q.id = ANY($2)
	`, orgID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]db.WorkSubject, len(ids))
	for rows.Next() {
		var (
			id                              int64
			rev                             int64
			taskID, eventType, source, name string
		)
		if err := rows.Scan(&id, &taskID, &rev, &eventType, &source, &name); err != nil {
			return nil, err
		}
		out[id] = db.TaskReDeriveSubject(taskID, rev, eventType, source, name)
	}
	return out, rows.Err()
}
