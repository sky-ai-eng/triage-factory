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

// prMergeabilityStore is the Postgres impl of db.PRMergeabilityStore. Wired
// against the admin pool in postgres.New: the poll and the probe worker are
// system services with no per-user identity, so org_id is bound in every
// statement and the tables' RLS policies are defense-in-depth.
//
// Lock order: a completion locks its probe row first (workitem's lock), then
// the pull request's mergeability row, then writes events and event_queue.
// An admission touches pr_mergeability_probes alone, so nothing takes these
// in another order.
type prMergeabilityStore struct {
	conn  *sql.DB
	kind  workitem.Kind
	queue workitem.Kind
}

func newPRMergeabilityStore(conn *sql.DB) db.PRMergeabilityStore {
	return &prMergeabilityStore{
		conn:  conn,
		kind:  workkinds.PRMergeabilityProbes(workitem.Postgres),
		queue: workkinds.EventQueue(workitem.Postgres),
	}
}

var _ db.PRMergeabilityStore = (*prMergeabilityStore)(nil)

const pgPRMergeabilityColumns = `entity_id::text, org_id::text, COALESCE(mergeable, ''), COALESCE(mergeable_head_sha, ''), checked_at`

func scanPgPRMergeability(row interface{ Scan(...any) error }) (domain.PRMergeability, error) {
	var m domain.PRMergeability
	var checked sql.NullTime
	if err := row.Scan(&m.EntityID, &m.OrgID, &m.Mergeable, &m.MergeableHeadSHA, &checked); err != nil {
		return domain.PRMergeability{}, err
	}
	if checked.Valid {
		t := checked.Time.UTC()
		m.CheckedAt = &t
	}
	return m, nil
}

