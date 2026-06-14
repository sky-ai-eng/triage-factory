package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// pendingPRStore is the SQLite impl of db.PendingPRStore. SQL bodies
// are ported from the pre-D2 internal/db/pending_prs.go; the only
// behavioral change is the orgID assertion at each method entry.
// SQLite's pending_prs table has an org_id column with a default
// pointing at the local sentinel, so writes don't need to set it
// explicitly.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl's (app, admin) split. SQLite has one connection so
// the second arg is discarded; `...System` admin-pool variants are
// thin wrappers around their non-System counterparts.
//
// pending_prs is a leaf table — no child rows hang off it — so
// Delete / DeleteByRunID are single-statement on both backends and
// don't need a transaction wrapper.
type pendingPRStore struct{ q queryer }

func newPendingPRStore(q, _ queryer) db.PendingPRStore { return &pendingPRStore{q: q} }

var _ db.PendingPRStore = (*pendingPRStore)(nil)

func (s *pendingPRStore) Create(ctx context.Context, orgID string, p domain.PendingPR) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	// One active pending PR per run is now an APP-LAYER guard: the DB
	// UNIQUE(run_id) was dropped at this baseline so a multi-repo run can
	// eventually queue several PRs. Until that feature lands we still block a
	// second PR per run, as a guarded INSERT ... WHERE NOT EXISTS — a single
	// statement (no TOCTOU under SQLite's single writer) that surfaces a dupe
	// as the clean ErrPendingPRAlreadyQueued the `pr create` flow already
	// teaches the agent on, rather than a raw SQL constraint error. Relax to
	// per-(run, repo) when multi-repo PR opening ships.
	res, err := s.q.ExecContext(ctx,
		`INSERT INTO pending_prs
		   (id, run_id, owner, repo, head_branch, head_sha, base_branch,
		    title, body, original_title, original_body, draft)
		 SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		  WHERE NOT EXISTS (SELECT 1 FROM pending_prs WHERE run_id = ?)`,
		p.ID, p.RunID, p.Owner, p.Repo, p.HeadBranch, p.HeadSHA, p.BaseBranch,
		p.Title, nullIfEmpty(p.Body),
		// Snapshot the agent's draft as originals at insert time so
		// the human-feedback diff has a stable baseline even if the
		// user edits before the agent has called Lock.
		p.Title, nullIfEmpty(p.Body),
		boolToInt(p.Draft),
		p.RunID,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return db.ErrPendingPRAlreadyQueued
	}
	return nil
}

func (s *pendingPRStore) Get(ctx context.Context, orgID, id string) (*domain.PendingPR, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, run_id, owner, repo, head_branch, head_sha, base_branch,
		       title, COALESCE(body, ''),
		       original_title, original_body,
		       draft, locked, submitted_at, created_at
		  FROM pending_prs
		 WHERE id = ?`, id)
	return scanPendingPRRow(row)
}

func (s *pendingPRStore) ByRunID(ctx context.Context, orgID, runID string) (*domain.PendingPR, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, run_id, owner, repo, head_branch, head_sha, base_branch,
		       title, COALESCE(body, ''),
		       original_title, original_body,
		       draft, locked, submitted_at, created_at
		  FROM pending_prs
		 WHERE run_id = ?`, runID)
	return scanPendingPRRow(row)
}

func (s *pendingPRStore) UpdateTitleBody(ctx context.Context, orgID, id, title, body string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx,
		`UPDATE pending_prs
		    SET title = ?,
		        body = ?,
		        original_title = COALESCE(original_title, ?),
		        original_body = COALESCE(original_body, ?)
		  WHERE id = ? AND submitted_at IS NULL`,
		title, nullIfEmpty(body), title, nullIfEmpty(body), id,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Disambiguate "row not found" from "row exists but already
		// submitted" so the caller can give the user the right
		// reason. A second SELECT is cheap relative to the cost of
		// a misleading toast.
		var submittedAt sql.NullTime
		err := s.q.QueryRowContext(ctx, `SELECT submitted_at FROM pending_prs WHERE id = ?`, id).Scan(&submittedAt)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("pending PR %s not found", id)
			}
			return err
		}
		if submittedAt.Valid {
			return db.ErrPendingPRSubmitted
		}
		return fmt.Errorf("pending PR %s update matched 0 rows but row state is consistent; investigate", id)
	}
	return nil
}

