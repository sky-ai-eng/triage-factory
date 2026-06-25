package github

import (
	"encoding/json"
	"fmt"
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
				... on Team { slug organization { login } }
			}
		}
	}
	latestReviews(first: 20) {
		nodes {
			id
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
			commit { oid committedDate }
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
// per-check-run CI data from the head commit. Used by RefreshPRs for
// OPEN PRs only — these are the ones where CI state changes drive
// events (github:pr:ci_failed, github:pr:ci_passed).
//
// CI is read from the head commit's statusCheckRollup.contexts — a FLAT
// list of the actual check runs on the commit. We deliberately do NOT use
// checkSuites(first:N){checkRuns(first:M)}: GitHub mints one check suite
// per installed GitHub App on every commit, including apps that post no
// check runs, so the suite connection is padded with empties. In orgs with
// a normal app footprint that blows past any reasonable suite cap, wastes
// the node budget on empty padding, and (since suites aren't relevance-
// sorted) can push a real CI suite past the cap. The flat rollup sidesteps
// all of that.
//
// Node budget per PR: ~160 (100 contexts + overhead) — down ~10× from the
// old ~1,060 (20 suites × 50 runs). A RefreshPRs call for N open PRs costs
// roughly N × 160 nodes; with refreshBatchSize=20 that's ~3,200 nodes per
// query, comfortably under the 500k ceiling. contexts is a union of CheckRun
// and StatusContext; we inline only CheckRun fields (StatusContext is the
// legacy commit-status API the old suite query never surfaced). The
// hasNextPage watchdog in toSnapshot() logs if we ever truncate at the flat
// 100 cap so real truncation stays visible instead of silent.
const prFullFragment = `
fragment PRFullFields on PullRequest {
` + prBaseFields + `
	commits(last: 1) {
		nodes {
			commit {
				oid
				committedDate
				statusCheckRollup {
					contexts(first: 100) {
						pageInfo { hasNextPage }
						nodes {
							__typename
							... on CheckRun {
								databaseId
								name
								status
								conclusion
								completedAt
								detailsUrl
								checkSuite { workflowRun { databaseId } }
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
	# Timeline tail for source-time enrichment of label / review-request /
	# ready-for-review events. The diff layer needs the per-action
	# createdAt for each transition; the PR's top-level updatedAt is the
	# last activity on the PR which clusters wrong when multiple actions
	# happen between polls. last:20 covers the typical poll window
	# comfortably; older timeline items fall through to the diff's
	# updatedAt fallback. ~5 nodes per item × 20 = ~100 nodes per PR,
	# additive to the existing ~160-node budget — a modest bump well
	# within the 500k ceiling per the prFullFragment math above.
	timelineItems(last: 20, itemTypes: [LABELED_EVENT, UNLABELED_EVENT, REVIEW_REQUESTED_EVENT, READY_FOR_REVIEW_EVENT]) {
		nodes {
			__typename
			... on LabeledEvent {
				createdAt
				label { name }
			}
			... on UnlabeledEvent {
				createdAt
				label { name }
			}
			... on ReviewRequestedEvent {
				createdAt
				requestedReviewer {
					... on User { login }
					... on Team { slug organization { login } }
				}
			}
			... on ReadyForReviewEvent {
				createdAt
			}
		}
	}
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
// 20 IDs with the full fragment (~160 nodes each = ~3,200 nodes) is
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
	TimelineItems  gqlTimeline   `json:"timelineItems"`
}

// gqlTimeline is the heterogeneous PullRequest.timelineItems connection,
// scoped (via the GraphQL query) to the four event types we use for
// source-time enrichment. Nodes is decoded into the union struct
// gqlTimelineNode; non-requested fields stay zero-valued for items
// whose __typename doesn't carry them.
type gqlTimeline struct {
	Nodes []gqlTimelineNode `json:"nodes"`
}

// gqlTimelineNode flattens the four timeline event types we request into
// a single struct. The discriminator is __typename — buildSnapshot reads
// it to know which fields are populated. Adding a new timeline kind here
// requires extending both the GraphQL query (prFullFragment) and the
// switch in buildSnapshot that maps these into domain.TimelineEvent.
type gqlTimelineNode struct {
	Typename  string `json:"__typename"`
	CreatedAt string `json:"createdAt"`
	// Label is populated for LabeledEvent and UnlabeledEvent.
	Label struct {
		Name string `json:"name"`
	} `json:"label"`
	// RequestedReviewer is populated for ReviewRequestedEvent. Reuses
	// gqlReviewer's User/Team union.
	RequestedReviewer gqlReviewer `json:"requestedReviewer"`
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

// gqlReviewer unions GraphQL's User/Team reviewer node. Login is populated
// for User reviewers; Slug + Organization.Login are populated for Team
// reviewers. We emit team identifiers as "org/slug" (matching what
// GET /user/teams returns) so team-based review requests can be matched
// against the user's team memberships. The older Name field is no longer
// requested — display names aren't a stable identifier.
type gqlReviewer struct {
	Login        string `json:"login"` // User
	Slug         string `json:"slug"`  // Team slug
	Organization struct {
		Login string `json:"login"`
	} `json:"organization"` // Team's org, for building "org/slug"
}

type gqlRevNodes struct {
	Nodes []struct {
		ID          string    `json:"id"`
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
	OID               string                `json:"oid"`
	CommittedDate     string                `json:"committedDate"`
	StatusCheckRollup *gqlStatusCheckRollup `json:"statusCheckRollup"`
}

// gqlStatusCheckRollup is the head commit's flattened CI state. contexts is a
// flat list of the commit's actual check runs (and legacy status contexts) —
// it is NOT padded with the empty per-installed-app check suites that the old
// checkSuites query truncated on. nil when the commit has no rollup at all.
type gqlStatusCheckRollup struct {
	Contexts gqlRollupContexts `json:"contexts"`
}

type gqlRollupContexts struct {
	PageInfo gqlPageInfo        `json:"pageInfo"`
	Nodes    []gqlRollupContext `json:"nodes"`
}

// gqlRollupContext is one node of statusCheckRollup.contexts. The connection
// is a union of CheckRun and StatusContext; the fragment inlines only CheckRun
// fields, so StatusContext nodes decode with Typename=="StatusContext" and
// zero CheckRun fields. buildSnapshot filters on Typename to drop them —
// preserving the old suite query's behavior of surfacing check runs only.
type gqlRollupContext struct {
	Typename    string            `json:"__typename"`
	DatabaseID  int64             `json:"databaseId"`
	Name        string            `json:"name"`
	Status      string            `json:"status"`
	Conclusion  string            `json:"conclusion"`
	CompletedAt string            `json:"completedAt"`
	DetailsURL  string            `json:"detailsUrl"`
	CheckSuite  *gqlCheckSuiteRef `json:"checkSuite"`
}

// gqlCheckSuiteRef carries the owning suite's workflow-run linkage so we can
// recover the GitHub Actions WorkflowRunID for Actions-backed check runs.
type gqlCheckSuiteRef struct {
	WorkflowRun *gqlWorkflowRun `json:"workflowRun"`
}

// gqlWorkflowRun is non-nil only for check runs originating from GitHub
// Actions workflows. Third-party CI systems (Supabase, Circle, etc.) produce
// check runs whose suite has workflowRun == nil.
type gqlWorkflowRun struct {
	DatabaseID int64 `json:"databaseId"`
}

// gqlPageInfo is a minimal subset of GitHub's PageInfo used only to detect
// when a connection was truncated at the first-N limit. We don't paginate
// (a head commit with >100 check runs isn't a real case today) but we log a
// warning if we hit the limit so we notice before missing events becomes a
// silent failure mode. See the page-cap comment inside prFullFragment for
// why the cap is what it is.
type gqlPageInfo struct {
	HasNextPage bool `json:"hasNextPage"`
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
		snap.HeadCommittedAt = commit.CommittedDate

		if includeCheckRuns {
			// Initialize to non-nil empty so downstream diff sees "polled,
			// nothing here" rather than "unknown prior state" (nil).
			snap.CheckRuns = []domain.CheckRun{}

			// statusCheckRollup is nil only when the commit has no CI rollup
			// at all — treated identically to "polled, nothing here".
			if commit.StatusCheckRollup != nil {
				contexts := commit.StatusCheckRollup.Contexts

				// Pagination truncation watchdog. Do not raise the cap without
				// re-running the node-budget math in prFullFragment's comment.
				if contexts.PageInfo.HasNextPage {
					githubLog.Warn("check rollup contexts truncated; some CI state may be missing from snapshot",
						"repo", snap.Repo, "number", snap.Number, "cap", 100)
				}

				var raw []domain.CheckRun
				for _, ctx := range contexts.Nodes {
					// contexts is a CheckRun|StatusContext union; the fragment
					// inlines only CheckRun fields, so StatusContext nodes
					// arrive empty. Drop anything that isn't a CheckRun to
					// preserve the old suite query's check-runs-only behavior.
					if ctx.Typename != "CheckRun" {
						continue
					}
					var workflowRunID int64
					if ctx.CheckSuite != nil && ctx.CheckSuite.WorkflowRun != nil {
						workflowRunID = ctx.CheckSuite.WorkflowRun.DatabaseID
					}
					raw = append(raw, domain.CheckRun{
						ID:            ctx.DatabaseID,
						Name:          ctx.Name,
						Status:        strings.ToLower(ctx.Status),
						Conclusion:    strings.ToLower(ctx.Conclusion),
						CompletedAt:   ctx.CompletedAt,
						DetailsURL:    ctx.DetailsURL,
						WorkflowRunID: workflowRunID,
					})
				}
				snap.CheckRuns = domain.DedupCheckRunsByName(raw)
			}
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
		githubLog.Warn("review requests truncated; reviewer detection may miss users past the cap",
			"repo", snap.Repo, "number", snap.Number, "cap", 100)
	}
	for _, rr := range pr.ReviewRequests.Nodes {
		if login := rr.RequestedReviewer.Login; login != "" {
			snap.ReviewRequests = append(snap.ReviewRequests, login)
			continue
		}
		// Team reviewer: emit "org/slug" so it matches the format returned
		// by GET /user/teams. Without the org prefix, two teams named
		// "platform" in different orgs would collide.
		if slug := rr.RequestedReviewer.Slug; slug != "" {
			org := rr.RequestedReviewer.Organization.Login
			if org != "" {
				snap.ReviewRequests = append(snap.ReviewRequests, org+"/"+slug)
			} else {
				snap.ReviewRequests = append(snap.ReviewRequests, slug)
			}
		}
	}

	// Latest reviews per reviewer. ID is the review's GraphQL node id —
	// carried so the artifact reconciler (TFAC-464) can match a pending
	// review artifact (keyed on that id) to its now-submitted/dismissed state
	// here, rather than matching by author (which can't distinguish a fresh
	// pending review from a prior submitted one by the same identity).
	for _, rev := range pr.LatestReviews.Nodes {
		if rev.Author.Login != "" {
			snap.Reviews = append(snap.Reviews, domain.ReviewState{
				ID:          rev.ID,
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

	// Timeline items — only populated on the full fragment. The discovery
	// fragment doesn't request timelineItems (saves node budget on a path
	// that doesn't need source-time enrichment), so pr.TimelineItems.Nodes
	// is empty there and the loop is a no-op.
	for _, item := range pr.TimelineItems.Nodes {
		te := domain.TimelineEvent{CreatedAt: item.CreatedAt}
		switch item.Typename {
		case "LabeledEvent":
			te.Kind = "labeled"
			te.Label = item.Label.Name
		case "UnlabeledEvent":
			te.Kind = "unlabeled"
			te.Label = item.Label.Name
		case "ReviewRequestedEvent":
			te.Kind = "review_requested"
			// Mirror the "login" or "org/slug" formatting used for
			// snap.ReviewRequests so diff lookups can match by string
			// equality without re-deriving the team identifier.
			if login := item.RequestedReviewer.Login; login != "" {
				te.Reviewer = login
			} else if slug := item.RequestedReviewer.Slug; slug != "" {
				if org := item.RequestedReviewer.Organization.Login; org != "" {
					te.Reviewer = org + "/" + slug
				} else {
					te.Reviewer = slug
				}
			}
		case "ReadyForReviewEvent":
			te.Kind = "ready_for_review"
		default:
			continue // unknown type from the union — skip rather than guess
		}
		snap.Timeline = append(snap.Timeline, te)
	}

	return snap
}
