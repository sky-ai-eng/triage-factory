package github

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// PRStatus is the live status for a single PR, fetched on demand.
type PRStatus struct {
	Mergeable      *bool             `json:"mergeable"` // null = unknown/calculating
	AutoMerge      bool              `json:"auto_merge"`
	MergeableState string            `json:"mergeable_state"` // "clean", "dirty", "blocked", "behind", "unknown"
	Reviews        []ReviewState     `json:"reviews"`
	ChecksStatus   ChecksStatus      `json:"checks_status"`
	CheckRuns      []domain.CheckRun `json:"check_runs"` // deduped by name, latest execution per check
	Conflicts      bool              `json:"conflicts"`
	ReviewDecision string            `json:"review_decision"` // "approved", "changes_requested", "review_required", ""
}

type ReviewState struct {
	Author      string `json:"author"`
	State       string `json:"state"` // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	SubmittedAt string `json:"submitted_at"`
}

type ChecksStatus struct {
	Total   int `json:"total"`
	Passing int `json:"passing"`
	Failing int `json:"failing"`
	Pending int `json:"pending"`
}

// GetPRStatus fetches the live status for a PR: mergeability, reviews, checks.
func (c *Client) GetPRStatus(owner, repo string, number int) (*PRStatus, error) {
	status := &PRStatus{}

	// 1. PR mergeability (from the PR endpoint)
	prData, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number))
	if err != nil {
		return nil, err
	}
	var pr map[string]any
	if err := json.Unmarshal(prData, &pr); err != nil {
		return nil, fmt.Errorf("parse PR response: %w", err)
	}

	if m, ok := pr["mergeable"].(bool); ok {
		status.Mergeable = &m
	}
	status.AutoMerge = pr["auto_merge"] != nil
	status.MergeableState, _ = pr["mergeable_state"].(string)
	if status.MergeableState == "dirty" {
		status.Conflicts = true
	}

	// 2. Reviews — deduplicate to latest per author
	reviewsData, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews?per_page=100", owner, repo, number))
	if err == nil {
		var rawReviews []map[string]any
		if json.Unmarshal(reviewsData, &rawReviews) == nil {
			// Keep latest review per author
			latest := make(map[string]ReviewState)
			for _, rv := range rawReviews {
				author := ""
				if u, ok := rv["user"].(map[string]any); ok {
					author = strVal(u, "login")
				}
				state := strVal(rv, "state")
				// Skip COMMENTED — only track decision reviews
				if state == "COMMENTED" {
					continue
				}
				latest[author] = ReviewState{
					Author:      author,
					State:       state,
					SubmittedAt: strVal(rv, "submitted_at"),
				}
			}
			for _, rs := range latest {
				status.Reviews = append(status.Reviews, rs)
			}
		}
	}

	// Derive review decision
	status.ReviewDecision = deriveReviewDecision(status.Reviews)

	// 3. Check runs on the head SHA. The endpoint returns every check run
	// recorded against this SHA, including re-runs — the same logical check
	// (by name) can appear multiple times if anyone clicked "re-run failed
	// jobs." We dedup by name keeping the highest ID (monotonic creation
	// order == latest execution) so the display reflects current state, not
	// the full execution history.
	//
	// Check runs are decoded into a typed struct rather than map[string]any
	// because check-run IDs are int64s that can be larger than float64's
	// exact-integer range (2^53). Decoding through an any/float64 path would
	// lose precision and break the ID-based dedup and diff identity.
	status.CheckRuns = []domain.CheckRun{}
	headSHA := ""
	if head, ok := pr["head"].(map[string]any); ok {
		headSHA, _ = head["sha"].(string)
	}
	if headSHA != "" {
		checksData, err := c.Get(fmt.Sprintf("/repos/%s/%s/commits/%s/check-runs?per_page=100", owner, repo, headSHA))
		if err == nil {
			var resp restCheckRunsResponse
			if json.Unmarshal(checksData, &resp) == nil {
				raw := make([]domain.CheckRun, 0, len(resp.CheckRuns))
				for _, run := range resp.CheckRuns {
					raw = append(raw, domain.CheckRun{
						ID:            run.ID,
						Name:          run.Name,
						Status:        strings.ToLower(run.Status),
						Conclusion:    strings.ToLower(run.Conclusion),
						CompletedAt:   run.CompletedAt,
						DetailsURL:    run.DetailsURL,
						WorkflowRunID: parseWorkflowRunIDFromURL(run.DetailsURL),
					})
				}
				status.CheckRuns = domain.DedupCheckRunsByName(raw)
			}
		}
	}
	// Derive aggregate counts from the deduped list so the bar chart reflects
	// one entry per logical check, not per-execution. This is the fix for the
	// "additive Checks display" — before this ticket, re-runs on the same SHA
	// inflated the totals.
	for _, cr := range status.CheckRuns {
		status.ChecksStatus.Total++
		switch {
		case cr.Status != "completed":
			status.ChecksStatus.Pending++
		case domain.IsFailingConclusion(cr.Conclusion):
			status.ChecksStatus.Failing++
		default:
			status.ChecksStatus.Passing++
		}
	}

	return status, nil
}

