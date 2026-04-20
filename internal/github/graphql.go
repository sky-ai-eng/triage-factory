package github

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// prBaseFields are the common GraphQL fields shared between the discovery
// and full fragments. Kept as a const so the two fragments stay in sync on
// everything except the CI check-run block.
const prBaseFields = `
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
	reviewRequests(first: 100) {
		pageInfo { hasNextPage }
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
`

// prDiscoveryFragment is a lightweight GraphQL fragment for discovery
// and for refreshing terminal (merged/closed) PRs. It fetches PR
// identity, metadata, reviews, and head SHA — but NOT check runs.
//
// Check runs are omitted because:
//   - Discovery only needs to find PRs and seed entities; the
//     next refresh cycle fills in CI detail for any PRs that need it.
//   - Merged/closed PRs are terminal — CI status is historical noise.
//
// The resulting snapshot has CheckRuns == nil, which the diff logic
// (diff.go:69) treats as "unknown prior state" and skips CI events.
//
// Node budget: ~50 per PR (no nested connections beyond reviews).
// A 50-result discovery query costs ~2,500 nodes — trivial compared
// to the 500,000-node ceiling.
const prDiscoveryFragment = `
fragment PRDiscoveryFields on PullRequest {
` + prBaseFields + `
	commits(last: 1) {
		nodes {
			commit { oid }
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

// prFullFragment includes everything in the discovery fragment plus
// per-check-run CI data from the head commit's check suites. Used by
// RefreshPRs for OPEN PRs only — these are the ones where CI state
// changes drive events (github:pr:ci_failed, github:pr:ci_passed).
//
// Node budget per PR: ~1,060 (20 suites × 50 runs + overhead).
// A RefreshPRs call for N open PRs costs roughly N × 1,060 nodes,
// so ~470 open PRs fit in a single query before hitting the 500k
// ceiling. If your tracked-open set grows past that, the fix is to
// batch RefreshPRs calls, not to bump page caps.
//
// Page caps (20 suites, 50 runs per suite) are load-bearing:
// 100/100 pushes a 50-result query to ~507k nodes and hard-errors.
// Do not bump without re-running the math. The hasNextPage watchdogs
// in toSnapshot() log when we truncate, so real truncation becomes
// visible in operator logs instead of being silent.
const prFullFragment = `
fragment PRFullFields on PullRequest {
` + prBaseFields + `
	commits(last: 1) {
		nodes {
			commit {
				oid
				checkSuites(first: 20) {
					pageInfo { hasNextPage }
					nodes {
						workflowRun { databaseId }
						checkRuns(first: 50) {
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
				nodes { ...PRDiscoveryFields }
			}
		}
		%s
	`, prDiscoveryFragment)

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
			Snapshot: pr.toDiscoverySnapshot(),
		})
	}
	return results, nil
}

// refreshBatchSize caps how many node IDs go into a single GraphQL
// nodes(ids: [...]) call. GitHub's per-query runtime-cost limit can
// reject large batches even when the static 500k-node budget is fine.
// 20 IDs with the full fragment (~1,060 nodes each = ~21k nodes) is
// well within both limits while keeping the round-trip count reasonable.
const refreshBatchSize = 20

// RefreshPRs batch-fetches current state for tracked PRs using their GraphQL node IDs.
// Returns a map of node ID → snapshot. Missing/deleted PRs are silently omitted.
//
// Internally batches into chunks of refreshBatchSize to stay under
// GitHub's per-query runtime-cost ceiling. Transparent to callers.
//
// includeCheckRuns controls which fragment is used and whether the resulting
// snapshots carry CI data:
//   - true  → prFullFragment, CheckRuns populated. Use for OPEN PRs where
//     CI state changes drive events.
//   - false → prDiscoveryFragment, CheckRuns == nil. Use for terminal
//     (merged/closed) PRs where CI status is irrelevant.
func (c *Client) RefreshPRs(nodeIDs []string, includeCheckRuns bool) (map[string]domain.PRSnapshot, error) {
	if len(nodeIDs) == 0 {
		return nil, nil
	}

	results := make(map[string]domain.PRSnapshot, len(nodeIDs))
	for i := 0; i < len(nodeIDs); i += refreshBatchSize {
		end := i + refreshBatchSize
		if end > len(nodeIDs) {
			end = len(nodeIDs)
		}
		batch, err := c.refreshPRsBatch(nodeIDs[i:end], includeCheckRuns)
		if err != nil {
			return nil, err
		}
		for k, v := range batch {
			results[k] = v
		}
	}
	return results, nil
}

// refreshPRsBatch is the single-call implementation for one batch of IDs.
func (c *Client) refreshPRsBatch(nodeIDs []string, includeCheckRuns bool) (map[string]domain.PRSnapshot, error) {
	fragment := prDiscoveryFragment
	fragmentSpread := "PRDiscoveryFields"
	if includeCheckRuns {
		fragment = prFullFragment
		fragmentSpread = "PRFullFields"
	}

	query := fmt.Sprintf(`
		query($ids: [ID!]!) {
			nodes(ids: $ids) { ...%s }
		}
		%s
	`, fragmentSpread, fragment)

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
		if includeCheckRuns {
			results[nodeIDs[i]] = pr.toSnapshot()
		} else {
			results[nodeIDs[i]] = pr.toDiscoverySnapshot()
		}
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
	PageInfo gqlPageInfo `json:"pageInfo"`
	Nodes    []struct {
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
// (matrix builds deep enough to blow through 20×50 check runs aren't a real
// case today) but we log a warning if we hit the limit so we notice before
// missing events becomes a silent failure mode. See the page-cap comment
// inside prFragment for why the caps are what they are.
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

// toSnapshot builds a full snapshot with CheckRuns populated — used by
// RefreshPRs for open PRs.
func (pr gqlPR) toSnapshot() domain.PRSnapshot { return pr.buildSnapshot(true) }

// toDiscoverySnapshot builds a lightweight snapshot with CheckRuns == nil
// (unknown prior state). Used by DiscoverPRs and by RefreshPRs for
// terminal PRs where CI is irrelevant.
func (pr gqlPR) toDiscoverySnapshot() domain.PRSnapshot { return pr.buildSnapshot(false) }

// buildSnapshot is the shared implementation for both snapshot methods.
// includeCheckRuns controls whether CI data is populated:
//
//   - true: CheckRuns is a non-nil slice (possibly empty). The diff logic
//     (diff.go:69) treats this as "known CI state" and evaluates check
//     transitions.
//   - false: CheckRuns stays nil. The diff logic skips the entire CI
//     section, preventing spurious events on first startup or for
//     terminal PRs that don't need CI tracking.
func (pr gqlPR) buildSnapshot(includeCheckRuns bool) domain.PRSnapshot {
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

	if len(pr.Commits.Nodes) > 0 {
		commit := pr.Commits.Nodes[0].Commit
		snap.HeadSHA = commit.OID

		if includeCheckRuns {
			// Initialize to non-nil empty so downstream diff sees "polled,
			// nothing here" rather than "unknown prior state" (nil).
			snap.CheckRuns = []domain.CheckRun{}

			// Pagination truncation watchdog. Do not raise caps without
			// re-running the node-budget math in prFullFragment's comment.
			if commit.CheckSuites.PageInfo.HasNextPage {
				log.Printf("[github] WARN: check suites truncated at 20 for %s#%d — some CI state may be missing from snapshot", snap.Repo, snap.Number)
			}

			var raw []domain.CheckRun
			for _, suite := range commit.CheckSuites.Nodes {
				if suite.CheckRuns.PageInfo.HasNextPage {
					log.Printf("[github] WARN: check runs truncated at 50 within a suite for %s#%d — some CI state may be missing from snapshot", snap.Repo, snap.Number)
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
		// If !includeCheckRuns, snap.CheckRuns stays nil — "unknown prior
		// state" — so the diff logic skips CI evaluation for this snapshot.
	}

	// Review requests. The first: cap is load-bearing for detecting whether
	// the session user is a pending reviewer — if they fall outside the
	// returned slice, both the discovery backfill (tracker.go) and the diff
	// transition (diff.go) silently skip emitting review_requested. Log on
	// truncation so a future CODEOWNERS-spam case that trips the cap is
	// visible rather than manifesting as missing queue items.
	if pr.ReviewRequests.PageInfo.HasNextPage {
		log.Printf("[github] WARN: review requests truncated at 100 for %s#%d — reviewer detection may miss users past the cap", snap.Repo, snap.Number)
	}
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
