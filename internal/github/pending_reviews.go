package github

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PendingReviewComment is one inline comment on a GitHub *pending* review,
// addressed by its GraphQL node ID. It carries the fields the review-editor
// sync (TFAC-463 / B·3) needs to reconcile a local draft against the real
// pending review on GitHub: the node ID (for in-place update/delete), the
// anchor (Path + Line, plus StartLine for a multi-line range), and the Body.
//
// Line and StartLine are pointers because GitHub returns them null for a
// comment with no anchor on the current diff (an outdated/unpositioned
// comment). nil therefore means "no current-diff line" — distinct from a real
// line, and not silently collapsed to 0, so a consumer can tell the two apart.
//
// Distinct from ReviewDetailComment (REST, int ID, used for already-submitted
// reviews): every pending-review comment mutation goes through GraphQL, which
// addresses comments by node ID, not the REST integer id.
type PendingReviewComment struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
}

// CreatePendingReview creates a review on a PR left in the PENDING state —
// genuinely private to the authenticated identity until it is submitted (see
// SubmitExistingReview). This is the GitHub-native backing for the
// preview/approval flow (TFAC-454 Sub-epic B): the review editor syncs its
// draft to a real pending review, and approval submits it.
//
// It posts to the same REST endpoint as the atomic SubmitReview but with NO
// "event" field — that omission is exactly what tells GitHub to leave the
// review pending instead of submitting it. commitSHA pins the review to a
// commit (omitted when empty, so GitHub defaults to the PR head); comments
// optionally seed the review with inline threads up front.
//
// Returns the review's GraphQL node ID — the currency every incremental
// pending-review operation here speaks (AddPendingReviewComment,
// SubmitExistingReview, GetPendingReview). GitHub allows only one pending
// review per identity per PR; a duplicate create surfaces a clear error so
// B·3's collision check can fall back to GetPendingReview.
func (c *Client) CreatePendingReview(owner, repo string, number int, commitSHA string, comments []SubmitReviewComment) (string, error) {
	payload := map[string]any{}
	if commitSHA != "" {
		payload["commit_id"] = commitSHA
	}
	if ghComments := reviewCommentsPayload(comments); ghComments != nil {
		payload["comments"] = ghComments
	}

	data, err := c.Post(fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, number), payload)
	if err != nil {
		// Match GitHub's current duplicate-pending wording ("User can only have
		// one pending review per pull request") to point the caller at the
		// recovery path. This is a message-text dependency: if GitHub rewrites the
		// 422, the friendly hint stops firing and the raw (lifted) 422 surfaces
		// instead — still an error, never a silent success, so it degrades safely.
		if strings.Contains(err.Error(), "422") && strings.Contains(strings.ToLower(err.Error()), "one pending review") {
			return "", fmt.Errorf("a pending review already exists on this PR for the authenticated identity — fetch it with GetPendingReview instead of creating a new one")
		}
		return "", liftValidationErr(err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", err
	}
	nodeID := strVal(raw, "node_id")
	if nodeID == "" {
		return "", fmt.Errorf("create pending review: response missing node_id")
	}
	return nodeID, nil
}

