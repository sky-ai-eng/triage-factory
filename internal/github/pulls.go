package github

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// PRView is the compact PR details returned by `gh pr view`.
type PRView struct {
	Number       int               `json:"number"`
	Title        string            `json:"title"`
	Body         string            `json:"body"`
	State        string            `json:"state"`
	Merged       bool              `json:"merged"`
	AutoMerge    bool              `json:"auto_merge"`
	Author       string            `json:"author"`
	Additions    int               `json:"additions"`
	Deletions    int               `json:"deletions"`
	ChangedFiles int               `json:"changed_files"`
	HeadRef      string            `json:"head_ref"`
	BaseRef      string            `json:"base_ref"`
	HeadSHA      string            `json:"head_sha"`
	HTMLURL      string            `json:"html_url"`
	CloneURL     string            `json:"clone_url"`
	CreatedAt    string            `json:"created_at"`
	UpdatedAt    string            `json:"updated_at"`
	Reviews      []PRReviewSummary `json:"reviews"`
	Comments     []PRTopComment    `json:"comments"`
}

// PRReviewSummary is a compact view of a review shown in `pr view`.
type PRReviewSummary struct {
	ID           int    `json:"id"`
	Author       string `json:"author"`
	State        string `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	Body         string `json:"body"`
	CommentCount int    `json:"comment_count"`
	SubmittedAt  string `json:"submitted_at"`
}

// PRTopComment is a top-level issue comment with reply count.
type PRTopComment struct {
	ID        int    `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// GetPR fetches PR details including top-level comments.
// In compact mode (verbose=false), review/comment bodies are truncated to first line.
func (c *Client) GetPR(owner, repo string, number int, verbose bool) (*PRView, error) {
	// Fetch PR
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number))
	if err != nil {
		return nil, err
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	pr := &PRView{
		Number:       intVal(raw, "number"),
		Title:        strVal(raw, "title"),
		Body:         strVal(raw, "body"),
		State:        strVal(raw, "state"),
		Merged:       boolVal(raw, "merged"),
		AutoMerge:    raw["auto_merge"] != nil,
		Additions:    intVal(raw, "additions"),
		Deletions:    intVal(raw, "deletions"),
		ChangedFiles: intVal(raw, "changed_files"),
		HTMLURL:      strVal(raw, "html_url"),
		CreatedAt:    strVal(raw, "created_at"),
		UpdatedAt:    strVal(raw, "updated_at"),
	}

	if user, ok := raw["user"].(map[string]any); ok {
		pr.Author = strVal(user, "login")
	}
	if head, ok := raw["head"].(map[string]any); ok {
		pr.HeadRef = strVal(head, "ref")
		pr.HeadSHA = strVal(head, "sha")
		if headRepo, ok := head["repo"].(map[string]any); ok {
			pr.CloneURL = strVal(headRepo, "clone_url")
		}
	}
	if base, ok := raw["base"].(map[string]any); ok {
		pr.BaseRef = strVal(base, "ref")
	}

	// Fetch reviews
	reviewsData, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", owner, repo, number))
	if err == nil {
		var rawReviews []map[string]any
		if json.Unmarshal(reviewsData, &rawReviews) == nil {
			// Count comments per review
			commentCounts := make(map[int]int)
			reviewCommentsData, rcErr := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?per_page=100", owner, repo, number))
			if rcErr == nil {
				var rawRC []map[string]any
				if json.Unmarshal(reviewCommentsData, &rawRC) == nil {
					for _, rc := range rawRC {
						rid := intVal(rc, "pull_request_review_id")
						if rid > 0 {
							commentCounts[rid]++
						}
					}
				}
			}

			for _, rv := range rawReviews {
				author := ""
				if u, ok := rv["user"].(map[string]any); ok {
					author = strVal(u, "login")
				}
				rid := intVal(rv, "id")
				body := strVal(rv, "body")
				if !verbose {
					body = firstLine(body)
				}
				pr.Reviews = append(pr.Reviews, PRReviewSummary{
					ID:           rid,
					Author:       author,
					State:        strVal(rv, "state"),
					Body:         body,
					CommentCount: commentCounts[rid],
					SubmittedAt:  strVal(rv, "submitted_at"),
				})
			}
		}
	}

	// Fetch issue comments (top-level)
	commentsData, err := c.Get(fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, number))
	if err == nil {
		var rawComments []map[string]any
		if json.Unmarshal(commentsData, &rawComments) == nil {
			for _, rc := range rawComments {
				author := ""
				if u, ok := rc["user"].(map[string]any); ok {
					author = strVal(u, "login")
				}
				body := strVal(rc, "body")
				if !verbose {
					body = firstLine(body)
				}
				pr.Comments = append(pr.Comments, PRTopComment{
					ID:        intVal(rc, "id"),
					Author:    author,
					Body:      body,
					CreatedAt: strVal(rc, "created_at"),
				})
			}
		}
	}

	return pr, nil
}

