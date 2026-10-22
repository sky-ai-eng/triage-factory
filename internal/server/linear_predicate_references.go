package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// init registers linearTeamInFaults for every Linear event type whose
// predicate has a linear_team_id_in field, and linearParentChangedFaults,
// which adds new_parent_id to it, for parent_changed. The events package
// registers its schemas in its own init, which runs before this one because
// this package imports it.
func init() {
	for eventType, schema := range events.All() {
		if !strings.HasPrefix(eventType, "linear:") {
			continue
		}
		for _, field := range schema.Fields {
			if field.Name != "linear_team_id_in" {
				continue
			}
			if eventType == domain.EventLinearIssueParentChanged {
				RegisterPredicateReferenceCheck(eventType, linearParentChangedFaults)
			} else {
				RegisterPredicateReferenceCheck(eventType, linearTeamInFaults)
			}
			break
		}
	}
}

// linearParentChangedFaults is parent_changed's check: linearTeamInFaults,
// and new_parent_id when the predicate gives it a value the stored one does
// not hold. That value must be a Linear issue id (a lowercase UUID), since the
// matcher compares it with the event's new_parent_id, the parent's UUID. An
// identifier such as "ENG-12", the value a caller would naturally reach for,
// is refused rather than stored as a filter that never fires. An empty value
// is allowed: it matches a parent being cleared.
func linearParentChangedFaults(ctx context.Context, tx db.TxStores, orgID, teamID string, predicate, prior *string) ([]string, error) {
	faults, err := linearTeamInFaults(ctx, tx, orgID, teamID, predicate, prior)
	if err != nil {
		return nil, err
	}
	next, err := predicateString(predicate, "new_parent_id")
	if err != nil {
		return nil, err
	}
	stored, err := predicateString(prior, "new_parent_id")
	if err != nil {
		return nil, err
	}
	if next != "" && next != stored && !isLinearID(next) {
		faults = append(faults, fmt.Sprintf("new_parent_id: %q is not a Linear issue id "+
			"(it takes the parent's UUID, the new_parent_id a parent_changed event carries, not its identifier)", next))
	}
	return faults, nil
}

// predicateString reads one string field from a predicate; "" for match-all
// or a predicate without the field.
func predicateString(predicate *string, field string) (string, error) {
	if predicate == nil {
		return "", nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(*predicate), &fields); err != nil {
		return "", fmt.Errorf("decode predicate: %w", err)
	}
	raw, ok := fields[field]
	if !ok {
		return "", nil
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("decode predicate %s: %w", field, err)
	}
	return v, nil
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
