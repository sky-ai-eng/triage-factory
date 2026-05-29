package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// reviewStore is the Postgres impl of db.ReviewStore. Holds two
// pools:
//
//   - q: app pool (tf_app, RLS-active). Every request-equivalent
//     consumer (reviews handler, swipe-dismiss, agent submit-review
//     tool) runs here.
//
//   - admin: admin pool (supabase_admin, BYPASSRLS). The delegate
//     spawner's processCompletion reads the pending review attached
//     to a run from a goroutine that has detached from any request
//     context, so it routes through admin via ByRunIDSystem. org_id
//     stays in the WHERE clause as defense in depth.
//
// SQL is written fresh against D3's schema: $N placeholders, org_id
// in every INSERT, NULL run_id handled via NULLIF on the empty-string
// caller bind so the unique-index path doesn't see "" vs uuid.
//
// Postgres has ON DELETE CASCADE on
// pending_review_comments.review_id, so Delete / DeleteByRunID can
// run a single statement against pending_reviews and the comment
// rows go automatically.
type reviewStore struct {
	q     queryer
	admin queryer
}

func newReviewStore(q, admin queryer) db.ReviewStore {
	return &reviewStore{q: q, admin: admin}
}

var _ db.ReviewStore = (*reviewStore)(nil)

// --- Reviews ---

func (s *reviewStore) Create(ctx context.Context, orgID string, r domain.PendingReview) error {
	return createReview(ctx, s.q, orgID, r)
}

func (s *reviewStore) CreateSystem(ctx context.Context, orgID string, r domain.PendingReview) error {
	return createReview(ctx, s.admin, orgID, r)
}

func createReview(ctx context.Context, q queryer, orgID string, r domain.PendingReview) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO pending_reviews (id, org_id, pr_number, owner, repo, commit_sha,
		                             diff_lines, diff_hunks, run_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, '')::uuid)
	`,
		r.ID, orgID, r.PRNumber, r.Owner, r.Repo, r.CommitSHA,
		r.DiffLines, r.DiffHunks, r.RunID,
	)
	return err
}

func (s *reviewStore) Get(ctx context.Context, orgID, reviewID string) (*domain.PendingReview, error) {
	return getReview(ctx, s.q, orgID, reviewID)
}

func (s *reviewStore) GetSystem(ctx context.Context, orgID, reviewID string) (*domain.PendingReview, error) {
	return getReview(ctx, s.admin, orgID, reviewID)
}

func getReview(ctx context.Context, q queryer, orgID, reviewID string) (*domain.PendingReview, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, pr_number, owner, repo, commit_sha,
		       COALESCE(diff_lines, ''), COALESCE(diff_hunks, ''),
		       COALESCE(run_id::text, ''),
		       COALESCE(review_body, ''), COALESCE(review_event, ''),
		       original_review_body, original_review_event
		FROM pending_reviews WHERE org_id = $1 AND id = $2
	`, orgID, reviewID)
	return pgScanReviewRow(row)
}

func (s *reviewStore) ByRunID(ctx context.Context, orgID, runID string) (*domain.PendingReview, error) {
	return reviewByRunID(ctx, s.q, orgID, runID)
}

func (s *reviewStore) ByRunIDSystem(ctx context.Context, orgID, runID string) (*domain.PendingReview, error) {
	return reviewByRunID(ctx, s.admin, orgID, runID)
}

func reviewByRunID(ctx context.Context, q queryer, orgID, runID string) (*domain.PendingReview, error) {
	row := q.QueryRowContext(ctx, `
		SELECT id, pr_number, owner, repo, commit_sha,
		       COALESCE(diff_lines, ''), COALESCE(diff_hunks, ''),
		       COALESCE(run_id::text, ''),
		       COALESCE(review_body, ''), COALESCE(review_event, ''),
		       original_review_body, original_review_event
		FROM pending_reviews
		WHERE org_id = $1 AND run_id = $2
		  AND review_event IS NOT NULL AND review_event != ''
	`, orgID, runID)
	return pgScanReviewRow(row)
}

func (s *reviewStore) Delete(ctx context.Context, orgID, reviewID string) error {
	return deleteReview(ctx, s.q, orgID, reviewID)
}

func (s *reviewStore) DeleteSystem(ctx context.Context, orgID, reviewID string) error {
	return deleteReview(ctx, s.admin, orgID, reviewID)
}

func deleteReview(ctx context.Context, q queryer, orgID, reviewID string) error {
	// pending_review_comments.review_id has ON DELETE CASCADE, so a
	// single DELETE against pending_reviews tears down the comments
	// too. No manual transaction needed.
	_, err := q.ExecContext(ctx,
		`DELETE FROM pending_reviews WHERE org_id = $1 AND id = $2`, orgID, reviewID)
	return err
}

func (s *reviewStore) DeleteByRunID(ctx context.Context, orgID, runID string) error {
	_, err := s.q.ExecContext(ctx,
		`DELETE FROM pending_reviews WHERE org_id = $1 AND run_id = $2`, orgID, runID)
	return err
}

func (s *reviewStore) SetSubmission(ctx context.Context, orgID, reviewID, body, event string) error {
	_, err := s.q.ExecContext(ctx, `
		UPDATE pending_reviews
		SET review_body = $1,
		    review_event = $2,
		    original_review_body = COALESCE(original_review_body, $3),
		    original_review_event = COALESCE(original_review_event, $4)
		WHERE org_id = $5 AND id = $6
	`, body, event, body, event, orgID, reviewID)
	return err
}

