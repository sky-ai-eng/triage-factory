package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// gitHubAccountIDFields are the GitHub predicate fields that name people by
// GitHub's account id. Each event type has the ones its metadata carries an
// actor id for.
var gitHubAccountIDFields = []string{"author_id_in", "reviewer_id_in", "commenter_id_in", "requested_user_id_in"}

// init registers githubPredicateFaults for every GitHub event type whose
// predicate has a repo_in field, which is every GitHub type with a predicate.
// The events package registers its schemas in its own init, which runs before
// this one because this package imports it.
func init() {
	for eventType, schema := range events.All() {
		if !strings.HasPrefix(eventType, "github:") {
			continue
		}
		for _, field := range schema.Fields {
			if field.Name == "repo_in" {
				RegisterPredicateReferenceCheck(eventType, githubPredicateFaults)
				break
			}
		}
	}
}

// githubPredicateFaults is the PredicateReferenceCheck for GitHub event types.
// It refuses the entries a write adds that cannot match: a repo_in entry that
// is not a repository the team tracks (repoInFaults), a people entry that is
// not a GitHub account id (a login, the value a caller would naturally reach
// for, never equals one), and a requested_team_id_in entry that is not one of
// the team's GitHub-team mappings.
func githubPredicateFaults(ctx context.Context, tx db.TxStores, orgID, teamID string, predicate, prior *string) ([]PredicateReferenceFault, error) {
	faults, err := repoInFaults(ctx, tx, orgID, teamID, predicate, prior)
	if err != nil {
		return nil, err
	}
	for _, field := range gitHubAccountIDFields {
		added, err := addedPredicateEntries(predicate, prior, field)
		if err != nil {
			return nil, err
		}
		for _, id := range added {
			if !domain.IsGitHubID(id) {
				faults = append(faults, PredicateReferenceFault{Key: field, Message: fmt.Sprintf("%s: %q is not a GitHub account id (%s takes GitHub's numeric id for the account, "+
					"the github_user_id of GET /api/orgs/{org_id}/github/users/by-name/{login}, not a login)", field, id, field)})
			}
		}
	}
	teamFaults, err := trackedIDFaults(predicate, prior, []string{"requested_team_id_in"},
		"is not the GitHub team id of one of this team's GitHub-team mappings "+
			"(requested_team_id_in takes the github_team_id of GET /api/teams/{team_id}/github-groups, not org/slug)",
		func() (map[string]bool, error) { return mappedGitHubTeamIDs(ctx, tx, orgID, teamID) })
	if err != nil {
		return nil, err
	}
	return append(faults, teamFaults...), nil
}

// mappedGitHubTeamIDs is the set of GitHub team ids the team maps on the org's
// GitHub host: the teams whose review requests route to it, and the set the
// predicate editor offers for requested_team_id_in.
func mappedGitHubTeamIDs(ctx context.Context, tx db.TxStores, orgID, teamID string) (map[string]bool, error) {
	settings, err := tx.Orgs.GetSettings(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("read org github host: %w", err)
	}
	mapped := map[string]bool{}
	host := db.EffectiveGitHubHost(settings.GitHubBaseURL)
	if host == "" {
		return mapped, nil
	}
	groups, err := tx.TeamGitHubGroups.ListForTeam(ctx, teamID, host)
	if err != nil {
		return nil, fmt.Errorf("read team github-team mappings: %w", err)
	}
	for _, g := range groups {
		if g.GitHubTeamID != "" {
			mapped[g.GitHubTeamID] = true
		}
	}
	return mapped, nil
}

// addedPredicateEntries returns the entries of field that predicate adds over
// prior.
func addedPredicateEntries(predicate, prior *string, field string) ([]string, error) {
	priorIn, err := predicateIDList(prior, field)
	if err != nil {
		return nil, err
	}
	nextIn, err := predicateIDList(predicate, field)
	if err != nil {
		return nil, err
	}
	return addedEntries(priorIn, nextIn), nil
}

// repoInFaults reports the repo_in entries the predicate adds that are not the
// registry id of a repository the handler's team tracks on the org's GitHub
// host. That is the set the predicate editor offers, and the only set that can
// match, because the router delivers a team's handlers only events from
// repositories it tracks. An owner/name, the value a caller would naturally
// reach for, is refused here rather than stored as a filter that never fires.
func repoInFaults(ctx context.Context, tx db.TxStores, orgID, teamID string, predicate, prior *string) ([]PredicateReferenceFault, error) {
	added, err := addedPredicateEntries(predicate, prior, "repo_in")
	if err != nil {
		return nil, err
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
	var faults []PredicateReferenceFault
	for _, id := range added {
		if !tracked[id] {
			faults = append(faults, PredicateReferenceFault{Key: "repo_in", Message: fmt.Sprintf("repo_in: %q is not the registry id of a repository this team tracks "+
				"(repo_in takes the id field of POST /api/repos/list, not owner/name)", id)})
		}
	}
	return faults, nil
}