// PRFile is a file changed in a PR.
type PRFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"` // added, modified, removed, renamed
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// GetPRFiles lists files changed in a PR.
func (c *Client) GetPRFiles(owner, repo string, number int) ([]PRFile, error) {
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100", owner, repo, number))
	if err != nil {
		return nil, err
	}

	var rawFiles []map[string]any
	if err := json.Unmarshal(data, &rawFiles); err != nil {
		return nil, err
	}

	files := make([]PRFile, len(rawFiles))
	for i, f := range rawFiles {
		files[i] = PRFile{
			Filename:  strVal(f, "filename"),
			Status:    strVal(f, "status"),
			Additions: intVal(f, "additions"),
			Deletions: intVal(f, "deletions"),
		}
	}
	return files, nil
}

// GetPRDiff fetches the raw diff for a PR, optionally filtered to a single file.
func (c *Client) GetPRDiff(owner, repo string, number int, file string) (string, error) {
	diff, err := c.GetRaw(fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number), "application/vnd.github.v3.diff")
	if err != nil {
		return "", err
	}

	if file == "" {
		return string(diff), nil
	}

	// Filter to the requested file
	return filterDiffByFile(string(diff), file), nil
}

// CommentThread is a top-level comment with its replies.
type CommentThread struct {
	ID        int           `json:"id"`
	Author    string        `json:"author"`
	Body      string        `json:"body"`
	CreatedAt string        `json:"created_at"`
	Replies   []ThreadReply `json:"replies"`
	Page      int           `json:"page"`
	HasMore   bool          `json:"has_more"`
}

