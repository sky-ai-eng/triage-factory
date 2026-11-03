package server

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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

// prListRequest is the body both routes take: the narrowing fields plus the
// shared paging fields. `{}` is "every pull request, last polled first, first
// page".
type prListRequest struct {
	// States narrows to a subset of open|merged|closed. Empty = all, like
	// every other narrowing filter. Validated against domain.PRListStates;
	// an unknown value is a client fault, not an empty page.
	States []string `json:"states"`
	// Draft narrows to the row's draft flag: true keeps the drafts, false
	// the pull requests ready for review. Absent (or null) is both. A value
	// that is not a JSON boolean is a 400 naming draft.
	Draft *bool `json:"draft"`
	// SortKey / SortDir order the page (prListSortKeys). Absent is the
	// default order, last polled first.
	SortKey string `json:"sort_key"`
	SortDir string `json:"sort_dir"`

	httpx.PageRequest
}

// prListSortKeys is the pull-request lists' sort_key vocabulary. updated_at
// is GitHub's last-activity time, newest first unless sort_dir says
// otherwise; db.PRListFilter.Sort says where a pull request with none sorts.
var prListSortKeys = []httpx.SortKey{{Name: db.PRSortUpdatedAt, Default: db.SortDesc}}

// prListFilterKey is the canonicalized form the page token is fingerprinted
// against. Scope and TeamID are in it because the fingerprint's job is to stop
// a token addressing a result set it wasn't minted for: the two routes page
// different populations, and so does the team route for two different teams,
// so an offset from one of them is meaningless in the others. The sort is in
// it because an offset addresses a position in one ordering.
type prListFilterKey struct {
	Scope  string   `json:"scope"`
	TeamID string   `json:"team_id,omitempty"`
	States []string `json:"states"`
	Draft  *bool    `json:"draft"`
	Sort   db.Sort  `json:"sort"`
}

// resolvePRListFilter validates the body's narrowing fields and returns the
// filter the store takes, canonicalized (states sorted and deduped) so it is
// also the form the fingerprint is computed over. Faults are appended to v,
// which the caller flushes with the rest of its field errors so one response
// reports every problem.
func resolvePRListFilter(v *httpx.Validation, req prListRequest) db.PRListFilter {
	states := httpx.CanonicalStrings(req.States)
	for _, st := range states {
		if !slices.Contains(domain.PRListStates, st) {
			v.Invalid("states", fmt.Sprintf("unknown state %q; must be one of: %s",
				st, strings.Join(domain.PRListStates, ", ")))
		}
	}
	return db.PRListFilter{
		States: states,
		Draft:  req.Draft,
		Sort:   httpx.Sort(v, req.SortKey, req.SortDir, prListSortKeys),
	}
}

// prListFingerprint is the filter fingerprint a pull-request list's page token
// is minted against. Each handler still calls httpx.ResolvePage itself — the
// page window is the route's own business, and a route that resolved it behind
// a helper would be one the list-contract ratchet cannot see.
func prListFingerprint(scope, teamID string, f db.PRListFilter) string {
	return httpx.FilterFingerprint(prListFilterKey{
		Scope: scope, TeamID: teamID, States: f.States, Draft: f.Draft, Sort: f.Sort,
	})
}
