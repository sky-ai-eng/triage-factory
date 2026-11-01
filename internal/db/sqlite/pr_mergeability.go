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

// prMergeabilityStore is the SQLite impl of db.PRMergeabilityStore. The
// handle's IMMEDIATE begin serializes a probe's completion against every
// other writer of the file, so the answer it reads as previous is the one its
// upsert replaces.
type prMergeabilityStore struct {
	conn  *sql.DB
	kind  workitem.Kind
	queue workitem.Kind
}

func newPRMergeabilityStore(conn *sql.DB) db.PRMergeabilityStore {
	return &prMergeabilityStore{
		conn:  conn,
		kind:  workkinds.PRMergeabilityProbes(workitem.SQLite),
		queue: workkinds.EventQueue(workitem.SQLite),
	}
}

var _ db.PRMergeabilityStore = (*prMergeabilityStore)(nil)

const sqlitePRMergeabilityColumns = `entity_id, org_id, COALESCE(mergeable, ''), COALESCE(mergeable_head_sha, ''), COALESCE(mergeable_base_ref, ''), checked_at, COALESCE(base_move_reason, ''), give_ups`

func scanSQLitePRMergeability(row interface{ Scan(...any) error }) (domain.PRMergeability, error) {
	var m domain.PRMergeability
	err := row.Scan(&m.EntityID, &m.OrgID, &m.Mergeable, &m.MergeableHeadSHA, &m.MergeableBaseRef, &m.CheckedAt, &m.BaseMoveReason, &m.GiveUps)
	return m, err
}

// sqliteClearBaseMove is the SET clauses a read's upsert clears the row's
// recorded base move with when the probe making it was leased at or after the
// move was recorded, its one bind being the probe's id. Both columns are
// leased_at's text shape, so they compare as text; the probe's lease is still
// held, since the completion's closure runs before its terminal flip.
const sqliteClearBaseMove = `
				base_move_reason = CASE WHEN pull_request_mergeability.base_moved_at <= (SELECT leased_at FROM pr_mergeability_probes WHERE id = ?)
					THEN NULL ELSE pull_request_mergeability.base_move_reason END,
				base_moved_at = CASE WHEN pull_request_mergeability.base_moved_at <= (SELECT leased_at FROM pr_mergeability_probes WHERE id = ?)
					THEN NULL ELSE pull_request_mergeability.base_moved_at END`