func (s *prMergeabilityStore) GetSystem(ctx context.Context, orgID, entityID string) (*domain.PRMergeability, error) {
	m, err := scanPgPRMergeability(s.conn.QueryRowContext(ctx, `
		SELECT `+pgPRMergeabilityColumns+` FROM public.pull_request_mergeability
		WHERE org_id = $1 AND entity_id = $2
	`, orgID, entityID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *prMergeabilityStore) ListSystem(ctx context.Context, orgID string) ([]domain.PRMergeability, error) {
	rows, err := s.conn.QueryContext(ctx, `
		SELECT `+pgPRMergeabilityColumns+` FROM public.pull_request_mergeability
		WHERE org_id = $1 ORDER BY entity_id
	`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PRMergeability
	for rows.Next() {
		m, err := scanPgPRMergeability(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *prMergeabilityStore) DeleteInactiveSystem(ctx context.Context, orgID string) (int, error) {
	res, err := s.conn.ExecContext(ctx, `
		DELETE FROM public.pull_request_mergeability m
		WHERE m.org_id = $1
		  AND NOT EXISTS (
			SELECT 1 FROM public.entities e
			WHERE e.org_id = m.org_id AND e.id = m.entity_id AND e.state = 'active'
		  )
	`, orgID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// probeAfter is the instant a probe admitted now with delay becomes ripe.
func probeAfter(delay time.Duration) time.Time {
	return time.Now().UTC().Add(delay)
}

func (s *prMergeabilityStore) AdmitSystem(ctx context.Context, orgID string, probes []db.PRMergeabilityProbe) (int, error) {
	if err := db.ValidatePRMergeabilityProbes(probes); err != nil {
		return 0, err
	}
	if len(probes) == 0 {
		return 0, nil
	}
	admitted := 0
	err := inTxRaw(ctx, s.conn, func(tx *sql.Tx) error {
		admitted = 0
		after := probeAfter(workkinds.PRMergeabilityProbeDelay)
		for _, p := range probes {
			_, deduplicated, err := workitem.Admit(ctx, tx, s.kind, orgID, workkinds.PRMergeabilityProbeKey(p.EntityID),
				db.PRMergeabilityProbeCols(p.EntityID, p.Reason, after, 0))
			if err != nil {
				return fmt.Errorf("admit mergeability probe for entity %s: %w", p.EntityID, err)
			}
			if !deduplicated {
				admitted++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return admitted, nil
}

func (s *prMergeabilityStore) Claim(ctx context.Context, owner workitem.Owner, orgID string, n int) (db.PRMergeabilityClaim, error) {
	res, claimErr := workitem.Claim(ctx, s.conn, s.kind, owner, orgID, n)
	out := db.PRMergeabilityClaim{Cancelled: res.Cancelled, Parked: res.Parked, Reclaimed: res.Reclaimed}
	if len(res.Claimed) == 0 {
		return out, claimErr
	}
	// The kind's columns are immutable after admission, so reading them by
	// id after the claim reads the values the row was admitted with. The
	// entity and its repository are read as they are now; the completion
	// reads the entity again under the probe's lock.
	ids := make([]int64, len(res.Claimed))
	for i, r := range res.Claimed {
		ids[i] = r.ItemID
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT p.id, p.entity_id::text, p.reason, p.unknown_reads,
		       COALESCE(e.external_id, ''), COALESCE(e.state = 'active', false),
		       COALESCE(e.repository_id::text, ''), COALESCE(r.owner, ''), COALESCE(r.repo, '')
		FROM public.pr_mergeability_probes p
		LEFT JOIN public.entities e ON e.id = p.entity_id AND e.org_id = p.org_id
		LEFT JOIN public.repositories r ON r.id = e.repository_id AND r.org_id = e.org_id
		WHERE p.org_id = $1 AND p.id = ANY($2)
	`, orgID, ids)
	if err != nil {
		return out, errors.Join(claimErr, err)
	}
	defer rows.Close()
	byID := make(map[int64]db.ClaimedPRMergeabilityProbe, len(res.Claimed))
	for rows.Next() {
		var id int64
		var item db.ClaimedPRMergeabilityProbe
		if err := rows.Scan(&id, &item.EntityID, &item.Reason, &item.UnknownReads,
			&item.NodeID, &item.Active, &item.RepositoryID, &item.Owner, &item.Repo); err != nil {
			return out, errors.Join(claimErr, err)
		}
		byID[id] = item
	}
	if err := rows.Err(); err != nil {
		return out, errors.Join(claimErr, err)
	}
	out.Items = make([]db.ClaimedPRMergeabilityProbe, 0, len(res.Claimed))
	for _, r := range res.Claimed {
		item, ok := byID[r.ItemID]
		if !ok {
			return out, errors.Join(claimErr, fmt.Errorf("pr_mergeability_probes row %d leased but not readable", r.ItemID))
		}
		item.Receipt = r
		out.Items = append(out.Items, item)
	}
	return out, claimErr
}

func (s *prMergeabilityStore) RenewLease(ctx context.Context, r workitem.Receipt) (workitem.Receipt, error) {
	return workitem.RenewLease(ctx, s.conn, s.kind, r)
}

func (s *prMergeabilityStore) CompleteProbe(ctx context.Context, r workitem.Receipt, answer db.PRMergeabilityAnswer, traceparent string) (db.PRMergeabilityProbeResult, error) {
	if err := db.ValidatePRMergeabilityAnswer(answer); err != nil {
		return db.PRMergeabilityProbeResult{}, err
	}
	// The result is filled inside the transaction and handed back only once
	// it commits; a rolled-back attempt returns nothing of it.
	var res db.PRMergeabilityProbeResult
	successor, err := workitem.CompleteAndAdmit(ctx, s.conn, s.kind, r, func(tx *sql.Tx) (map[string]any, error) {
		res = db.PRMergeabilityProbeResult{Outcome: db.PRMergeabilitySkipped}
		var (
			entityID, repositoryID, snapshotJSON string
			unknownReads                         int
			active                               bool
		)
		if err := tx.QueryRowContext(ctx, `
			SELECT p.entity_id::text, p.unknown_reads,
			       COALESCE(e.state = 'active', false), COALESCE(e.repository_id::text, ''), COALESCE(e.snapshot_json::text, '')
			FROM public.pr_mergeability_probes p
			LEFT JOIN public.entities e ON e.id = p.entity_id AND e.org_id = p.org_id
			WHERE p.org_id = $1 AND p.id = $2
		`, r.OrgID, r.ItemID).Scan(&entityID, &unknownReads, &active, &repositoryID, &snapshotJSON); err != nil {
			return nil, fmt.Errorf("read pr_mergeability_probes row %d: %w", r.ItemID, err)
		}
		if !answer.Found || !active {
			return nil, nil
		}
		if answer.Mergeable == domain.MergeableUnknown {
			if unknownReads+1 >= workkinds.PRMergeabilityMaxUnknownReads {
				res.Outcome = db.PRMergeabilityGaveUp
				return nil, nil
			}
			res.Outcome = db.PRMergeabilityRetried
			return db.PRMergeabilityProbeCols(entityID, domain.MergeabilityProbeUnknownRetry,
				probeAfter(workkinds.PRMergeabilityUnknownRetryDelay), unknownReads+1), nil
		}

		var prev sql.NullString
		switch err := tx.QueryRowContext(ctx, `
			SELECT mergeable FROM public.pull_request_mergeability
			WHERE org_id = $1 AND entity_id = $2
			FOR UPDATE
		`, r.OrgID, entityID).Scan(&prev); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("read mergeability of entity %s: %w", entityID, err)
		}
		now := time.Now().UTC()
		row, err := scanPgPRMergeability(tx.QueryRowContext(ctx, `
			INSERT INTO public.pull_request_mergeability (entity_id, org_id, mergeable, mergeable_head_sha, checked_at)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (entity_id) DO UPDATE SET
				mergeable = excluded.mergeable,
				mergeable_head_sha = excluded.mergeable_head_sha,
				checked_at = excluded.checked_at
			RETURNING `+pgPRMergeabilityColumns,
			entityID, r.OrgID, answer.Mergeable, nullIfEmpty(answer.HeadSHA), now))
		if err != nil {
			return nil, fmt.Errorf("write mergeability of entity %s: %w", entityID, err)
		}
		res.Outcome, res.Previous, res.Row = db.PRMergeabilityRecorded, prev.String, &row

		evt, ok, err := db.PRMergeabilityTransition(r.OrgID, entityID, repositoryID, snapshotJSON, prev.String, answer.Mergeable, answer.HeadSHA, now)
		if err != nil || !ok {
			return nil, err
		}
		id, err := recordEvent(ctx, tx, r.OrgID, evt)
		if err != nil {
			return nil, err
		}
		if _, _, err := workitem.Admit(ctx, tx, s.queue, r.OrgID, "", db.EventQueueRowCols(id, evt, traceparent, nil)); err != nil {
			return nil, err
		}
		evt.ID = id
		res.Events = []domain.Event{evt}
		return nil, nil
	})
	if err != nil {
		return db.PRMergeabilityProbeResult{}, err
	}
	res.SuccessorID = successor
	return res, nil
}

func (s *prMergeabilityStore) Requeue(ctx context.Context, r workitem.Receipt, outcome workitem.Outcome, cause error) (bool, error) {
	return workitem.Requeue(ctx, s.conn, s.kind, r, outcome, cause)
}

func (s *prMergeabilityStore) PruneSettled(ctx context.Context, before time.Time) (int, error) {
	res, err := s.conn.ExecContext(ctx, `
		DELETE FROM public.pr_mergeability_probes
		WHERE status IN ('done', 'cancelled') AND done_at < $1
	`, before)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// The store is also the kind's WorkKindHandle, as the other kinds' are: the
// surface runs the package's own reads and controls on Conn, org-scoped by
// argument on the admin pool, so the org-admin predicate in the handler is
// the authorization, not RLS.
var _ db.WorkKindHandle = (*prMergeabilityStore)(nil)

func (s *prMergeabilityStore) Name() string          { return workkinds.PRMergeabilityProbesName }
func (s *prMergeabilityStore) Label() string         { return workkinds.PRMergeabilityProbesLabel }
func (s *prMergeabilityStore) Kind() workitem.Kind   { return s.kind }
func (s *prMergeabilityStore) Conn() workitem.DBTX   { return s.conn }
func (s *prMergeabilityStore) Access() db.WorkAccess { return db.WorkAccessOrgAdmin }

// Controls: redrive and cancel, never supersede. A parked probe has no
// replacement; a redrive reads the pull request once more.
func (s *prMergeabilityStore) Controls() db.WorkControls {
	return db.WorkControls{Redrive: true, Cancel: true}
}

func (s *prMergeabilityStore) Objective() db.WorkObjective {
	return db.WorkObjective{OldestReadyAge: workkinds.PRMergeabilityProbesOldestReadyObjective}
}

// Describe names each probe by its pull request. The entity join is LEFT so a
// row is described whether or not its entity is still there, and binds org_id
// beside the id, since the composite key is what identifies it.
func (s *prMergeabilityStore) Describe(ctx context.Context, orgID string, ids []int64) (map[int64]db.WorkSubject, error) {
	if len(ids) == 0 {
		return map[int64]db.WorkSubject{}, nil
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT p.id, p.entity_id::text, p.reason, p.unknown_reads, COALESCE(e.source_id, ''), COALESCE(e.title, '')
		FROM public.pr_mergeability_probes p
		LEFT JOIN public.entities e ON e.id = p.entity_id AND e.org_id = p.org_id
		WHERE p.org_id = $1 AND p.id = ANY($2)
	`, orgID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]db.WorkSubject, len(ids))
	for rows.Next() {
		var (
			id                              int64
			unknownReads                    int
			entityID, reason, source, title string
		)
		if err := rows.Scan(&id, &entityID, &reason, &unknownReads, &source, &title); err != nil {
			return nil, err
		}
		out[id] = db.PRMergeabilityProbeSubject(entityID, reason, unknownReads, source, title)
	}
	return out, rows.Err()
}