// AddPendingReviewComment adds one inline comment thread to an existing pending
// review, addressed by the review's GraphQL node ID (as returned by
// CreatePendingReview / GetPendingReview). Per-comment edits to a *pending*
// review aren't supported by the REST review-comment endpoints, so this uses
// the GraphQL addPullRequestReviewThread mutation (mirroring the GraphQL usage
// in MarkPRReady / ConvertPRToDraft).
//
// The thread is anchored on the RIGHT (post-change) side at comment.Line,
// matching the atomic SubmitReview path, which never sets an explicit side. A
// non-nil comment.StartLine makes it a multi-line range (start..line on the
// same side). Returns the new comment's GraphQL node ID, for a later
// UpdatePendingReviewComment / DeletePendingReviewComment.
func (c *Client) AddPendingReviewComment(reviewID string, comment SubmitReviewComment) (string, error) {
	if reviewID == "" {
		return "", fmt.Errorf("add pending review comment: empty review id")
	}
	// path/body/line are all required for a LINE-anchored thread; fail fast with
	// a pointed message rather than letting GraphQL reject the mutation with a
	// vaguer one (matching the reviewID guard above).
	if comment.Path == "" {
		return "", fmt.Errorf("add pending review comment: empty path")
	}
	if comment.Body == "" {
		return "", fmt.Errorf("add pending review comment: empty body")
	}
	if comment.Line <= 0 {
		return "", fmt.Errorf("add pending review comment: line must be positive, got %d", comment.Line)
	}

	decls := "$reviewId: ID!, $path: String!, $body: String!, $line: Int!"
	inputs := []string{
		"pullRequestReviewId: $reviewId",
		"path: $path",
		"body: $body",
		"line: $line",
		"side: RIGHT",
		"subjectType: LINE",
	}
	vars := map[string]any{
		"reviewId": reviewID,
		"path":     comment.Path,
		"body":     comment.Body,
		"line":     comment.Line,
	}
	if comment.StartLine != nil {
		decls += ", $startLine: Int!"
		inputs = append(inputs, "startLine: $startLine", "startSide: RIGHT")
		vars["startLine"] = *comment.StartLine
	}

	query := fmt.Sprintf(`mutation(%s) {
		addPullRequestReviewThread(input: {%s}) {
			thread { comments(first: 1) { nodes { id } } }
		}
	}`, decls, strings.Join(inputs, ", "))

	data, err := c.PostGraphQL(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return "", err
	}

	var resp struct {
		Data struct {
			AddPullRequestReviewThread struct {
				Thread struct {
					Comments struct {
						Nodes []struct {
							ID string `json:"id"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"thread"`
			} `json:"addPullRequestReviewThread"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("parse addPullRequestReviewThread response: %w", err)
	}
	nodes := resp.Data.AddPullRequestReviewThread.Thread.Comments.Nodes
	if len(nodes) == 0 || nodes[0].ID == "" {
		return "", fmt.Errorf("addPullRequestReviewThread returned no comment id")
	}
	return nodes[0].ID, nil
}

// UpdatePendingReviewComment edits the body of a pending-review comment,
// addressed by its GraphQL node ID (GraphQL updatePullRequestReviewComment).
// Used by the editor⇄pending-review sync to push a body change without
// recreating the thread.
func (c *Client) UpdatePendingReviewComment(commentID, body string) error {
	if commentID == "" {
		return fmt.Errorf("update pending review comment: empty comment id")
	}
	mutation := map[string]any{
		"query": `mutation($id: ID!, $body: String!) {
			updatePullRequestReviewComment(input: {pullRequestReviewCommentId: $id, body: $body}) {
				pullRequestReviewComment { id }
			}
		}`,
		"variables": map[string]any{"id": commentID, "body": body},
	}
	_, err := c.PostGraphQL(mutation)
	return err
}

// DeletePendingReviewComment removes a pending-review comment by its GraphQL
// node ID (GraphQL deletePullRequestReviewComment). Used by the
// editor⇄pending-review sync to drop a comment the user removed locally.
//
// Note the input field is `id` here, unlike updatePullRequestReviewComment's
// `pullRequestReviewCommentId` — GitHub's schema is asymmetric between the two.
func (c *Client) DeletePendingReviewComment(commentID string) error {
	if commentID == "" {
		return fmt.Errorf("delete pending review comment: empty comment id")
	}
	mutation := map[string]any{
		"query": `mutation($id: ID!) {
			deletePullRequestReviewComment(input: {id: $id}) {
				pullRequestReviewComment { id }
			}
		}`,
		"variables": map[string]any{"id": commentID},
	}
	_, err := c.PostGraphQL(mutation)
	return err
}

// SubmitExistingReview submits an already-created pending review (from
// CreatePendingReview), addressed by its GraphQL node ID, via the GraphQL
// submitPullRequestReview mutation. This is the "approve the preview" step: it
// flips a private pending review into a public submitted one. Distinct from the
// atomic SubmitReview, which creates and submits in one call.
//
// event is "APPROVE", "REQUEST_CHANGES", or "COMMENT" (the same vocabulary as
// SubmitReview); body is the optional top-level review body. Unlike
// SubmitReview, this does NOT create the review or its comments (those are
// staged incrementally first) and it does NOT apply the auto-merge
// APPROVE→COMMENT guardrail — the caller (B·3), which holds the PR context,
// owns that.
//
// owner and repo are accepted for symmetry with the other review entry points
// (and a possible future REST fallback); the GraphQL mutation keys solely off
// the review node ID, which is globally unique.
func (c *Client) SubmitExistingReview(owner, repo, reviewID, event, body string) error {
	if reviewID == "" {
		return fmt.Errorf("submit existing review: empty review id")
	}
	if event == "" {
		return fmt.Errorf("submit existing review: empty event (want APPROVE, REQUEST_CHANGES, or COMMENT)")
	}
	mutation := map[string]any{
		"query": `mutation($reviewId: ID!, $event: PullRequestReviewEvent!, $body: String) {
			submitPullRequestReview(input: {pullRequestReviewId: $reviewId, event: $event, body: $body}) {
				pullRequestReview { id state }
			}
		}`,
		"variables": map[string]any{
			"reviewId": reviewID,
			"event":    event,
			"body":     body,
		},
	}
	_, err := c.PostGraphQL(mutation)
	return err
}

// GetPendingReview fetches the authenticated identity's current PENDING review
// on a PR, if any, via GraphQL. A pending review is private to its author, so
// filtering reviews by the PENDING state already scopes results to the viewer;
// the viewerDidAuthor guard is belt-and-suspenders. GitHub allows at most one
// pending review per identity per PR, so the result is effectively single — the
// reviews(first: 50) request plus the loop is only defensive against an
// unexpected non-author PENDING node, not an expectation of many.
//
// Returns ("", nil, nil) when there is no pending review — absence is a normal
// result, not an error (B·3's collision check creates one in that case). A
// GraphQL error propagates instead of masquerading as "no review" — including
// the 200 *partial* shape (usable data plus errors[]) that PostGraphQL passes
// through, e.g. a FORBIDDEN on the reviews field — so the collision check never
// creates a duplicate on top of an access failure.
//
// Otherwise returns the review's GraphQL node ID and its inline comments (each
// carrying its own node ID for incremental edit/delete). Comments are capped at
// 100: this is a correctness boundary for B·3's 1:1 sync, not just a perf cap —
// a pending review with >100 inline comments would omit the oldest, which the
// sync must NOT read as deletions (paginate, or bound the editor, when B·3 lands).
func (c *Client) GetPendingReview(owner, repo string, number int) (string, []PendingReviewComment, error) {
	query := `query($owner: String!, $repo: String!, $number: Int!) {
		repository(owner: $owner, name: $repo) {
			pullRequest(number: $number) {
				reviews(first: 50, states: [PENDING]) {
					nodes {
						id
						viewerDidAuthor
						comments(first: 100) {
							nodes { id path line startLine body }
						}
					}
				}
			}
		}
	}`
	data, err := c.PostGraphQL(map[string]any{
		"query": query,
		"variables": map[string]any{
			"owner":  owner,
			"repo":   repo,
			"number": number,
		},
	})
	if err != nil {
		return "", nil, err
	}

	var resp struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					Reviews struct {
						Nodes []struct {
							ID              string `json:"id"`
							ViewerDidAuthor bool   `json:"viewerDidAuthor"`
							Comments        struct {
								Nodes []struct {
									ID        string `json:"id"`
									Path      string `json:"path"`
									Line      *int   `json:"line"`
									StartLine *int   `json:"startLine"`
									Body      string `json:"body"`
								} `json:"nodes"`
							} `json:"comments"`
						} `json:"nodes"`
					} `json:"reviews"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", nil, fmt.Errorf("parse pending review response: %w", err)
	}
	// Surface a partial GraphQL error before interpreting the data. PostGraphQL
	// returns a 200 partial response (usable data + errors[]) without erroring,
	// so a FORBIDDEN scoped to the reviews field arrives here as empty nodes
	// alongside errors[] — checked first so it can't be silently read as "no
	// pending review". (Total failures with null data already returned above.)
	if len(resp.Errors) > 0 {
		e := resp.Errors[0]
		return "", nil, fmt.Errorf("get pending review: GraphQL error (%s): %s", e.Type, e.Message)
	}
	if resp.Data.Repository == nil || resp.Data.Repository.PullRequest == nil {
		return "", nil, nil
	}

	for _, rv := range resp.Data.Repository.PullRequest.Reviews.Nodes {
		if !rv.ViewerDidAuthor || rv.ID == "" {
			continue
		}
		comments := make([]PendingReviewComment, 0, len(rv.Comments.Nodes))
		for _, cm := range rv.Comments.Nodes {
			comments = append(comments, PendingReviewComment{
				ID:        cm.ID,
				Path:      cm.Path,
				Body:      cm.Body,
				Line:      cm.Line,
				StartLine: cm.StartLine,
			})
		}
		return rv.ID, comments, nil
	}
	return "", nil, nil
}
