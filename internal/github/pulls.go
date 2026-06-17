package github

import (
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// PRView is the compact PR details returned by `gh pr view`.
//
// CloneURL is the head's HTTPS clone URL (the FORK's URL when the PR
// is from a fork). BaseCloneURL is the upstream's HTTPS clone URL —
// by construction the repo where /pulls/<n> lives, which is always
// the upstream. SSHURL and BaseSSHURL are the SSH forms of the same
// URLs (git@github.com:owner/repo.git). Callers (the spawner, mostly)
// pick HTTPS or SSH based on the user's GitHubConfig.CloneProtocol.
// Use the *Base* variants when configuring a bare clone's origin;
// the head URL is only for fork-PR push tracking.
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
	BaseCloneURL string            `json:"base_clone_url"`
	SSHURL       string            `json:"ssh_url"`
	BaseSSHURL   string            `json:"base_ssh_url"`
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
			pr.SSHURL = strVal(headRepo, "ssh_url")
		}
	}
	if base, ok := raw["base"].(map[string]any); ok {
		pr.BaseRef = strVal(base, "ref")
		if baseRepo, ok := base["repo"].(map[string]any); ok {
			pr.BaseCloneURL = strVal(baseRepo, "clone_url")
			pr.BaseSSHURL = strVal(baseRepo, "ssh_url")
		}
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
	Filename string `json:"filename"`
	Status   string `json:"status"` // added, modified, removed, renamed
	// PreviousFilename is the pre-rename path; GitHub sets it only when
	// Status is "renamed". Empty for every other status.
	PreviousFilename string `json:"previous_filename,omitempty"`
	Additions        int    `json:"additions"`
	Deletions        int    `json:"deletions"`
	// Patch is the unified diff hunks for the file. GitHub omits it for
	// binary files and also for oversized/truncated diffs — so an empty Patch
	// does not by itself mean the file is binary (see isBinaryFile, which also
	// checks the line counts).
	Patch string `json:"patch,omitempty"`
}

// MaxPRFiles caps the total number of files fetched by GetPRFiles across all
// pages. Exported so callers can detect a truncated listing (len(files) >=
// MaxPRFiles means GetPRFiles hit the cap and the result may be incomplete).
const MaxPRFiles = 1000

