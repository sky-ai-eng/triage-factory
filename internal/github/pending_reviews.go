package github

import (
	"context"
	"encoding/json"
	"fmt"
)

// PendingReviewComment is one inline review comment addressed by its GraphQL node
// ID, carrying the anchor (Path + Line, plus StartLine for a multi-line range)
// and Body. It is the node-id-keyed shape GetReview returns for a submitted
// review's inline comments (the reconciler's proposed-vs-final diff joins on the
// node id).
//
// Line and StartLine are pointers because GitHub returns them null for a
// comment with no anchor on the current diff (an outdated/unpositioned
// comment). nil therefore means "no current-diff line" — distinct from a real
// line, and not silently collapsed to 0, so a consumer can tell the two apart.
//
// Distinct from ReviewDetailComment (REST, int ID): GetReview reads comments via
// GraphQL, which addresses them by node ID, not the REST integer id.
type PendingReviewComment struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Line      *int   `json:"line,omitempty"`
	StartLine *int   `json:"start_line,omitempty"`
	Body      string `json:"body"`
}

// SubmittedReview is a review's content fetched by its GraphQL node id,
// regardless of state — a PENDING-only fetch returns nothing once a review is
// submitted. Comments carry their GraphQL node
// ids (not the REST integer ids GetReviewDetail returns) so a proposed-vs-final
// diff can join on the same key the pending-review editor used. TFAC-464.
type SubmittedReview struct {
	State string // APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED, PENDING
	Body  string //
	// Comments are the node-id-keyed RIGHT-side inline comments, capped at the
	// first 100 — a review with >100 comments truncates here, and GetReview
	// logs a warning so the
	// proposed-vs-final diff silently omitting the tail stays visible).
	Comments []PendingReviewComment
}

// GetReview fetches a review by its GraphQL node id in ANY state (submitted or
// dismissed included), via node(id:) — the handle the artifact stores as
// ExternalID. It backs the reconciler's review-divergence note: the agent's
// drafted review (the artifact's Proposed snapshot) is diffed against what
// actually landed here.
//
// Returns (nil, nil) when the id doesn't resolve to a review (deleted, or a node
// of another type) — absence is a normal result the caller degrades on, not an
// error. A GraphQL error (including a 200 partial) propagates.
func (c *Client) GetReview(ctx context.Context, reviewNodeID string) (*SubmittedReview, error) {
	if reviewNodeID == "" {
		return nil, fmt.Errorf("get review: empty review id")
	}
	query := `query($id: ID!) {
		node(id: $id) {
			... on PullRequestReview {
				state
				body
				comments(first: 100) {
					pageInfo { hasNextPage }
					nodes { id path line startLine body }
				}
			}
		}
	}`
	data, err := c.PostGraphQL(ctx, map[string]any{"query": query, "variables": map[string]any{"id": reviewNodeID}})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Node *struct {
				State    string `json:"state"`
				Body     string `json:"body"`
				Comments struct {
					PageInfo struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID        string `json:"id"`
						Path      string `json:"path"`
						Line      *int   `json:"line"`
						StartLine *int   `json:"startLine"`
						Body      string `json:"body"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"node"`
		} `json:"data"`
		Errors gqlErrors `json:"errors"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse review response: %w", err)
	}
	if err := resp.Errors.first("get review"); err != nil {
		return nil, err
	}
	// node==null (unresolved) or a non-review node (the inline fragment matched
	// nothing, leaving State empty) → no review to describe.
	if resp.Data.Node == nil || resp.Data.Node.State == "" {
		return nil, nil
	}
	// Comment-count truncation watchdog: a review
	// with >100 inline comments returns only the first 100, so the
	// proposed-vs-final diff would silently omit the tail. Log it rather than let
	// truncation read as deletions.
	if resp.Data.Node.Comments.PageInfo.HasNextPage {
		githubLog.Warn("review comments truncated at 100; the proposed-vs-final diff may omit comments past the cap",
			"review", reviewNodeID)
	}
	out := &SubmittedReview{State: resp.Data.Node.State, Body: resp.Data.Node.Body}
	for _, cm := range resp.Data.Node.Comments.Nodes {
		out.Comments = append(out.Comments, PendingReviewComment{
			ID: cm.ID, Path: cm.Path, Line: cm.Line, StartLine: cm.StartLine, Body: cm.Body,
		})
	}
	return out, nil
}
