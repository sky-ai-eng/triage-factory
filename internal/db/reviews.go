package db

import (
	"database/sql"
	"fmt"

	"github.com/sky-ai-eng/todo-triage/internal/domain"
)


func CreatePendingReview(database *sql.DB, r domain.PendingReview) error {
	_, err := database.Exec(
		`INSERT INTO pending_reviews (id, pr_number, owner, repo, commit_sha, diff_lines, run_id) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.PRNumber, r.Owner, r.Repo, r.CommitSHA, r.DiffLines, r.RunID,
	)
	return err
}

func GetPendingReview(database *sql.DB, reviewID string) (*domain.PendingReview, error) {
	row := database.QueryRow(`SELECT id, pr_number, owner, repo, commit_sha, COALESCE(diff_lines, ''), COALESCE(run_id, ''), COALESCE(review_body, ''), COALESCE(review_event, '') FROM pending_reviews WHERE id = ?`, reviewID)
	var r domain.PendingReview
	err := row.Scan(&r.ID, &r.PRNumber, &r.Owner, &r.Repo, &r.CommitSHA, &r.DiffLines, &r.RunID, &r.ReviewBody, &r.ReviewEvent)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &r, err
}

func AddPendingReviewComment(database *sql.DB, c domain.PendingReviewComment) error {
	_, err := database.Exec(
		`INSERT INTO pending_review_comments (id, review_id, path, line, start_line, body) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.ReviewID, c.Path, c.Line, c.StartLine, c.Body,
	)
	return err
}

func UpdatePendingReviewComment(database *sql.DB, commentID, body string) error {
	result, err := database.Exec(`UPDATE pending_review_comments SET body = ? WHERE id = ?`, body, commentID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending comment %s not found", commentID)
	}
	return nil
}

func DeletePendingReviewComment(database *sql.DB, commentID string) error {
	result, err := database.Exec(`DELETE FROM pending_review_comments WHERE id = ?`, commentID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("pending comment %s not found", commentID)
	}
	return nil
}

func ListPendingReviewComments(database *sql.DB, reviewID string) ([]domain.PendingReviewComment, error) {
	rows, err := database.Query(
		`SELECT id, review_id, path, line, start_line, body FROM pending_review_comments WHERE review_id = ? ORDER BY rowid`,
		reviewID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var comments []domain.PendingReviewComment
	for rows.Next() {
		var c domain.PendingReviewComment
		var startLine sql.NullInt64
		if err := rows.Scan(&c.ID, &c.ReviewID, &c.Path, &c.Line, &startLine, &c.Body); err != nil {
			return nil, err
		}
		if startLine.Valid {
			v := int(startLine.Int64)
			c.StartLine = &v
		}
		comments = append(comments, c)
	}
	return comments, rows.Err()
}

// DeletePendingReview removes a review and all its comments (on submit or cancel).
func DeletePendingReview(database *sql.DB, reviewID string) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM pending_review_comments WHERE review_id = ?`, reviewID); err != nil {
		return fmt.Errorf("delete review comments: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM pending_reviews WHERE id = ?`, reviewID); err != nil {
		return fmt.Errorf("delete review: %w", err)
	}
	return tx.Commit()
}

// SetPendingReviewSubmission stores the deferred review body and event, marking
// the review as ready for user approval rather than immediate GitHub submission.
func SetPendingReviewSubmission(database *sql.DB, reviewID, body, event string) error {
	_, err := database.Exec(
		`UPDATE pending_reviews SET review_body = ?, review_event = ? WHERE id = ?`,
		body, event, reviewID,
	)
	return err
}

// PendingReviewByRunID returns the pending review associated with a given agent
// run that has a deferred submission (review_event is set). Returns nil if none.
func PendingReviewByRunID(database *sql.DB, runID string) (*domain.PendingReview, error) {
	row := database.QueryRow(
		`SELECT id, pr_number, owner, repo, commit_sha, COALESCE(diff_lines, ''), COALESCE(run_id, ''), COALESCE(review_body, ''), COALESCE(review_event, '')
		 FROM pending_reviews WHERE run_id = ? AND review_event IS NOT NULL AND review_event != ''`, runID)
	var r domain.PendingReview
	err := row.Scan(&r.ID, &r.PRNumber, &r.Owner, &r.Repo, &r.CommitSHA, &r.DiffLines, &r.RunID, &r.ReviewBody, &r.ReviewEvent)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &r, err
}

// IsPendingCommentID checks if a comment ID belongs to a local pending review.
func IsPendingCommentID(database *sql.DB, commentID string) bool {
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pending_review_comments WHERE id = ?`, commentID).Scan(&count); err != nil {
		return false
	}
	return count > 0
}