// GetPRFiles lists files changed in a PR, including per-file patch content.
// Paginates up to MaxPRFiles total results.
func (c *Client) GetPRFiles(owner, repo string, number int) ([]PRFile, error) {
	var files []PRFile
	for page := 1; ; page++ {
		data, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=100&page=%d", owner, repo, number, page))
		if err != nil {
			return nil, err
		}

		var rawFiles []map[string]any
		if err := json.Unmarshal(data, &rawFiles); err != nil {
			return nil, err
		}

		for _, f := range rawFiles {
			files = append(files, PRFile{
				Filename:         strVal(f, "filename"),
				Status:           strVal(f, "status"),
				PreviousFilename: strVal(f, "previous_filename"),
				Additions:        intVal(f, "additions"),
				Deletions:        intVal(f, "deletions"),
				Patch:            strVal(f, "patch"),
			})
		}

		if len(rawFiles) < 100 || len(files) >= MaxPRFiles {
			break
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
		// Backstop: pre-submit validation in cmd/exec/gh should prevent
		// cross-hunk multi-line comments, but if the PR was force-pushed
		// after start-review the captured hunks are stale and we'd land
		// here. The fix is the same in both cases — edit/delete the
		// offending comment, or restart the review against the current
		// diff.
		if strings.Contains(errStr, "must be part of the same hunk") {
			return 0, event, fmt.Errorf(
				"a pending review comment has a multi-line range that crosses a diff hunk boundary — " +
					"GitHub requires start_line and line to be in the same hunk. " +
					"Edit or delete the offending comment, then resubmit. If the PR was force-pushed since the review was started, " +
					"start a new review so the captured hunks match the current diff",
			)
		}
		// Unclassified review-submit failure. Log the request shape so
		// mystery 422s (e.g., "thread end commit oid is not part of the
		// pull request") can be diagnosed against the PR's /commits list
		// and live diff without re-running the agent.
		positions := make([]string, len(comments))
		for i, cm := range comments {
			if cm.StartLine != nil {
				positions[i] = fmt.Sprintf("%s:%d-%d", cm.Path, *cm.StartLine, cm.Line)
			} else {
				positions[i] = fmt.Sprintf("%s:%d", cm.Path, cm.Line)
			}
		}
		log.Printf("[github] SubmitReview failed %s/%s#%d commit=%s event=%s comments=%d [%s]: %v",
			owner, repo, number, commitSHA, event, len(comments), strings.Join(positions, ", "), err)
		return 0, event, err
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, event, err
	}

	return intVal(raw, "id"), event, nil
}

// CreatePR opens a new pull request on the upstream repo. head is the
// branch the agent has already pushed (the upstream must already know
// about it — git push must precede this call). base is the merge
// target. draft=true creates a draft PR.
//
// Returns (number, htmlURL, err). On a 422, the GitHub validation
// payload is parsed and the per-error "message" / "code"+"field" is
// folded into the returned error so callers see "Validation Failed:
// base 'develop' is not a valid branch" rather than the raw JSON
// blob — the retry flow is much more useful when the actual reason
// is visible.
func (c *Client) CreatePR(owner, repo, head, base, title, body string, draft bool) (int, string, error) {
	payload := map[string]any{
		"title": title,
		"head":  head,
		"base":  base,
		"body":  body,
		"draft": draft,
	}

	data, err := c.Post(fmt.Sprintf("/repos/%s/%s/pulls", owner, repo), payload)
	if err != nil {
		return 0, "", liftValidationErr(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, "", err
	}

	return intVal(raw, "number"), strVal(raw, "html_url"), nil
}

// liftValidationErr extracts a useful message from a GitHub error
// returned by client.do. The original error string has the shape
// "POST /path returned NNN: {raw JSON body}"; for 422s the body
// follows GitHub's validation envelope:
//
//	{
//	  "message": "Validation Failed",
//	  "errors": [
//	    {"resource": "PullRequest", "code": "invalid", "field": "base"},
//	    {"resource": "PullRequest", "code": "custom",
//	     "message": "No commits between main and feature/X"}
//	  ]
//	}
//
// Falls back to the original error verbatim when the body isn't
// parseable as JSON or doesn't match the envelope (other 4xx/5xx
// shapes go straight through unchanged).
func liftValidationErr(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	idx := strings.Index(s, ": ")
	if idx == -1 {
		return err
	}
	body := s[idx+2:]
	var parsed struct {
		Message string `json:"message"`
		Errors  []struct {
			Resource string `json:"resource"`
			Code     string `json:"code"`
			Field    string `json:"field"`
			Message  string `json:"message"`
		} `json:"errors"`
	}
	if jsonErr := json.Unmarshal([]byte(body), &parsed); jsonErr != nil {
		return err
	}
	if parsed.Message == "" && len(parsed.Errors) == 0 {
		return err
	}
	var detail strings.Builder
	if parsed.Message != "" {
		detail.WriteString(parsed.Message)
	}
	for _, e := range parsed.Errors {
		detail.WriteString(": ")
		switch {
		case e.Message != "":
			detail.WriteString(e.Message)
		case e.Field != "":
			fmt.Fprintf(&detail, "%s field '%s'", e.Code, e.Field)
		default:
			detail.WriteString(e.Code)
		}
	}
	return fmt.Errorf("%s", detail.String())
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

// restPR is the subset of the REST pull-request object (GET
// /repos/{o}/{r}/pulls) the poller's discovery path consumes. The REST
// object carries node_id, which composes directly with the GraphQL Phase-2
// refresh (RefreshPRs takes node IDs). Fields mirror what
// prDiscoveryFragment produces so both discovery paths feed the same
// snapshot shape into the gate (updated_at + head.sha) and entity seeding.
type restPR struct {
	Number    int    `json:"number"`
	NodeID    string `json:"node_id"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Draft     bool   `json:"draft"`
	HTMLURL   string `json:"html_url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	RequestedReviewers []struct {
		Login string `json:"login"`
	} `json:"requested_reviewers"`
	RequestedTeams []struct {
		Slug string `json:"slug"`
	} `json:"requested_teams"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// ListOpenPRs lists a repo's open PRs via REST with a conditional request,
// returning them as DiscoveredPRs (node ID + discovery snapshot) ready to
// feed the existing two-phase tracker (Phase-2 GraphQL refresh keys on the
// node IDs). It is the App/PAT-parity replacement for user-perspective
// GraphQL search discovery: reach differs by what the token can see, the
// mechanism is identical.
//
// etag is the caller's stored ETag for this repo's open-PR listing.
//   - 304 → (nil, "", true, nil): the open set is unchanged; the caller
//     skips discovery for this repo this cycle (free on the primary limit).
//   - 200 → (prs, newEtag, false, nil): the caller stores newEtag and
//     processes the listing.
//
// The ETag fast-path is single-page: page 1 carries the conditional header
// (default sort created desc, so a newly opened PR always changes page 1),
// and on a 200 any further pages are listed unconditionally. >100 open PRs
// therefore re-list fully on each change; the Phase-2 refresh remains the
// correctness backstop for state changes on already-tracked entities.
func (c *Client) ListOpenPRs(owner, repo, etag string) ([]DiscoveredPR, string, bool, error) {
	first := fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=100&page=1", owner, repo)
	body, newEtag, notModified, err := c.GetConditional(first, etag)
	if err != nil {
		return nil, "", false, err
	}
	if notModified {
		return nil, "", true, nil
	}

	var out []DiscoveredPR
	page1, err := parseOpenPRs(owner, repo, body)
	if err != nil {
		return nil, "", false, err
	}
	out = append(out, page1...)

	// Single-page fast path: fewer than a full page means no more pages.
	for page := 2; len(out) > 0 && len(out)%100 == 0; page++ {
		data, perr := c.Get(fmt.Sprintf("/repos/%s/%s/pulls?state=open&per_page=100&page=%d", owner, repo, page))
		if perr != nil {
			return nil, "", false, perr
		}
		more, perr := parseOpenPRs(owner, repo, data)
		if perr != nil {
			return nil, "", false, perr
		}
		if len(more) == 0 {
			break
		}
		out = append(out, more...)
	}

	return out, newEtag, false, nil
}

// parseOpenPRs decodes a REST pulls listing page into DiscoveredPRs.
func parseOpenPRs(owner, repo string, data []byte) ([]DiscoveredPR, error) {
	var raw []restPR
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse open PRs for %s/%s: %w", owner, repo, err)
	}
	out := make([]DiscoveredPR, 0, len(raw))
	for _, pr := range raw {
		if pr.Number == 0 {
			continue
		}
		out = append(out, DiscoveredPR{
			NodeID:   pr.NodeID,
			Snapshot: pr.toDiscoverySnapshot(owner, repo),
		})
	}
	return out, nil
}

// toDiscoverySnapshot maps a REST PR object to a discovery snapshot. Mirrors
// gqlPR.toDiscoverySnapshot: CheckRuns stays nil ("unknown prior state") so
// the diff layer skips CI evaluation until the Phase-2 refresh fills it in.
// Review-request team reviewers are emitted as "owner/slug" — a repo's
// requested team always belongs to the repo's owning org, so owner is the
// org login, matching the "org/slug" form GraphQL discovery and
// GET /user/teams produce.
func (pr restPR) toDiscoverySnapshot(owner, repo string) domain.PRSnapshot {
	snap := domain.PRSnapshot{
		NodeID:    pr.NodeID,
		Number:    pr.Number,
		Title:     pr.Title,
		Author:    pr.User.Login,
		Repo:      owner + "/" + repo,
		URL:       pr.HTMLURL,
		State:     strings.ToUpper(pr.State), // REST "open" → "OPEN" (GraphQL form)
		IsDraft:   pr.Draft,
		HeadRef:   pr.Head.Ref,
		BaseRef:   pr.Base.Ref,
		HeadSHA:   pr.Head.SHA,
		CreatedAt: pr.CreatedAt,
		UpdatedAt: pr.UpdatedAt,
	}
	if pr.Head.Repo != nil {
		snap.HeadRepo = pr.Head.Repo.FullName
	}
	for _, rr := range pr.RequestedReviewers {
		if rr.Login != "" {
			snap.ReviewRequests = append(snap.ReviewRequests, rr.Login)
		}
	}
	for _, tm := range pr.RequestedTeams {
		if tm.Slug != "" {
			snap.ReviewRequests = append(snap.ReviewRequests, owner+"/"+tm.Slug)
		}
	}
	for _, l := range pr.Labels {
		snap.Labels = append(snap.Labels, l.Name)
	}
	return snap
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
//
// Matches the new-side path exactly (the token after " b/" on the
// "diff --git a/… b/…" header) rather than a substring of the whole header.
// A substring check would misfire on paths that merely contain the requested
// name (e.g. asking for "foo.go" capturing "lib/b/foo.go"), and would be
// asymmetric with the 406 fallback's exact match. Renamed files are keyed on
// their new (b-side) path, which is what callers reference.
func filterDiffByFile(diff, file string) string {
	lines := strings.Split(diff, "\n")
	var result []string
	capturing := false
	for _, line := range lines {
		if strings.HasPrefix(line, "diff --git") {
			if capturing {
				break
			}
			if parts := strings.SplitN(line, " b/", 2); len(parts) == 2 && parts[1] == file {
				capturing = true
			}
		}
		if capturing {
			result = append(result, line)
		}
	}
	return strings.Join(result, "\n")
}