// restCheckRunsResponse is the typed decoding of GitHub's REST
// /repos/{owner}/{repo}/commits/{ref}/check-runs endpoint. Decoding through a
// typed struct (rather than map[string]any → float64 → int64) keeps check run
// IDs exact — they're int64s that can eventually exceed float64's 2^53
// precision limit, and any loss there would silently break dedup and diff
// identity.
type restCheckRunsResponse struct {
	CheckRuns []restCheckRun `json:"check_runs"`
}

type restCheckRun struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CompletedAt string `json:"completed_at"`
	DetailsURL  string `json:"details_url"`
}

// parseWorkflowRunIDFromURL extracts the GitHub Actions workflow run ID from a
// check-run details URL of the form
//
//	https://github.com/owner/repo/actions/runs/12345/job/67890
//
// Returns 0 for non-Actions check runs (URLs that don't match this pattern —
// e.g. third-party CI systems that post their own details_url). Kept ID-only
// so the log-download ticket has a clean numeric handle to work with.
func parseWorkflowRunIDFromURL(url string) int64 {
	const marker = "/actions/runs/"
	idx := strings.Index(url, marker)
	if idx < 0 {
		return 0
	}
	rest := url[idx+len(marker):]
	end := strings.Index(rest, "/")
	if end < 0 {
		end = len(rest)
	}
	n, err := strconv.ParseInt(rest[:end], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// MarkPRReady marks a draft PR as ready for review. Requires GraphQL.
func (c *Client) MarkPRReady(owner, repo string, number int) error {
	// Get node_id
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number))
	if err != nil {
		return err
	}
	var pr map[string]any
	if err := json.Unmarshal(data, &pr); err != nil {
		return fmt.Errorf("parse PR response: %w", err)
	}
	nodeID := strVal(pr, "node_id")
	if nodeID == "" {
		return fmt.Errorf("could not get node_id for PR %d", number)
	}

	mutation := map[string]any{
		"query": `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`,
		"variables": map[string]any{
			"id": nodeID,
		},
	}
	_, err = c.PostGraphQL(mutation)
	return err
}

// ConvertPRToDraft converts a PR back to draft. Requires GraphQL.
func (c *Client) ConvertPRToDraft(owner, repo string, number int) error {
	// First get the node_id for the PR
	data, err := c.Get(fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, number))
	if err != nil {
		return err
	}
	var pr map[string]any
	if err := json.Unmarshal(data, &pr); err != nil {
		return fmt.Errorf("parse PR response: %w", err)
	}
	nodeID := strVal(pr, "node_id")
	if nodeID == "" {
		return fmt.Errorf("could not get node_id for PR %d", number)
	}

	// GraphQL mutation
	mutation := map[string]any{
		"query": `mutation($id: ID!) { convertPullRequestToDraft(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`,
		"variables": map[string]any{
			"id": nodeID,
		},
	}
	_, err = c.PostGraphQL(mutation)
	return err
}

// SearchUserPRs returns open PRs authored by the given username.
func (c *Client) SearchUserPRs(username string) ([]PRSummary, error) {
	data, err := c.Get(fmt.Sprintf("/search/issues?q=author:%s+type:pr+state:open&per_page=50&sort=updated", username))
	if err != nil {
		return nil, err
	}
	var result struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	prs := make([]PRSummary, 0, len(result.Items))
	for _, item := range result.Items {
		pr := PRSummary{
			Number:    intVal(item, "number"),
			Title:     strVal(item, "title"),
			State:     strVal(item, "state"),
			Draft:     boolVal(item, "draft"),
			CreatedAt: strVal(item, "created_at"),
			UpdatedAt: strVal(item, "updated_at"),
			HTMLURL:   strVal(item, "html_url"),
		}
		if user, ok := item["user"].(map[string]any); ok {
			pr.Author = strVal(user, "login")
		}
		// Extract repo from html_url: https://github.com/owner/repo/pull/N
		pr.Repo = extractRepoFromURL(pr.HTMLURL)

		if labels, ok := item["labels"].([]any); ok {
			for _, l := range labels {
				if label, ok := l.(map[string]any); ok {
					pr.Labels = append(pr.Labels, strVal(label, "name"))
				}
			}
		}

		prs = append(prs, pr)
	}
	return prs, nil
}

type PRSummary struct {
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Repo      string   `json:"repo"`
	Author    string   `json:"author"`
	State     string   `json:"state"`
	Draft     bool     `json:"draft"`
	Labels    []string `json:"labels"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
	HTMLURL   string   `json:"html_url"`
}

func extractRepoFromURL(htmlURL string) string {
	// https://github.com/owner/repo/pull/123
	parts := splitURL(htmlURL)
	if len(parts) >= 5 {
		return parts[len(parts)-4] + "/" + parts[len(parts)-3]
	}
	return ""
}

func splitURL(u string) []string {
	var parts []string
	current := ""
	for _, c := range u {
		if c == '/' {
			if current != "" {
				parts = append(parts, current)
			}
			current = ""
		} else {
			current += string(c)
		}
	}
	if current != "" {
		parts = append(parts, current)
	}
	return parts
}

func deriveReviewDecision(reviews []ReviewState) string {
	hasApproval := false
	for _, r := range reviews {
		if r.State == "CHANGES_REQUESTED" {
			return "changes_requested"
		}
		if r.State == "APPROVED" {
			hasApproval = true
		}
	}
	if hasApproval {
		return "approved"
	}
	if len(reviews) == 0 {
		return "review_required"
	}
	return ""
}
