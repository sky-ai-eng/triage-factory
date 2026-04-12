package tracker

import (
	"encoding/json"
	"fmt"
	"log"
)

func mustJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("[tracker] mustJSON marshal error: %v", err)
		return "{}"
	}
	return string(data)
}

// ghSourceID returns a globally unique source_id for a GitHub PR.
// PR numbers are only unique within a repo, so we prefix with "owner/repo#".
func ghSourceID(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
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
			queries = append(queries, current)
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
