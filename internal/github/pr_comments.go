package github

import (
	"context"
	"encoding/json"
	"fmt"
)

// commentPageSize is how many conversation comments one read of a pull
// request's comments asks for.
const commentPageSize = 50

var prCommentsQuery = fmt.Sprintf(`
	query($id: ID!, $before: String) {
		node(id: $id) {
			... on PullRequest {
				comments(last: %d, before: $before) {
					totalCount
					pageInfo { hasPreviousPage startCursor }
					nodes {
						fullDatabaseId
						createdAt
						author { __typename login }
					}
				}
			}
		}
	}
`, commentPageSize)

// PRComment is a comment in a pull request's conversation: its numeric id, in
// the numbering /issues/comments/{id} addresses, its author and when it was
// made.
type PRComment struct {
	ID        int64
	Author    string // REST-spelled login; empty when GitHub names no author
	CreatedAt string
}

// PRCommentPage is one page of a pull request's conversation comments, oldest
// first, the order GitHub lists them in.
type PRCommentPage struct {
	Comments []PRComment
	// TotalCount is how many conversation comments the pull request has.
	TotalCount int
	// HasPrevious reports whether comments precede this page, and Before is
	// the cursor that reads them.
	HasPrevious bool
	Before      string
}

// PRCommentsBefore reads the page of conversation comments just before cursor
// before on the pull request whose GraphQL node id is prNodeID, or the last
// page when before is empty. Comments in review threads are not among them:
// they belong to reviews.
func (c *Client) PRCommentsBefore(ctx context.Context, prNodeID, before string) (PRCommentPage, error) {
	vars := map[string]any{"id": prNodeID, "before": nil}
	if before != "" {
		vars["before"] = before
	}
	data, err := c.PostGraphQL(ctx, map[string]any{"query": prCommentsQuery, "variables": vars})
	if err != nil {
		return PRCommentPage{}, fmt.Errorf("read comments: %w", err)
	}

	var resp struct {
		Data struct {
			Node *struct {
				Comments *struct {
					TotalCount int `json:"totalCount"`
					PageInfo   struct {
						HasPreviousPage bool   `json:"hasPreviousPage"`
						StartCursor     string `json:"startCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						DatabaseID gqlBigInt `json:"fullDatabaseId"`
						CreatedAt  string    `json:"createdAt"`
						Author     gqlAuthor `json:"author"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"node"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return PRCommentPage{}, fmt.Errorf("parse comments response: %w", err)
	}
	if resp.Data.Node == nil || resp.Data.Node.Comments == nil {
		return PRCommentPage{}, fmt.Errorf("read comments: pull request %s not found", prNodeID)
	}

	conn := resp.Data.Node.Comments
	page := PRCommentPage{
		TotalCount:  conn.TotalCount,
		HasPrevious: conn.PageInfo.HasPreviousPage,
		Before:      conn.PageInfo.StartCursor,
	}
	for _, n := range conn.Nodes {
		page.Comments = append(page.Comments, PRComment{
			ID:        int64(n.DatabaseID),
			Author:    n.Author.restLogin(),
			CreatedAt: n.CreatedAt,
		})
	}
	return page, nil
}
