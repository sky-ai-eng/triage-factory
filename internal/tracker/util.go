package tracker

import (
	"encoding/json"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		trackerLog.Error("mustJSON marshal error", "error", err)
		return "{}"
	}
	return string(data)
}

// ghSourceID returns a globally unique source_id for a GitHub PR.
// PR numbers are only unique within a repo, so we prefix with "owner/repo#".
//
// Delegates rather than formatting its own: this is the key the poller mints
// entities on, and the exec recording funnel resolves the same entity from a
// PR artifact's target to stamp its owning team. The two have to agree byte for
// byte — a drifted separator would not error anywhere, it would just quietly
// mint a second entity and lose the stamp — so both now read the format off
// domain.PullRequestTarget.
func ghSourceID(repo string, number int) string {
	return domain.PullRequestTarget(repo, number)
}

// maxReposPerQuery caps how many repo: qualifiers go into a single GitHub
// search query. GitHub's GraphQL runtime-cost limit is per-query: a search
// across many repos with a rich fragment can exhaust the "Resource limits
// for this query exceeded" budget even when the static 500k-node budget is
// fine. Splitting into smaller batches keeps each query's runtime cost low.
//
// With 2-3 repos per query, a worst-case merged-PRs backfill (lots of
// results, 30-day window) stays well under the limit. The trade-off is
// more round trips — 8 base queries x ceil(N/maxReposPerQuery) scoped
// queries — but each one is cheap and the rate-limit overhead (1 point
// per search query, 5000/hour budget) is negligible even at dozens of
// queries per poll cycle.
const maxReposPerQuery = 3

// scopedQueries takes a base search query and returns one or more queries
// with " repo:owner/name" qualifiers appended, batched to stay under both
// maxSearchQueryLen and maxReposPerQuery. If no repos are configured,
// returns the base query as-is.
func scopedQueries(base string, repos []string) []string {
	if len(repos) == 0 {
		return []string{base}
	}

	var queries []string
	current := base
	reposInCurrent := 0
	for _, repo := range repos {
		term := " repo:" + repo
		if len(current)+len(term) > maxSearchQueryLen || reposInCurrent >= maxReposPerQuery {
			if reposInCurrent > 0 {
				queries = append(queries, current)
			}
			current = base + term
			reposInCurrent = 1
		} else {
			current += term
			reposInCurrent++
		}
	}
	queries = append(queries, current)
	return queries
}