type ThreadReply struct {
	ID        int    `json:"id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// GetCommentThread fetches a comment and its replies. For issue comments,
// there are no threaded replies in the REST API, so we return just the comment.
// For review comments, we fetch the thread.
func (c *Client) GetCommentThread(owner, repo string, commentID, page int) (*CommentThread, error) {
	// Try as issue comment first
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, repo, commentID))
	if err == nil {
		var raw map[string]any
		if json.Unmarshal(data, &raw) == nil {
			author := ""
			if u, ok := raw["user"].(map[string]any); ok {
				author = strVal(u, "login")
			}
			return &CommentThread{
				ID:        intVal(raw, "id"),
				Author:    author,
				Body:      strVal(raw, "body"),
				CreatedAt: strVal(raw, "created_at"),
				Page:      1,
			}, nil
		}
	}

	return nil, fmt.Errorf("comment %d not found", commentID)
}

// ReviewDetail is the expanded view of a single review with all its inline comments.
type ReviewDetail struct {
	ID          int                   `json:"id"`
	Author      string                `json:"author"`
	State       string                `json:"state"`
	Body        string                `json:"body"`
	SubmittedAt string                `json:"submitted_at"`
	Comments    []ReviewDetailComment `json:"comments"`
}

type ReviewDetailComment struct {
	ID        int    `json:"id"`
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

// GetReviewDetail fetches a review and all its inline comments.
// In compact mode, comment bodies are truncated to first line.
func (c *Client) GetReviewDetail(owner, repo string, number, reviewID int, verbose bool) (*ReviewDetail, error) {
	// Fetch the review itself
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d", owner, repo, number, reviewID))
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	author := ""
	if u, ok := raw["user"].(map[string]any); ok {
		author = strVal(u, "login")
	}

	detail := &ReviewDetail{
		ID:          intVal(raw, "id"),
		Author:      author,
		State:       strVal(raw, "state"),
		Body:        strVal(raw, "body"),
		SubmittedAt: strVal(raw, "submitted_at"),
	}

	// Fetch comments for this review
	commentsData, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d/comments?per_page=100", owner, repo, number, reviewID))
	if err == nil {
		var rawComments []map[string]any
		if json.Unmarshal(commentsData, &rawComments) == nil {
			for _, rc := range rawComments {
				commentAuthor := ""
				if u, ok := rc["user"].(map[string]any); ok {
					commentAuthor = strVal(u, "login")
				}
				body := strVal(rc, "body")
				if !verbose {
					body = firstLine(body)
				}
				comment := ReviewDetailComment{
					ID:        intVal(rc, "id"),
					Path:      strVal(rc, "path"),
					Line:      intVal(rc, "line"),
					Body:      body,
					Author:    commentAuthor,
					CreatedAt: strVal(rc, "created_at"),
				}
				if sl := intVal(rc, "start_line"); sl > 0 {
					comment.StartLine = &sl
				}
				detail.Comments = append(detail.Comments, comment)
			}
		}
	}

	return detail, nil
}

// SubmitReviewInput is a comment to include when submitting a review.
type SubmitReviewComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
}

// SubmitReview creates and submits a review atomically with all comments in one API call.
// Event is "APPROVE", "REQUEST_CHANGES", or "COMMENT".
// If auto_merge is enabled on the PR and event is "APPROVE", it's downgraded to "COMMENT".
func (c *Client) SubmitReview(owner, repo string, number int, commitSHA, event, body string, comments []SubmitReviewComment) (int, string, error) {
	// Check auto_merge guardrail
	if event == "APPROVE" {
		pr, err := c.GetPR(owner, repo, number, false)
		if err == nil && pr.AutoMerge {
			event = "COMMENT"
			body = "[Auto-merge is enabled — downgraded from APPROVE to COMMENT]\n\n" + body
		}
	}

	payload := map[string]any{
		"commit_id": commitSHA,
		"event":     event,
		"body":      body,
	}

	if len(comments) > 0 {
		ghComments := make([]map[string]any, len(comments))
		for i, c := range comments {
			m := map[string]any{
				"path": c.Path,
				"line": c.Line,
				"body": c.Body,
			}
			if c.StartLine != nil {
				m["start_line"] = *c.StartLine
			}
			ghComments[i] = m
		}
		payload["comments"] = ghComments
	}

	data, err := c.Post(fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, number), payload)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "422") && event == "REQUEST_CHANGES" {
			return 0, event, fmt.Errorf("cannot request changes on your own pull request — change the review type to Comment")
		}
		return 0, event, err
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, event, err
	}

	return intVal(raw, "id"), event, nil
}

// DismissReview dismisses a submitted review (removes approval/change-request status).
// Only works on APPROVED or CHANGES_REQUESTED reviews — COMMENTED reviews cannot be dismissed.
func (c *Client) DismissReview(owner, repo string, number, reviewID int, message string) error {
	_, err := c.Put(fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews/%d/dismissals", owner, repo, number, reviewID), map[string]any{
		"message": message,
	})
	if err != nil && strings.Contains(err.Error(), "422") {
		return fmt.Errorf("cannot dismiss this review — only APPROVED or CHANGES_REQUESTED reviews can be dismissed (COMMENTED reviews are permanent)")
	}
	return err
}

// AddComment adds a top-level issue comment to a PR.
func (c *Client) AddComment(owner, repo string, number int, body string) (int, error) {
	data, err := c.Post(fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, number), map[string]any{
		"body": body,
	})
	if err != nil {
		return 0, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, err
	}
	return intVal(raw, "id"), nil
}

// ReplyToComment replies to a review comment thread.
func (c *Client) ReplyToComment(owner, repo string, number, commentID int, body string) (int, error) {
	data, err := c.Post(fmt.Sprintf("/repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, number, commentID), map[string]any{
		"body": body,
	})
	if err != nil {
		return 0, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, err
	}
	return intVal(raw, "id"), nil
}

// ReactToComment adds a reaction to a comment. Tries issue comments first, then review comments.
func (c *Client) ReactToComment(owner, repo string, commentID int, emoji string) error {
	body := map[string]any{"content": emoji}
	// Try issue comment
	_, err := c.Post(fmt.Sprintf("/repos/%s/%s/issues/comments/%d/reactions", owner, repo, commentID), body)
	if err == nil {
		return nil
	}
	// Try review comment
	_, err = c.Post(fmt.Sprintf("/repos/%s/%s/pulls/comments/%d/reactions", owner, repo, commentID), body)
	return err
}

// UpdateComment updates a comment body. Tries issue comments first, then review comments.
func (c *Client) UpdateComment(owner, repo string, commentID int, body string) error {
	payload := map[string]any{"body": body}
	_, err := c.Patch(fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, repo, commentID), payload)
	if err == nil {
		return nil
	}
	_, err = c.Patch(fmt.Sprintf("/repos/%s/%s/pulls/comments/%d", owner, repo, commentID), payload)
	return err
}

// DeleteComment deletes a comment. Tries issue comments first, then review comments.
func (c *Client) DeleteComment(owner, repo string, commentID int) error {
	_, err := c.Delete(fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, repo, commentID))
	if err == nil {
		return nil
	}
	_, err = c.Delete(fmt.Sprintf("/repos/%s/%s/pulls/comments/%d", owner, repo, commentID))
	return err
}

// SearchReviewRequested searches for open PRs requesting the user's review.
func (c *Client) SearchReviewRequested(username string) ([]map[string]any, error) {
	q := url.QueryEscape(fmt.Sprintf("review-requested:%s type:pr state:open", username))
	data, err := c.Get(fmt.Sprintf("/search/issues?q=%s&per_page=50", q))
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result.Items, nil
}

// --- helpers ---

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func intVal(m map[string]any, key string) int {
	if v, ok := m[key].(float64); ok {
		return int(v)
	}
	return 0
}

func boolVal(m map[string]any, key string) bool {
	if v, ok := m[key].(bool); ok {
		return v
	}
	return false
}

// firstLine returns the first non-empty line of s, truncated to 120 chars.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "---" {
			continue
		}
		if len(line) > 120 {
			return line[:117] + "..."
		}
		return line
	}
	return ""
}

// filterDiffByFile extracts the diff section for a single file.
func filterDiffByFile(diff, file string) string {
	lines := strings.Split(diff, "\n")
	var result []string
	capturing := false
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git") {
			if capturing {
				break
			}
			if strings.Contains(line, "b/"+file) {
				capturing = true
			}
		}
		if capturing {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}
