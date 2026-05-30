package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// errActingTeamRequired is returned when the caller belongs to two or
// more teams, supplied no explicit pick, and has no usable sticky
// default — the write is ambiguous and the picker (required in the UI
// whenever ≥2 teams are visible) should have supplied team_id. Handlers
// map it to 400.
var errActingTeamRequired = errors.New("a team is required: you belong to multiple teams, so select one")

// errActingTeamForbidden is returned when the caller explicitly picks a
// team they are not a member of (in the request org). The UI only ever
// offers the caller's own teams, so this is a malformed/forged
// selection; handlers map it to 400.
var errActingTeamForbidden = errors.New("invalid team selection: you are not a member of the selected team")

// isActingTeamSelectionError reports whether err is a caller-fault team
// selection problem (→ 400) versus an internal failure (→ 500). Handlers
// use it to map the error surfaced out of the WithTx closure.
func isActingTeamSelectionError(err error) bool {
	return errors.Is(err, errActingTeamRequired) || errors.Is(err, errActingTeamForbidden)
}

// writeIfActingTeamError writes a 400 with the error's message and
// returns true when err is a caller-fault team-selection error; returns
// false otherwise so the caller falls through to its normal (500)
// handling. Lets every team-stamping handler map the picker errors with
// one line after its WithTx returns.
func writeIfActingTeamError(w http.ResponseWriter, err error) bool {
	if isActingTeamSelectionError(err) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return true
	}
	return false
}

// resolveActingTeam returns the team a write should be attributed to for
// the current request — "the acting team" — and records it as the
// caller's last-written team so the write pickers seed there next time.
// It centralizes the team selection that the multi-team UX layers on top
// of every write site, so the resolution order lives in one place.
//
// Reads and writes are deliberately decoupled: a read is a multi-team
// *view scope* (the board can show many teams at once), while a write is
// always a single-team *ownership stamp*. So this resolver never consults
// the read filter, and the read filter never consults this — they share
// no primitive.
//
// Resolution order (see resolveActingTeamID); on success the resolved
// team is stamped onto users.preferred_team_id as the durable last-used
// write default. The stamp is best-effort — a preference-write failure
// must not fail the actual write — and runs under the caller's claims
// (users_modify RLS gates id = current_user_id()). Call this inside the
// request's WithTx.
func resolveActingTeam(ctx context.Context, teams db.TeamsStore, users db.UsersStore, orgID, userID, picked string) (string, error) {
	resolved, err := resolveActingTeamID(ctx, teams, users, orgID, userID, picked)
	if err != nil {
		return "", err
	}
	if resolved != "" {
		// Best-effort: remember the team this write landed on as the
		// user's last-used default. Ignore the error — the write itself
		// has already been decided and must not fail on a preference miss
		// (e.g. a missing user row in a thin fixture).
		_ = users.SetPreferredTeam(ctx, userID, resolved)
	}
	return resolved, nil
}

// resolveActingTeamID is the pure resolution (no side effects), kept
// separate so the contract is unit-testable without the last-used stamp.
//
// Resolution order:
//
//  1. Explicit pick — the team_id the write-time picker supplies. The
//     picker is *required* in the UI whenever the caller belongs to ≥2
//     teams, so for a multi-team caller this is the normal path. The
//     pick is validated against the caller's current-org teams; a pick
//     for a team they don't belong to is rejected (errActingTeamForbidden)
//     rather than silently honored.
//
//  2. Last-written default — users.preferred_team_id, when set and still
//     one of the caller's current-org teams. This is the team the
//     caller's previous write landed on (stamped by resolveActingTeam);
//     it only applies when no explicit pick arrived (a ≤1-team caller,
//     whose picker is hidden, or a non-UI client). A stale value (team
//     deleted, or in another org) fails the membership re-check and
//     falls through.
//
//  3. Sole team — when the caller has exactly one team, that team. This
//     is the only path a solo (local or hosted-1-team) caller ever
//     takes, and it is byte-identical to the pre-PR1 hardcoded default:
//     ListForUser orders oldest-first, and a 1-team org's only team is
//     its default team.
//
//  4. Ambiguous (≥2 teams, no pick, no usable default) → errActingTeamRequired.
//     The UI cannot reach this (the picker is required), so it only
//     fires for a malformed non-UI write; failing loudly beats guessing
//     a team that could misattribute shared work.
//
//  5. Zero teams (a member of the org but of no team — a bootstrap edge
//     that shouldn't occur in practice) → fall back to the org's default
//     team, preserving PR1's never-block posture; a truly teamless org
//     still errors.
func resolveActingTeamID(ctx context.Context, teams db.TeamsStore, users db.UsersStore, orgID, userID, picked string) (string, error) {
	myTeams, err := teams.ListForUser(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("acting team: list teams: %w", err)
	}
	member := make(map[string]struct{}, len(myTeams))
	for _, t := range myTeams {
		member[t.ID] = struct{}{}
	}

	// 1. Explicit pick.
	if picked != "" {
		if _, ok := member[picked]; ok {
			return picked, nil
		}
		return "", errActingTeamForbidden
	}

	// 2. Last-written default (only when still a member).
	preferred, err := users.GetPreferredTeam(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("acting team: preferred team: %w", err)
	}
	if preferred != "" {
		if _, ok := member[preferred]; ok {
			return preferred, nil
		}
	}

	// 3 + 4. One team → it; ≥2 → ambiguous.
	switch {
	case len(myTeams) == 1:
		return myTeams[0].ID, nil
	case len(myTeams) >= 2:
		return "", errActingTeamRequired
	}

	// 5. No team membership — fall back to the org's default team rather
	// than block (mirrors PR1). A genuinely teamless org still errors.
	teamID, err := teams.GetDefaultForOrg(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("acting team lookup: %w", err)
	}
	if teamID == "" {
		return "", fmt.Errorf("org %s has no team", orgID)
	}
	return teamID, nil
}

// resolveReadTeam picks the single team a single-team-oriented *read*
// should target, given an optional explicit pick from the caller. The
// Jira stock/discovery deck is the only consumer: it scopes to one
// team's Jira projects at a time (and the same team owns the claim), so
// it is single-team by construction — unlike the board/queue/factory
// reads, which are multi-team and use the IN-list narrow instead.
//
// Unlike resolveActingTeam this never errors on a multi-team caller —
// a read defaults rather than blocks. Order: valid pick → last-written
// default (still a member) → first (oldest) team. Returns "" with a nil
// error only when the caller has no teams (the handler degrades to its
// "not configured" branch). An explicit pick the caller isn't a member
// of is ignored (falls through to the default) rather than erroring,
// since a stale filter value should never 4xx a read.
func resolveReadTeam(ctx context.Context, teams db.TeamsStore, users db.UsersStore, orgID, userID, picked string) (string, error) {
	myTeams, err := teams.ListForUser(ctx, orgID)
	if err != nil {
		return "", fmt.Errorf("read team: list teams: %w", err)
	}
	member := make(map[string]struct{}, len(myTeams))
	for _, t := range myTeams {
		member[t.ID] = struct{}{}
	}
	if picked != "" {
		if _, ok := member[picked]; ok {
			return picked, nil
		}
	}
	preferred, err := users.GetPreferredTeam(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("read team: preferred team: %w", err)
	}
	if preferred != "" {
		if _, ok := member[preferred]; ok {
			return preferred, nil
		}
	}
	if len(myTeams) > 0 {
		return myTeams[0].ID, nil
	}
	return "", nil
}