func (s *pendingPRStore) Lock(ctx context.Context, orgID, id, title, body string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx,
		`UPDATE pending_prs
		    SET title = ?,
		        body = ?,
		        original_title = COALESCE(original_title, ?),
		        original_body = COALESCE(original_body, ?),
		        locked = 1
		  WHERE id = ? AND locked = 0`,
		title, nullIfEmpty(body), title, nullIfEmpty(body), id,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Distinguish "already queued" from "no such PR" — same
		// SKY-212 disambiguation pattern as ReviewStore.LockSubmission.
		var exists int
		if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_prs WHERE id = ?`, id).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("pending PR %s not found", id)
		}
		return db.ErrPendingPRAlreadyQueued
	}
	return nil
}

func (s *pendingPRStore) MarkSubmitted(ctx context.Context, orgID, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx,
		`UPDATE pending_prs
		    SET submitted_at = ?
		  WHERE id = ? AND submitted_at IS NULL`,
		time.Now().UTC(), id,
	)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var exists int
		if qerr := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_prs WHERE id = ?`, id).Scan(&exists); qerr != nil {
			return qerr
		}
		if exists == 0 {
			return fmt.Errorf("pending PR %s not found", id)
		}
		return db.ErrPendingPRSubmitInFlight
	}
	return nil
}

func (s *pendingPRStore) ClearSubmitted(ctx context.Context, orgID, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE pending_prs SET submitted_at = NULL WHERE id = ?`, id,
	)
	return err
}

func (s *pendingPRStore) Delete(ctx context.Context, orgID, id string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, `DELETE FROM pending_prs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending PR %s not found", id)
	}
	return nil
}

func (s *pendingPRStore) DeleteByRunID(ctx context.Context, orgID, runID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx, `DELETE FROM pending_prs WHERE run_id = ?`, runID)
	return err
}

func (s *pendingPRStore) ByRunIDSystem(ctx context.Context, orgID, runID string) (*domain.PendingPR, error) {
	return s.ByRunID(ctx, orgID, runID)
}

// --- SKY-302 admin-pool variants — SQLite collapses to non-System ---

func (s *pendingPRStore) CreateSystem(ctx context.Context, orgID string, p domain.PendingPR) error {
	return s.Create(ctx, orgID, p)
}

func (s *pendingPRStore) LockSystem(ctx context.Context, orgID, id, title, body string) error {
	return s.Lock(ctx, orgID, id, title, body)
}

// scanPendingPRRow shared between Get and ByRunID. *Row.Scan; on
// no-rows returns (nil, nil) per the read-path contract.
func scanPendingPRRow(row *sql.Row) (*domain.PendingPR, error) {
	var p domain.PendingPR
	var origTitle, origBody sql.NullString
	var draft, locked int
	var submittedAt sql.NullTime
	err := row.Scan(
		&p.ID, &p.RunID, &p.Owner, &p.Repo, &p.HeadBranch, &p.HeadSHA, &p.BaseBranch,
		&p.Title, &p.Body,
		&origTitle, &origBody,
		&draft, &locked, &submittedAt, &p.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if origTitle.Valid {
		s := origTitle.String
		p.OriginalTitle = &s
	}
	if origBody.Valid {
		s := origBody.String
		p.OriginalBody = &s
	}
	p.Draft = draft != 0
	p.Locked = locked != 0
	if submittedAt.Valid {
		t := submittedAt.Time
		p.SubmittedAt = &t
	}
	return &p, nil
}

// boolToInt is the SQLite-idiomatic 0/1 mapping. Local to this file
// to avoid leaking a generic helper into a wider scope.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
