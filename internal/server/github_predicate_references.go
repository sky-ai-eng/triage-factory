package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// init registers repoInFaults for every GitHub event type whose predicate has a
// repo_in field. The events package registers its schemas in its own init,
// which runs before this one because this package imports it.
func init() {
	for eventType, schema := range events.All() {
		if !strings.HasPrefix(eventType, "github:") {
			continue
		}
		for _, field := range schema.Fields {
			if field.Name == "repo_in" {
				RegisterPredicateReferenceCheck(eventType, repoInFaults)
				break
			}
		}
	}
}

// repoInFaults is the PredicateReferenceCheck for GitHub event types: every
// repo_in entry the predicate adds must be the registry id of a repository the
// handler's team tracks on the org's GitHub host. That is the set the predicate
// editor offers, and the only set that can match, because the router delivers
// a team's handlers only events from repositories it tracks. An owner/name,
// the value a caller would naturally reach for, is refused here rather than
// stored as a filter that never fires.
func repoInFaults(ctx context.Context, tx db.TxStores, orgID, teamID string, predicate, prior *string) ([]string, error) {
	priorIn, err := predicateRepoIn(prior)
	if err != nil {
		return nil, err
	}
	nextIn, err := predicateRepoIn(predicate)
	if err != nil {
		return nil, err
	}
	// seen starts as the stored entries, which are not re-checked, and also
	// keeps an entry the predicate repeats from being reported twice.
	seen := make(map[string]bool, len(priorIn))
	for _, id := range priorIn {
		seen[id] = true
	}
	var added []string
	for _, id := range nextIn {
		if !seen[id] {
			seen[id] = true
			added = append(added, id)
		}
	}
	if len(added) == 0 {
		return nil, nil
	}

	settings, err := tx.Orgs.GetSettings(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("read org github host: %w", err)
	}
	tracked := map[string]bool{}
	if host := db.EffectiveGitHubHost(settings.GitHubBaseURL); host != "" {
		repos, _, err := tx.Repos.ListTrackedByTeam(ctx, orgID, teamID, host, db.Unwindowed)
		if err != nil {
			return nil, fmt.Errorf("read team tracked repositories: %w", err)
		}
		for _, repo := range repos {
			tracked[repo.ID] = true
		}
	}
	var faults []string
	for _, id := range added {
		if !tracked[id] {
			faults = append(faults, fmt.Sprintf("repo_in: %q is not the registry id of a repository this team tracks "+
				"(repo_in takes the id field of POST /api/repos/list, not owner/name)", id))
		}
	}
	return faults, nil
}

// predicateRepoIn reads repo_in from a GitHub predicate; nil for match-all.
func predicateRepoIn(predicate *string) ([]string, error) {
	if predicate == nil {
		return nil, nil
	}
	var p struct {
		RepoIn []string `json:"repo_in"`
	}
	if err := json.Unmarshal([]byte(*predicate), &p); err != nil {
		return nil, fmt.Errorf("decode github predicate: %w", err)
	}
	return p.RepoIn, nil
}
