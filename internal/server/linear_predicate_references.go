package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// init registers linearTeamInFaults for every Linear event type whose
// predicate has a linear_team_id_in field. The events package registers its
// schemas in its own init, which runs before this one because this package
// imports it.
func init() {
	for eventType, schema := range events.All() {
		if !strings.HasPrefix(eventType, "linear:") {
			continue
		}
		for _, field := range schema.Fields {
			if field.Name == "linear_team_id_in" {
				RegisterPredicateReferenceCheck(eventType, linearTeamInFaults)
				break
			}
		}
	}
}

// linearTeamInFaults is the PredicateReferenceCheck for Linear event types:
// every linear_team_id_in entry the predicate adds, and every
// old_linear_team_id_in entry on identifier_changed, must be the id of a
// Linear team the handler's team tracks in the org's current Linear
// workspace. That is the set the predicate editor offers, and for every event
// but identifier_changed the only set that can match, because the router
// delivers a team's handlers only events from Linear teams it tracks. A team
// key, the value a caller would naturally reach for, is refused here rather
// than stored as a filter that never fires.
//
// identifier_changed reaches a team that tracks either of its two Linear
// teams, so one of its filters could name a team this team does not track and
// still match. It is held to the tracked set all the same: TF stores nothing
// about a Linear team no team tracks, so there is nothing to check such an id
// against.
func linearTeamInFaults(ctx context.Context, tx db.TxStores, orgID, teamID string, predicate, prior *string) ([]string, error) {
	return trackedIDFaults(predicate, prior, []string{"linear_team_id_in", "old_linear_team_id_in"},
		"is not the id of a Linear team this team tracks "+
			"(it takes the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the team key)",
		func() (map[string]bool, error) {
			settings, err := tx.Orgs.GetSettings(ctx, orgID)
			if err != nil {
				return nil, fmt.Errorf("read org linear workspace: %w", err)
			}
			// An org with no Linear workspace recorded reads as no rows, so
			// every added entry is refused.
			rules, err := tx.LinearTeamRules.ListForTeam(ctx, teamID, domain.EntityScope("linear", settings))
			if err != nil {
				return nil, fmt.Errorf("read team tracked linear teams: %w", err)
			}
			tracked := make(map[string]bool, len(rules))
			for _, r := range rules {
				tracked[r.LinearTeamID] = true
			}
			return tracked, nil
		})
}
