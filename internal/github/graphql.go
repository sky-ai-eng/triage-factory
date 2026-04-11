package github

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/sky-ai-eng/todo-triage/internal/domain"
)

// prFragment is the GraphQL fragment used for both discovery and refresh.
// Contains every field needed to build a PRSnapshot.
const prFragment = `
fragment PRFields on PullRequest {
	id
	number
	title
	author { login }
	state
	isDraft
	merged
	mergeable
	headRefName
	baseRefName
	url
	repository { nameWithOwner }
	headRepository { nameWithOwner }
	additions
	deletions
	changedFiles
	reviewRequests(first: 10) {
		nodes {
			requestedReviewer {
				... on User { login }
				... on Team { name }
			}
		}
	}
	latestReviews(first: 20) {
		nodes {
			author { login }
			state
			submittedAt
		}
	}
	reviews(first: 1) { totalCount }
	commits(last: 1) {
		nodes {
			commit {
				oid
				checkSuites(first: 100) {
					pageInfo { hasNextPage }
					nodes {
						workflowRun { databaseId }
						checkRuns(first: 100) {
							pageInfo { hasNextPage }
							nodes {
								databaseId
								name
								status
								conclusion
								completedAt
								detailsUrl
							}
						}
					}
				}
			}
		}
	}
	labels(first: 10) { nodes { name } }
	comments { totalCount }
	createdAt
	updatedAt
	mergedAt
	closedAt
}
`

// DiscoveredPR is a PR returned from a discovery search, including its GraphQL node ID.
type DiscoveredPR struct {
	NodeID   string
	Snapshot domain.PRSnapshot
}

