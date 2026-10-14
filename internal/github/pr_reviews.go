package github

import (
	"context"
	"encoding/json"
	"fmt"
)

// reviewPageSize is how many submitted reviews one read of a pull request's
// reviews asks for.
const reviewPageSize = 50

// reviewCommentCap is how many of a review's inline comments the read returns.
// PRReview.CommentCount is the review's full count either way.
const reviewCommentCap = 100

var prReviewsQuery = fmt.Sprintf(`
	query($id: ID!, $before: String) {
		node(id: $id) {
			... on PullRequest {
				reviews(last: %d, before: $before, states: [%s]) {
					totalCount
					pageInfo { hasPreviousPage startCursor }
					nodes {
						fullDatabaseId
						state
						submittedAt
						author { __typename login }
						comments(first: %d) {
							totalCount
							nodes { fullDatabaseId path }
						}
					}
				}
			}
		}
	}
`, reviewPageSize, submittedReviewStates, reviewCommentCap)

// PRReview is a submitted review on a pull request.
type PRReview struct {
	ID          int64
	State       string // APPROVED, CHANGES_REQUESTED, COMMENTED or DISMISSED
	Author      string // REST-spelled login; empty when GitHub names no author
	SubmittedAt string
	// CommentCount is how many inline comments the review holds. A reply in a
	// review thread posted outside a pending review is a review of its own,
	// holding the reply.
	CommentCount int
	// Comments are the first reviewCommentCap of the review's inline comments.
	Comments []PRReviewComment
}

// PRReviewComment is an inline comment in a review: its numeric id, in the
// numbering /pulls/comments/{id} addresses, and the file it is on.
type PRReviewComment struct {
	ID   int64
	Path string
}

// PRReviewPage is one page of a pull request's submitted reviews, in the order
// GitHub lists them.
type PRReviewPage struct {
	Reviews []PRReview
	// TotalCount is how many submitted reviews the pull request has.
	TotalCount int
	// HasPrevious reports whether reviews precede this page, and Before is the
	// cursor that reads them.
	HasPrevious bool
	Before      string
}

// PRReviewsBefore reads the page of submitted reviews just before cursor
// before on the pull request whose GraphQL node id is prNodeID, or the last
// page when before is empty. Pending reviews are never returned.
func (c *Client) PRReviewsBefore(ctx context.Context, prNodeID, before string) (PRReviewPage, error) {
	vars := map[string]any{"id": prNodeID, "before": nil}
	if before != "" {
		vars["before"] = before
	}
	data, err := c.PostGraphQL(ctx, map[string]any{"query": prReviewsQuery, "variables": vars})
	if err != nil {
		return PRReviewPage{}, fmt.Errorf("read reviews: %w", err)
	}

	var resp struct {
		Data struct {
			Node *struct {
				Reviews *struct {
					TotalCount int `json:"totalCount"`
					PageInfo   struct {
						HasPreviousPage bool   `json:"hasPreviousPage"`
						StartCursor     string `json:"startCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						DatabaseID  gqlBigInt `json:"fullDatabaseId"`
						State       string    `json:"state"`
						SubmittedAt string    `json:"submittedAt"`
						Author      gqlAuthor `json:"author"`
						Comments    struct {
							TotalCount int `json:"totalCount"`
							Nodes      []struct {
								DatabaseID gqlBigInt `json:"fullDatabaseId"`
								Path       string    `json:"path"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviews"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return PRReviewPage{}, fmt.Errorf("parse reviews response: %w", err)
	}
	if resp.Data.Node == nil || resp.Data.Node.Reviews == nil {
		return PRReviewPage{}, fmt.Errorf("read reviews: pull request %s not found", prNodeID)
	}

	conn := resp.Data.Node.Reviews
	page := PRReviewPage{
		TotalCount:  conn.TotalCount,
		HasPrevious: conn.PageInfo.HasPreviousPage,
		Before:      conn.PageInfo.StartCursor,
	}
	for _, n := range conn.Nodes {
		if n.State == "PENDING" {
			continue
		}
		r := PRReview{
			ID:           int64(n.DatabaseID),
			State:        n.State,
			Author:       n.Author.restLogin(),
			SubmittedAt:  n.SubmittedAt,
			CommentCount: n.Comments.TotalCount,
		}
		for _, cm := range n.Comments.Nodes {
			r.Comments = append(r.Comments, PRReviewComment{ID: int64(cm.DatabaseID), Path: cm.Path})
		}
		page.Reviews = append(page.Reviews, r)
	}
	return page, nil
}
