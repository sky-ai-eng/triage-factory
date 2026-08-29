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

// resolvePRListPage validates the states filter and resolves the page against
// a fingerprint over the canonicalized filter set. Faults are appended to v,
// which the caller flushes with the rest of its field errors, so one response
// reports every problem. The returned states are canonical (sorted, deduped) —
// the form the store takes and the fingerprint was computed over.
func resolvePRListPage(v *httpx.Validation, req prListRequest, scope, teamID string) ([]string, httpx.Page) {
	states := canonicalStrings(req.States)
	for _, st := range states {
		if !slices.Contains(domain.PRListStates, st) {
			v.Invalid("states", fmt.Sprintf("unknown state %q; must be one of: %s",
				st, strings.Join(domain.PRListStates, ", ")))
		}
	}
	page := httpx.ResolvePage(v, req.PageRequest, httpx.FilterFingerprint(prListFilterKey{
		Scope: scope, TeamID: teamID, States: states,
	}), 0)
	return states, page
}
