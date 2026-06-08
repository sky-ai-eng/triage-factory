package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reviewStore is the SQLite impl of db.ReviewStore. SQL bodies are
// ported from the pre-D2 internal/db/reviews.go; the only behavioral
// change is the orgID assertion at each method entry. SQLite tables
// have no org_id column — local mode is single-tenant by construction.
//
// The constructor takes two queryers for signature parity with the
// Postgres impl's (app, admin) split. SQLite has one connection so
// the second arg is discarded; `...System` admin-pool variants are
// thin wrappers around their non-System counterparts.
type reviewStore struct{ q queryer }

func newReviewStore(q, _ queryer) db.ReviewStore { return &reviewStore{q: q} }

var _ db.ReviewStore = (*reviewStore)(nil)

// --- Reviews ---

func (s *reviewStore) Create(ctx context.Context, orgID string, r domain.PendingReview) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO pending_reviews (id, pr_number, owner, repo, commit_sha, diff_lines, diff_hunks, run_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.PRNumber, r.Owner, r.Repo, r.CommitSHA, r.DiffLines, r.DiffHunks, r.RunID,
	)
	return err
}

func (s *reviewStore) Get(ctx context.Context, orgID, reviewID string) (*domain.PendingReview, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx, `
		SELECT id, pr_number, owner, repo, commit_sha,
		       COALESCE(diff_lines, ''), COALESCE(diff_hunks, ''), COALESCE(run_id, ''),
		       COALESCE(review_body, ''), COALESCE(review_event, ''),
		       original_review_body, original_review_event
		FROM pending_reviews WHERE id = ?`, reviewID)
	return scanReviewRow(row)
}

func (s *reviewStore) ByRunID(ctx context.Context, orgID, runID string) (*domain.PendingReview, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	row := s.q.QueryRowContext(ctx,
		`SELECT id, pr_number, owner, repo, commit_sha,
		        COALESCE(diff_lines, ''), COALESCE(diff_hunks, ''), COALESCE(run_id, ''),
		        COALESCE(review_body, ''), COALESCE(review_event, ''),
		        original_review_body, original_review_event
		 FROM pending_reviews
		 WHERE run_id = ? AND review_event IS NOT NULL AND review_event != ''`, runID)
	return scanReviewRow(row)
}

func (s *reviewStore) Delete(ctx context.Context, orgID, reviewID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_review_comments WHERE review_id = ?`, reviewID); err != nil {
			return fmt.Errorf("delete review comments: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_reviews WHERE id = ?`, reviewID); err != nil {
			return fmt.Errorf("delete review: %w", err)
		}
		return nil
	})
}

func (s *reviewStore) DeleteByRunID(ctx context.Context, orgID, runID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	return inTx(ctx, s.q, func(tx queryer) error {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM pending_review_comments
			WHERE review_id IN (SELECT id FROM pending_reviews WHERE run_id = ?)`, runID); err != nil {
			return fmt.Errorf("delete review comments by run: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_reviews WHERE run_id = ?`, runID); err != nil {
			return fmt.Errorf("delete review by run: %w", err)
		}
		return nil
	})
}

func (s *reviewStore) SetSubmission(ctx context.Context, orgID, reviewID, body, event string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		`UPDATE pending_reviews
		 SET review_body = ?,
		     review_event = ?,
		     original_review_body = COALESCE(original_review_body, ?),
		     original_review_event = COALESCE(original_review_event, ?)
		 WHERE id = ?`,
		body, event, body, event, reviewID,
	)
	return err
}

func (s *reviewStore) LockSubmission(ctx context.Context, orgID, reviewID, body, event string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx,
		`UPDATE pending_reviews
		 SET review_body = ?,
		     review_event = ?,
		     original_review_body = COALESCE(original_review_body, ?),
		     original_review_event = COALESCE(original_review_event, ?)
		 WHERE id = ? AND (review_event IS NULL OR review_event = '')`,
		body, event, body, event, reviewID,
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
		if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_reviews WHERE id = ?`, reviewID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("pending review %s not found", reviewID)
		}
		return db.ErrPendingReviewAlreadySubmitted
	}
	return nil
}

// --- Comments ---

func (s *reviewStore) AddComment(ctx context.Context, orgID string, c domain.PendingReviewComment) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	_, err := s.q.ExecContext(ctx,
		`INSERT INTO pending_review_comments (id, review_id, path, line, start_line, body, original_body, severity)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.ReviewID, c.Path, c.Line, c.StartLine, c.Body, c.Body, c.Severity,
	)
	return err
}