// DiscoverPRs runs a GitHub search query via GraphQL and returns discovered PRs.
// The query should be a GitHub search string like "is:pr is:open review-requested:user".
func (c *Client) DiscoverPRs(searchQuery string, limit int) ([]DiscoveredPR, error) {
	if limit <= 0 {
		limit = 50
	}

	query := fmt.Sprintf(`
		query($q: String!, $limit: Int!) {
			search(query: $q, type: ISSUE, first: $limit) {
				nodes { ...PRFields }
			}
		}
		%s
	`, prFragment)

	data, err := c.PostGraphQL(map[string]any{
		"query":     query,
		"variables": map[string]any{"q": searchQuery, "limit": limit},
	})
	if err != nil {
		return nil, fmt.Errorf("discover PRs: %w", err)
	}

	var resp struct {
		Data struct {
			Search struct {
				Nodes []gqlPR `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse discover response: %w", err)
	}

	var results []DiscoveredPR
	for _, pr := range resp.Data.Search.Nodes {
		if pr.Number == 0 {
			continue // skip non-PR nodes (shouldn't happen but defensive)
		}
		results = append(results, DiscoveredPR{
			NodeID:   pr.ID,
			Snapshot: pr.toSnapshot(),
		})
	}
	return results, nil
}

// RefreshPRs batch-fetches current state for tracked PRs using their GraphQL node IDs.
// Returns a map of node ID → snapshot. Missing/deleted PRs are silently omitted.
func (c *Client) RefreshPRs(nodeIDs []string) (map[string]domain.PRSnapshot, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}

	query := fmt.Sprintf(`
		query($ids: [ID!]!) {
			nodes(ids: $ids) { ...PRFields }
		}
		%s
	`, prFragment)

	data, err := c.PostGraphQL(map[string]any{
		"query":     query,
		"variables": map[string]any{"ids": nodeIDs},
	})
	if err != nil {
		return nil, fmt.Errorf("refresh PRs: %w", err)
	}

	var resp struct {
		Data struct {
			Nodes []json.RawMessage `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("parse refresh response: %w", err)
	}

	results := make(map[string]domain.PRSnapshot, len(nodeIDs))
	for i, raw := range resp.Data.Nodes {
		if string(raw) == "null" {
			continue // deleted or inaccessible
		}
		var pr gqlPR
		if err := json.Unmarshal(raw, &pr); err != nil {
			continue
		}
		if pr.Number == 0 {
			continue
		}
		results[nodeIDs[i]] = pr.toSnapshot()
	}
	return results, nil
}

// --- GraphQL response types ---

type gqlPR struct {
	ID             string        `json:"id"`
	Number         int           `json:"number"`
	Title          string        `json:"title"`
	Author         gqlAuthor     `json:"author"`
	State          string        `json:"state"`
	IsDraft        bool          `json:"isDraft"`
	Merged         bool          `json:"merged"`
	Mergeable      string        `json:"mergeable"`
	HeadRefName    string        `json:"headRefName"`
	BaseRefName    string        `json:"baseRefName"`
	URL            string        `json:"url"`
	Repository     gqlRepo       `json:"repository"`
	HeadRepository *gqlRepo      `json:"headRepository"`
	Additions      int           `json:"additions"`
	Deletions      int           `json:"deletions"`
	ChangedFiles   int           `json:"changedFiles"`
	ReviewRequests gqlRRNodes    `json:"reviewRequests"`
	LatestReviews  gqlRevNodes   `json:"latestReviews"`
	Reviews        gqlCount      `json:"reviews"`
	Commits        gqlCommits    `json:"commits"`
	Labels         gqlLabelNodes `json:"labels"`
	Comments       gqlCount      `json:"comments"`
	CreatedAt      string        `json:"createdAt"`
	UpdatedAt      string        `json:"updatedAt"`
	MergedAt       string        `json:"mergedAt"`
	ClosedAt       string        `json:"closedAt"`
}

type gqlRepo struct {
	NameWithOwner string `json:"nameWithOwner"`
}

type gqlRRNodes struct {
	Nodes []struct {
		RequestedReviewer gqlReviewer `json:"requestedReviewer"`
	} `json:"nodes"`
}

type gqlReviewer struct {
	Login string `json:"login"` // User
	Name  string `json:"name"`  // Team
}

type gqlRevNodes struct {
	Nodes []struct {
		Author      gqlAuthor `json:"author"`
		State       string    `json:"state"`
		SubmittedAt string    `json:"submittedAt"`
	} `json:"nodes"`
}

type gqlAuthor struct {
	Login string `json:"login"`
}

type gqlCount struct {
	TotalCount int `json:"totalCount"`
}

type gqlCommits struct {
	Nodes []struct {
		Commit gqlCommit `json:"commit"`
	} `json:"nodes"`
}

type gqlCommit struct {
	OID         string         `json:"oid"`
	CheckSuites gqlCheckSuites `json:"checkSuites"`
}

type gqlCheckSuites struct {
	PageInfo gqlPageInfo     `json:"pageInfo"`
	Nodes    []gqlCheckSuite `json:"nodes"`
}

type gqlCheckSuite struct {
	WorkflowRun *gqlWorkflowRun `json:"workflowRun"`
	CheckRuns   gqlCheckRuns    `json:"checkRuns"`
}

// gqlWorkflowRun is non-nil only for check suites originating from GitHub
// Actions workflows. Third-party CI systems (Supabase, Circle, etc.) produce
// check suites with workflowRun == nil.
type gqlWorkflowRun struct {
	DatabaseID int64 `json:"databaseId"`
}

type gqlCheckRuns struct {
	PageInfo gqlPageInfo   `json:"pageInfo"`
	Nodes    []gqlCheckRun `json:"nodes"`
}

// gqlPageInfo is a minimal subset of GitHub's PageInfo used only to detect
// when a connection was truncated at the first-N limit. We don't paginate
// (matrix builds deep enough to blow through 100×100 check runs aren't a real
// case today) but we log a warning if we hit the limit so we notice before
// missing events becomes a silent failure mode.
type gqlPageInfo struct {
	HasNextPage bool `json:"hasNextPage"`
}

type gqlCheckRun struct {
	DatabaseID  int64  `json:"databaseId"`
	Name        string `json:"name"`
	Status      string `json:"status"`
	Conclusion  string `json:"conclusion"`
	CompletedAt string `json:"completedAt"`
	DetailsURL  string `json:"detailsUrl"`
}

type gqlLabelNodes struct {
	Nodes []struct {
		Name string `json:"name"`
	} `json:"nodes"`
}

// toSnapshot converts the GraphQL response to our extracted snapshot type.
func (pr gqlPR) toSnapshot() domain.PRSnapshot {
	snap := domain.PRSnapshot{
		Number:       pr.Number,
		Title:        pr.Title,
		Author:       pr.Author.Login,
		Repo:         pr.Repository.NameWithOwner,
		URL:          pr.URL,
		State:        pr.State,
		IsDraft:      pr.IsDraft,
		Merged:       pr.Merged,
		Mergeable:    pr.Mergeable,
		HeadRef:      pr.HeadRefName,
		BaseRef:      pr.BaseRefName,
		Additions:    pr.Additions,
		Deletions:    pr.Deletions,
		ChangedFiles: pr.ChangedFiles,
		ReviewCount:  pr.Reviews.TotalCount,
		CommentCount: pr.Comments.TotalCount,
		CreatedAt:    pr.CreatedAt,
		UpdatedAt:    pr.UpdatedAt,
		MergedAt:     pr.MergedAt,
		ClosedAt:     pr.ClosedAt,
	}

	if pr.HeadRepository != nil {
		snap.HeadRepo = pr.HeadRepository.NameWithOwner
	}

	// CI check runs from the latest commit.
	//
	// Even when the commit has no check suites, we initialize CheckRuns to a
	// non-nil empty slice so downstream diff logic can distinguish "polled,
	// nothing here" (empty) from "unknown prior state" (nil, meaning an old
	// snapshot from before this field existed).
	snap.CheckRuns = []domain.CheckRun{}
	if len(pr.Commits.Nodes) > 0 {
		commit := pr.Commits.Nodes[0].Commit
		snap.HeadSHA = commit.OID

		// Pagination truncation watchdog. The query caps at 100 suites and
		// 100 runs per suite — plenty for realistic PRs (even a heavy matrix
		// build caps around ~30 runs per suite × 5-8 suites). If we ever
		// blow past these, log once so we catch it before missing events
		// becomes a silent failure. Paginating would mean real cursor
		// logic; preferring the simpler cap + warning until there's pressure.
		if commit.CheckSuites.PageInfo.HasNextPage {
			log.Printf("[github] WARN: check suites truncated at 100 for %s#%d — some CI state may be missing from snapshot", snap.Repo, snap.Number)
		}

		var raw []domain.CheckRun
		for _, suite := range commit.CheckSuites.Nodes {
			if suite.CheckRuns.PageInfo.HasNextPage {
				log.Printf("[github] WARN: check runs truncated at 100 within a suite for %s#%d — some CI state may be missing from snapshot", snap.Repo, snap.Number)
			}
			var workflowRunID int64
			if suite.WorkflowRun != nil {
				workflowRunID = suite.WorkflowRun.DatabaseID
			}
			for _, cr := range suite.CheckRuns.Nodes {
				raw = append(raw, domain.CheckRun{
					ID:            cr.DatabaseID,
					Name:          cr.Name,
					Status:        strings.ToLower(cr.Status),
					Conclusion:    strings.ToLower(cr.Conclusion),
					CompletedAt:   cr.CompletedAt,
					DetailsURL:    cr.DetailsURL,
					WorkflowRunID: workflowRunID,
				})
			}
		}
		snap.CheckRuns = domain.DedupCheckRunsByName(raw)
	}

	// Review requests
	for _, rr := range pr.ReviewRequests.Nodes {
		name := rr.RequestedReviewer.Login
		if name == "" {
			name = rr.RequestedReviewer.Name // team
		}
		if name != "" {
			snap.ReviewRequests = append(snap.ReviewRequests, name)
		}
	}

	// Latest reviews per reviewer
	for _, rev := range pr.LatestReviews.Nodes {
		if rev.Author.Login != "" {
			snap.Reviews = append(snap.Reviews, domain.ReviewState{
				Author:      rev.Author.Login,
				State:       rev.State,
				SubmittedAt: rev.SubmittedAt,
			})
		}
	}

	// Labels
	for _, l := range pr.Labels.Nodes {
		snap.Labels = append(snap.Labels, l.Name)
	}

	return snap
}