func (s *prMergeabilityStore) GetSystem(ctx context.Context, orgID, entityID string) (*domain.PRMergeability, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	m, err := scanSQLitePRMergeability(s.conn.QueryRowContext(ctx, `
		SELECT `+sqlitePRMergeabilityColumns+` FROM pull_request_mergeability WHERE entity_id = ?
	`, entityID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *prMergeabilityStore) ListSystem(ctx context.Context, orgID string) ([]domain.PRMergeability, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT `+sqlitePRMergeabilityColumns+` FROM pull_request_mergeability ORDER BY entity_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PRMergeability
	for rows.Next() {
		m, err := scanSQLitePRMergeability(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *prMergeabilityStore) DeleteInactiveSystem(ctx context.Context, orgID string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	res, err := s.conn.ExecContext(ctx, `
		DELETE FROM pull_request_mergeability
		WHERE NOT EXISTS (
			SELECT 1 FROM entities e WHERE e.id = pull_request_mergeability.entity_id AND e.state = 'active'
		)
	`)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// probeAfter is the instant a probe admitted now with delay becomes ripe, in
// the block's timestamp shape, which is what the claim filter compares
// against as text.
func probeAfter(delay time.Duration) string {
	return time.Now().UTC().Add(delay).Format(blockTimeLayout)
}

func (s *prMergeabilityStore) AdmitSystem(ctx context.Context, orgID string, probes []db.PRMergeabilityProbe) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	if err := db.ValidatePRMergeabilityProbes(probes); err != nil {
		return 0, err
	}
	if len(probes) == 0 {
		return 0, nil
	}
	admitted := 0
	err := db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
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
	if err := assertLocalOrg(orgID); err != nil {
		return db.PRMergeabilityClaim{}, err
	}
	res, claimErr := workitem.Claim(ctx, s.conn, s.kind, owner, orgID, n)
	out := db.PRMergeabilityClaim{Cancelled: res.Cancelled, Parked: res.Parked, Reclaimed: res.Reclaimed}
	if len(res.Claimed) == 0 {
		return out, claimErr
	}
	// The kind's columns are immutable after admission, so reading them by
	// id after the claim reads the values the row was admitted with. The
	// entity and its repository are read as they are now; the completion
	// reads the entity again under the probe's lock.
	marks := make([]string, len(res.Claimed))
	args := make([]any, len(res.Claimed))
	for i, r := range res.Claimed {
		marks[i] = "?"
		args[i] = r.ItemID
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT p.id, p.entity_id, p.reason, p.unknown_reads,
		       COALESCE(e.external_id, ''), COALESCE(e.state = 'active', 0),
		       COALESCE(e.repository_id, ''), COALESCE(r.owner, ''), COALESCE(r.repo, '')
		FROM pr_mergeability_probes p
		LEFT JOIN entities e ON e.id = p.entity_id
		LEFT JOIN repositories r ON r.id = e.repository_id
		WHERE p.id IN (`+strings.Join(marks, ", ")+`)
	`, args...)
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
	if err := assertLocalOrg(r.OrgID); err != nil {
		return workitem.Receipt{}, err
	}
	return workitem.RenewLease(ctx, s.conn, s.kind, r)
}

func (s *prMergeabilityStore) CompleteProbe(ctx context.Context, r workitem.Receipt, answer db.PRMergeabilityAnswer, traceparent string) (db.PRMergeabilityProbeResult, error) {
	if err := assertLocalOrg(r.OrgID); err != nil {
		return db.PRMergeabilityProbeResult{}, err
	}
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
			SELECT p.entity_id, p.unknown_reads,
			       COALESCE(e.state = 'active', 0), COALESCE(e.repository_id, ''), COALESCE(e.snapshot_json, '')
			FROM pr_mergeability_probes p
			LEFT JOIN entities e ON e.id = p.entity_id
			WHERE p.id = ?
		`, r.ItemID).Scan(&entityID, &unknownReads, &active, &repositoryID, &snapshotJSON); err != nil {
			return nil, fmt.Errorf("read pr_mergeability_probes row %d: %w", r.ItemID, err)
		}
		if !answer.Found || !active {
			return nil, nil
		}
		if answer.Mergeable == domain.MergeableUnknown {
			if unknownReads+1 >= workkinds.PRMergeabilityMaxUnknownReads {
				// The give-up moves the row's head and time, leaves its
				// answer (NULL on a new row) and counts the give-up, so the
				// poll waits a while before a fresh round of reads rather
				// than admitting the pull request again at once.
				row, err := scanSQLitePRMergeability(tx.QueryRowContext(ctx, `
					INSERT INTO pull_request_mergeability (entity_id, org_id, mergeable, mergeable_head_sha, mergeable_base_ref, checked_at, give_ups)
					VALUES (?, ?, NULL, ?, ?, ?, 1)
					ON CONFLICT (entity_id) DO UPDATE SET
						mergeable_head_sha = excluded.mergeable_head_sha,
						mergeable_base_ref = excluded.mergeable_base_ref,
						checked_at = excluded.checked_at,
						give_ups = pull_request_mergeability.give_ups + 1,`+sqliteClearBaseMove+`
					RETURNING `+sqlitePRMergeabilityColumns,
					entityID, r.OrgID, nullIfEmpty(answer.HeadSHA), nullIfEmpty(answer.BaseRef), time.Now().UTC(), r.ItemID, r.ItemID))
				if err != nil {
					return nil, fmt.Errorf("record the give-up for entity %s: %w", entityID, err)
				}
				res.Outcome, res.Row = db.PRMergeabilityGaveUp, &row
				return nil, nil
			}
			res.Outcome = db.PRMergeabilityRetried
			return db.PRMergeabilityProbeCols(entityID, domain.MergeabilityProbeUnknownRetry,
				probeAfter(workkinds.PRMergeabilityUnknownRetryDelay), unknownReads+1), nil
		}

		var prev sql.NullString
		switch err := tx.QueryRowContext(ctx, `SELECT mergeable FROM pull_request_mergeability WHERE entity_id = ?`, entityID).Scan(&prev); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("read mergeability of entity %s: %w", entityID, err)
		}
		now := time.Now().UTC()
		row, err := scanSQLitePRMergeability(tx.QueryRowContext(ctx, `
			INSERT INTO pull_request_mergeability (entity_id, org_id, mergeable, mergeable_head_sha, mergeable_base_ref, checked_at)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (entity_id) DO UPDATE SET
				mergeable = excluded.mergeable,
				mergeable_head_sha = excluded.mergeable_head_sha,
				mergeable_base_ref = excluded.mergeable_base_ref,
				checked_at = excluded.checked_at,
				give_ups = 0,`+sqliteClearBaseMove+`
			RETURNING `+sqlitePRMergeabilityColumns,
			entityID, r.OrgID, answer.Mergeable, nullIfEmpty(answer.HeadSHA), nullIfEmpty(answer.BaseRef), now, r.ItemID, r.ItemID))
		if err != nil {
			return nil, fmt.Errorf("write mergeability of entity %s: %w", entityID, err)
		}
		res.Outcome, res.Previous, res.Row = db.PRMergeabilityRecorded, prev.String, &row

		evt, ok, err := db.PRMergeabilityTransition(r.OrgID, entityID, repositoryID, snapshotJSON, prev.String, answer.Mergeable, answer.HeadSHA, now)
		if err != nil || !ok {
			return nil, err
		}
		id, err := recordEvent(ctx, tx, evt)
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
	if err := assertLocalOrg(r.OrgID); err != nil {
		return false, err
	}
	return workitem.Requeue(ctx, s.conn, s.kind, r, outcome, cause)
}

func (s *prMergeabilityStore) DeferRateLimited(ctx context.Context, r workitem.Receipt, until time.Time) error {
	if err := assertLocalOrg(r.OrgID); err != nil {
		return err
	}
	return workitem.Defer(ctx, s.conn, s.kind, r, workkinds.PRMergeabilityDeferRateLimited, until, db.PRMergeabilityRateLimitHolds)
}

func (s *prMergeabilityStore) PruneSettled(ctx context.Context, before time.Time) (int, error) {
	// done_at is block text in the package's layout, so the cutoff is bound
	// in that layout for a plain text comparison.
	res, err := s.conn.ExecContext(ctx, `
		DELETE FROM pr_mergeability_probes
		WHERE status IN ('done', 'cancelled') AND done_at < ?
	`, before.UTC().Format(blockTimeLayout))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// The store is also the kind's WorkKindHandle, as the other kinds' are: the
// surface runs the package's own reads and controls on Conn, so the handler
// enforces the local sentinel; Describe is the store's own and still does.
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
// row is described whether or not its entity is still there.
func (s *prMergeabilityStore) Describe(ctx context.Context, orgID string, ids []int64) (map[int64]db.WorkSubject, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return map[int64]db.WorkSubject{}, nil
	}
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i] = "?"
		args[i] = id
	}
	rows, err := s.conn.QueryContext(ctx, `
		SELECT p.id, p.entity_id, p.reason, p.unknown_reads, COALESCE(e.source_id, ''), COALESCE(e.title, '')
		FROM pr_mergeability_probes p
		LEFT JOIN entities e ON e.id = p.entity_id
		WHERE p.id IN (`+strings.Join(marks, ", ")+`)
	`, args...)
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

const sqlitePRPathsColumns = `entity_id, org_id, paths_json, COALESCE(paths_head_sha, ''), COALESCE(paths_base_ref, ''), paths_complete`

func scanSQLitePRPaths(row interface{ Scan(...any) error }) (domain.PRPaths, error) {
	var p domain.PRPaths
	var raw sql.NullString
	if err := row.Scan(&p.EntityID, &p.OrgID, &raw, &p.HeadSHA, &p.BaseRef, &p.Complete); err != nil {
		return domain.PRPaths{}, err
	}
	paths, err := db.DecodePRPaths(raw)
	if err != nil {
		return domain.PRPaths{}, fmt.Errorf("entity %s: %w", p.EntityID, err)
	}
	p.Paths = paths
	return p, nil
}

func (s *prMergeabilityStore) ListPathsSystem(ctx context.Context, orgID string, entityIDs []string) ([]domain.PRPaths, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	if len(entityIDs) == 0 {
		return nil, nil
	}
	marks, args := inListArgs(entityIDs)
	rows, err := s.conn.QueryContext(ctx, `
		SELECT `+sqlitePRPathsColumns+` FROM pull_request_mergeability
		WHERE entity_id IN (`+marks+`)
		ORDER BY entity_id
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PRPaths
	for rows.Next() {
		p, err := scanSQLitePRPaths(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *prMergeabilityStore) ClearPathsSystem(ctx context.Context, orgID string, entityIDs []string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	if len(entityIDs) == 0 {
		return 0, nil
	}
	marks, args := inListArgs(entityIDs)
	res, err := s.conn.ExecContext(ctx, `
		UPDATE pull_request_mergeability
		SET paths_json = NULL, paths_head_sha = NULL, paths_base_ref = NULL, paths_complete = 0
		WHERE entity_id IN (`+marks+`) AND paths_json IS NOT NULL
	`, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func (s *prMergeabilityStore) MarkBaseMovedSystem(ctx context.Context, orgID string, entityIDs []string, reason string) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	if err := db.ValidateBaseMoveReason(reason); err != nil {
		return 0, err
	}
	if len(entityIDs) == 0 {
		return 0, nil
	}
	written := 0
	err := db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
		written = 0
		for _, id := range entityIDs {
			// base_moved_at is written in the probe queue's leased_at shape,
			// which the read's upsert compares it with.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO pull_request_mergeability (entity_id, org_id, base_move_reason, base_moved_at)
				VALUES (?, ?, ?, `+sqliteNowExpr+`)
				ON CONFLICT (entity_id) DO UPDATE SET
					base_move_reason = excluded.base_move_reason,
					base_moved_at = excluded.base_moved_at
			`, id, orgID, reason); err != nil {
				return fmt.Errorf("record the base move of entity %s: %w", id, err)
			}
			written++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return written, nil
}

func (s *prMergeabilityStore) UpsertPathsSystem(ctx context.Context, orgID string, p domain.PRPaths) (domain.PRPaths, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.PRPaths{}, err
	}
	if err := db.ValidatePRPaths(p); err != nil {
		return domain.PRPaths{}, err
	}
	raw, err := db.EncodePRPaths(p.Paths)
	if err != nil {
		return domain.PRPaths{}, err
	}
	return scanSQLitePRPaths(s.conn.QueryRowContext(ctx, `
		INSERT INTO pull_request_mergeability (entity_id, org_id, paths_json, paths_head_sha, paths_base_ref, paths_complete)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (entity_id) DO UPDATE SET
			paths_json = excluded.paths_json,
			paths_head_sha = excluded.paths_head_sha,
			paths_base_ref = excluded.paths_base_ref,
			paths_complete = excluded.paths_complete
		RETURNING `+sqlitePRPathsColumns,
		p.EntityID, orgID, raw, p.HeadSHA, p.BaseRef, p.Complete))
}

const sqliteBranchTipColumns = `repository_id, branch, org_id, tip_sha, COALESCE(etag, ''), observed_at, compare_failing_since`

func scanSQLiteBranchTip(row interface{ Scan(...any) error }) (domain.BranchTip, error) {
	var b domain.BranchTip
	err := row.Scan(&b.RepositoryID, &b.Branch, &b.OrgID, &b.TipSHA, &b.ETag, &b.ObservedAt, &b.CompareFailingSince)
	return b, err
}

// utcPtrValue binds an optional time as UTC, or NULL.
func utcPtrValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

func (s *prMergeabilityStore) ListBranchTipsSystem(ctx context.Context, orgID string) ([]domain.BranchTip, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	return s.listBranchTips(ctx, s.conn)
}

func (s *prMergeabilityStore) listBranchTips(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]domain.BranchTip, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+sqliteBranchTipColumns+` FROM branch_tips ORDER BY repository_id, branch
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.BranchTip
	for rows.Next() {
		b, err := scanSQLiteBranchTip(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *prMergeabilityStore) UpsertBranchTipSystem(ctx context.Context, orgID string, tip domain.BranchTip) (domain.BranchTip, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return domain.BranchTip{}, err
	}
	if err := db.ValidateBranchTip(tip); err != nil {
		return domain.BranchTip{}, err
	}
	return scanSQLiteBranchTip(s.conn.QueryRowContext(ctx, `
		INSERT INTO branch_tips (repository_id, branch, org_id, tip_sha, etag, observed_at, compare_failing_since)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repository_id, branch) DO UPDATE SET
			tip_sha = excluded.tip_sha,
			etag = excluded.etag,
			observed_at = excluded.observed_at,
			compare_failing_since = excluded.compare_failing_since
		RETURNING `+sqliteBranchTipColumns,
		tip.RepositoryID, tip.Branch, orgID, tip.TipSHA, nullIfEmpty(tip.ETag), tip.ObservedAt.UTC(), utcPtrValue(tip.CompareFailingSince)))
}

func (s *prMergeabilityStore) DeleteBranchTipsExceptSystem(ctx context.Context, orgID string, keep []domain.BranchRef) (int, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return 0, err
	}
	deleted := 0
	err := db.InTx(ctx, s.conn, func(tx *sql.Tx) error {
		deleted = 0
		stored, err := s.listBranchTips(ctx, tx)
		if err != nil {
			return err
		}
		for _, ref := range db.BranchTipsToDelete(stored, keep) {
			if _, err := tx.ExecContext(ctx, `DELETE FROM branch_tips WHERE repository_id = ? AND branch = ?`, ref.RepositoryID, ref.Branch); err != nil {
				return fmt.Errorf("delete branch tip %s: %w", ref.Branch, err)
			}
			deleted++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}
