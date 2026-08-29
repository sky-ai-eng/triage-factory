package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// The pull-request list's shared half.
//
// Two routes answer it — POST /api/dashboard/prs/list (mine) and
// POST /api/teams/{team_id}/prs/list (the team's) — over one row shape and one
// filter vocabulary. They differ only in whose pull requests they select, so
// the body, the validation and the token's filter key live here rather than
// twice: a states filter that accepted a value on one route and rejected it on
// the other would be the same word answering two questions.
// --------------------------------------------------------------------

// prListRequest is the body both routes take: the states filter plus the
// shared paging fields. `{}` is "every state, first page".
type prListRequest struct {
	// States narrows to a subset of open|merged|closed. Empty = all, like
	// every other narrowing filter. Validated against domain.PRListStates;
	// an unknown value is a client fault, not an empty page.
	States []string `json:"states"`

	httpx.PageRequest
}

// prListFilterKey is the canonicalized form the page token is fingerprinted
// against. Scope and TeamID are in it because the fingerprint's job is to stop
// a token addressing a result set it wasn't minted for: the two routes page
// different populations, and so does the team route for two different teams,
// so an offset from one of them is meaningless in the others.
type prListFilterKey struct {
	Scope  string   `json:"scope"`
	TeamID string   `json:"team_id,omitempty"`
	States []string `json:"states"`
}

// canonicalPRStates validates the states filter and returns it canonicalized
// (sorted, deduped) — the form the store takes and the fingerprint is computed
// over. Faults are appended to v, which the caller flushes with the rest of
// its field errors so one response reports every problem.
func canonicalPRStates(v *httpx.Validation, states []string) []string {
	out := canonicalStrings(states)
	for _, st := range out {
		if !slices.Contains(domain.PRListStates, st) {
			v.Invalid("states", fmt.Sprintf("unknown state %q; must be one of: %s",
				st, strings.Join(domain.PRListStates, ", ")))
		}
	}
	return out
}

// prListFingerprint is the filter fingerprint a pull-request list's page token
// is minted against. Each handler still calls httpx.ResolvePage itself — the
// page window is the route's own business, and a route that resolved it behind
// a helper would be one the list-contract ratchet cannot see.
func prListFingerprint(scope, teamID string, states []string) string {
	return httpx.FilterFingerprint(prListFilterKey{Scope: scope, TeamID: teamID, States: states})
}
