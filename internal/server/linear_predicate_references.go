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
	priorIn, err := predicateLinearTeamIn(prior)
	if err != nil {
		return nil, err
	}
	nextIn, err := predicateLinearTeamIn(predicate)
	if err != nil {
		return nil, err
	}
	addedNow := addedEntries(priorIn.LinearTeamIDIn, nextIn.LinearTeamIDIn)
	addedOld := addedEntries(priorIn.OldLinearTeamIDIn, nextIn.OldLinearTeamIDIn)
	if len(addedNow) == 0 && len(addedOld) == 0 {
		return nil, nil
	}

	settings, err := tx.Orgs.GetSettings(ctx, orgID)
	if err != nil {
		return nil, fmt.Errorf("read org linear workspace: %w", err)
	}
	// An org with no Linear workspace recorded reads as no rows, so every
	// added entry is refused.
	rules, err := tx.LinearTeamRules.ListForTeam(ctx, teamID, domain.EntityScope("linear", settings))
	if err != nil {
		return nil, fmt.Errorf("read team tracked linear teams: %w", err)
	}
	tracked := make(map[string]bool, len(rules))
	for _, r := range rules {
		tracked[r.LinearTeamID] = true
	}
	var faults []string
	report := func(field string, ids []string) {
		for _, id := range ids {
			if !tracked[id] {
				faults = append(faults, fmt.Sprintf("%s: %q is not the id of a Linear team this team tracks "+
					"(it takes the id of an entry in linear_teams on GET /api/teams/{team_id}/settings, not the team key)", field, id))
			}
		}
	}
	report("linear_team_id_in", addedNow)
	report("old_linear_team_id_in", addedOld)
	return faults, nil
}

// linearTeamIn is the two Linear team filters a predicate can carry.
// old_linear_team_id_in is identifier_changed's alone.
type linearTeamIn struct {
	LinearTeamIDIn    []string `json:"linear_team_id_in"`
	OldLinearTeamIDIn []string `json:"old_linear_team_id_in"`
}

// predicateLinearTeamIn reads the Linear team filters from a Linear
// predicate; both are nil for match-all.
func predicateLinearTeamIn(predicate *string) (linearTeamIn, error) {
	var p linearTeamIn
	if predicate == nil {
		return p, nil
	}
	if err := json.Unmarshal([]byte(*predicate), &p); err != nil {
		return p, fmt.Errorf("decode linear predicate: %w", err)
	}
	return p, nil
}

// addedEntries returns the entries of next that prior does not hold, each
// once, in next's order. The stored entries are not re-checked, so a reference
// that went stale after it was stored never blocks an edit that keeps it.
func addedEntries(prior, next []string) []string {
	seen := make(map[string]bool, len(prior))
	for _, id := range prior {
		seen[id] = true
	}
	var added []string
	for _, id := range next {
		if !seen[id] {
			seen[id] = true
			added = append(added, id)
		}
	}
	return added
}