func (s *reviewStore) LockSubmission(ctx context.Context, orgID, reviewID, body, event string) error {
	return lockReviewSubmission(ctx, s.q, orgID, reviewID, body, event)
}

func (s *reviewStore) LockSubmissionSystem(ctx context.Context, orgID, reviewID, body, event string) error {
	return lockReviewSubmission(ctx, s.admin, orgID, reviewID, body, event)
}

func lockReviewSubmission(ctx context.Context, q queryer, orgID, reviewID, body, event string) error {
	res, err := q.ExecContext(ctx, `
		UPDATE pending_reviews
		SET review_body = $1,
		    review_event = $2,
		    original_review_body = COALESCE(original_review_body, $3),
		    original_review_event = COALESCE(original_review_event, $4)
		WHERE org_id = $5 AND id = $6
		  AND (review_event IS NULL OR review_event = '')
	`, body, event, body, event, orgID, reviewID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		// Distinguish "already submitted" from "no such review".
		// The lookup is cheap and the row is already in the page
		// cache from the prSubmitReview load.
		var exists int
		if err := q.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pending_reviews WHERE org_id = $1 AND id = $2`,
			orgID, reviewID,
		).Scan(&exists); err != nil {
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
	return addReviewComment(ctx, s.q, orgID, c)
}

func (s *reviewStore) AddCommentSystem(ctx context.Context, orgID string, c domain.PendingReviewComment) error {
	return addReviewComment(ctx, s.admin, orgID, c)
}

func addReviewComment(ctx context.Context, q queryer, orgID string, c domain.PendingReviewComment) error {
	// start_line is *int — nil binds as SQL NULL.
	var startLine any
	if c.StartLine != nil {
		startLine = *c.StartLine
	}
	// Bind created_at = clock_timestamp() rather than rely on the
	// column's now() default. now() is equivalent to
	// transaction_timestamp() and is fixed for the lifetime of a
	// transaction, so batched AddComment calls inside one WithTx
	// would share the same created_at and ListComments's id
	// tiebreaker (random UUIDs) would scramble insertion order.
	// clock_timestamp() advances per row.
	_, err := q.ExecContext(ctx, `
		INSERT INTO pending_review_comments (id, org_id, review_id, path, line, start_line, body, original_body, created_at, severity)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7, clock_timestamp(), $8)
	`, c.ID, orgID, c.ReviewID, c.Path, c.Line, startLine, c.Body, c.Severity)
	return err
}

func (s *reviewStore) UpdateComment(ctx context.Context, orgID, commentID, body string) error {
	return updateReviewComment(ctx, s.q, orgID, commentID, body)
}

func (s *reviewStore) UpdateCommentSystem(ctx context.Context, orgID, commentID, body string) error {
	return updateReviewComment(ctx, s.admin, orgID, commentID, body)
}

func updateReviewComment(ctx context.Context, q queryer, orgID, commentID, body string) error {
	res, err := q.ExecContext(ctx,
		`UPDATE pending_review_comments SET body = $1 WHERE org_id = $2 AND id = $3`,
		body, orgID, commentID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending comment %s not found", commentID)
	}
	return nil
}

func (s *reviewStore) DeleteComment(ctx context.Context, orgID, commentID string) error {
	return deleteReviewComment(ctx, s.q, orgID, commentID)
}

func (s *reviewStore) DeleteCommentSystem(ctx context.Context, orgID, commentID string) error {
	return deleteReviewComment(ctx, s.admin, orgID, commentID)
}

func deleteReviewComment(ctx context.Context, q queryer, orgID, commentID string) error {
	res, err := q.ExecContext(ctx,
		`DELETE FROM pending_review_comments WHERE org_id = $1 AND id = $2`,
		orgID, commentID,
	)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending comment %s not found", commentID)
	}
	return nil
}

func (s *reviewStore) ListComments(ctx context.Context, orgID, reviewID string) ([]domain.PendingReviewComment, error) {
	return listReviewComments(ctx, s.q, orgID, reviewID)
}

func (s *reviewStore) ListCommentsSystem(ctx context.Context, orgID, reviewID string) ([]domain.PendingReviewComment, error) {
	return listReviewComments(ctx, s.admin, orgID, reviewID)
}

func listReviewComments(ctx context.Context, q queryer, orgID, reviewID string) ([]domain.PendingReviewComment, error) {
	// Order by created_at then id — Postgres has no implicit
	// insertion order like SQLite's rowid. created_at NOT NULL
	// DEFAULT now() makes this stable per-INSERT; the id tiebreaker
	// keeps the order deterministic when two rows land in the same
	// microsecond bucket.
	rows, err := q.QueryContext(ctx, `
		SELECT id, review_id, path, line, start_line, body, original_body, severity
		FROM pending_review_comments
		WHERE org_id = $1 AND review_id = $2
		ORDER BY created_at ASC, id ASC
	`, orgID, reviewID)
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
	var count int
	if err := s.q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pending_review_comments WHERE org_id = $1 AND id = $2`,
		orgID, commentID,
	).Scan(&count); err != nil {
		return false
	}
	return count > 0
}

func pgScanReviewRow(row *sql.Row) (*domain.PendingReview, error) {
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