func (s *reviewStore) UpdateComment(ctx context.Context, orgID, commentID, body string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, `UPDATE pending_review_comments SET body = ? WHERE id = ?`, body, commentID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("comment %s: %w", commentID, db.ErrPendingReviewCommentNotFound)
	}
	return nil
}

func (s *reviewStore) DeleteComment(ctx context.Context, orgID, commentID string) error {
	if err := assertLocalOrg(orgID); err != nil {
		return err
	}
	res, err := s.q.ExecContext(ctx, `DELETE FROM pending_review_comments WHERE id = ?`, commentID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("comment %s: %w", commentID, db.ErrPendingReviewCommentNotFound)
	}
	return nil
}

func (s *reviewStore) ListComments(ctx context.Context, orgID, reviewID string) ([]domain.PendingReviewComment, error) {
	if err := assertLocalOrg(orgID); err != nil {
		return nil, err
	}
	// original_body NOT COALESCEd — see domain.PendingReviewComment.
	rows, err := s.q.QueryContext(ctx,
		`SELECT id, review_id, path, line, start_line, body, original_body, severity
		 FROM pending_review_comments WHERE review_id = ? ORDER BY rowid`,
		reviewID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []domain.PendingReviewComment{}
	for rows.Next() {
		var c domain.PendingReviewComment
		var startLine sql.NullInt64
		var origBody sql.NullString
		if err := rows.Scan(&c.ID, &c.ReviewID, &c.Path, &c.Line, &startLine, &c.Body, &origBody, &c.Severity); err != nil {
			return nil, err
		}
		if startLine.Valid {
			v := int(startLine.Int64)
			c.StartLine = &v
		}
		if origBody.Valid {
			s := origBody.String
			c.OriginalBody = &s
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *reviewStore) IsCommentID(ctx context.Context, orgID, commentID string) bool {
	if err := assertLocalOrg(orgID); err != nil {
		return false
	}
	var count int
	if err := s.q.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending_review_comments WHERE id = ?`, commentID).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func (s *reviewStore) ByRunIDSystem(ctx context.Context, orgID, runID string) (*domain.PendingReview, error) {
	return s.ByRunID(ctx, orgID, runID)
}

// --- SKY-302 admin-pool variants — SQLite collapses to non-System ---

func (s *reviewStore) CreateSystem(ctx context.Context, orgID string, r domain.PendingReview) error {
	return s.Create(ctx, orgID, r)
}

func (s *reviewStore) GetSystem(ctx context.Context, orgID, reviewID string) (*domain.PendingReview, error) {
	return s.Get(ctx, orgID, reviewID)
}

func (s *reviewStore) DeleteSystem(ctx context.Context, orgID, reviewID string) error {
	return s.Delete(ctx, orgID, reviewID)
}

func (s *reviewStore) LockSubmissionSystem(ctx context.Context, orgID, reviewID, body, event string) error {
	return s.LockSubmission(ctx, orgID, reviewID, body, event)
}

func (s *reviewStore) AddCommentSystem(ctx context.Context, orgID string, c domain.PendingReviewComment) error {
	return s.AddComment(ctx, orgID, c)
}

func (s *reviewStore) UpdateCommentSystem(ctx context.Context, orgID, commentID, body string) error {
	return s.UpdateComment(ctx, orgID, commentID, body)
}

func (s *reviewStore) DeleteCommentSystem(ctx context.Context, orgID, commentID string) error {
	return s.DeleteComment(ctx, orgID, commentID)
}

func (s *reviewStore) ListCommentsSystem(ctx context.Context, orgID, reviewID string) ([]domain.PendingReviewComment, error) {
	return s.ListComments(ctx, orgID, reviewID)
}

// scanReviewRow shared between Get and ByRunID. *Row.Scan; on no-rows
// returns (nil, nil) per the read-path contract.
func scanReviewRow(row *sql.Row) (*domain.PendingReview, error) {
	var r domain.PendingReview
	var origBody, origEvent sql.NullString
	err := row.Scan(
		&r.ID, &r.PRNumber, &r.Owner, &r.Repo, &r.CommitSHA,
		&r.DiffLines, &r.DiffHunks, &r.RunID, &r.ReviewBody, &r.ReviewEvent,
		&origBody, &origEvent,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if origBody.Valid {
		s := origBody.String
		r.OriginalReviewBody = &s
	}
	if origEvent.Valid {
		s := origEvent.String
		r.OriginalReviewEvent = &s
	}
	return &r, nil
}
