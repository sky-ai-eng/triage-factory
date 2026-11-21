package server

import (
	"net/http"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// --------------------------------------------------------------------
// POST /api/teams/{team_id}/prs/list — the team's pull requests.
//
// Two consumers, one read. The PR page's team view is the list; the Overview's
// OPEN PRS figure is the same list's count-only read (`{"states":["open"],
// "page_size":0}` → total_count), which is what makes the figure and the page
// agree by construction rather than by two queries staying in step.
//
// The population is the team's tracked repos, narrowed to what its members
// authored or what TF opened on its behalf — see db.TeamPRStore. Rows are the
// dashboard's own projection (domain.PRSummaryRow), because "a pull request,
// summarized" does not become a different thing when a team asks.
//
// Member-gated on the /activity audience line: TeamIDFromPath + VerifyTeamInOrg
// puts a team outside the caller's org at 404 (disclosing nothing), and the
// store's tracked-set semi-join is bounded by team_github_repos RLS, so an org
// member who is not on the team reads an empty list rather than the team's
// work. Deliberately not admin-gated: these are the team page's figures, and
// the team page is every member's — spend stays on /usage behind its own gate.
//
// It is the PR resource addressed at a team, not an entities browser: a
// generic entities list stays deliberately unbuilt.
// --------------------------------------------------------------------

var opTeamPRList = httpx.NewListOp[prListRequest, domain.PRSummaryRow](httpx.OpSpec{
	Pattern:     "POST /api/teams/{team_id}/prs/list",
	ID:          "list_team_pull_requests",
	Summary:     "List a team's pull requests",
	Description: "Returns one page of the pull requests in the team's tracked repositories that a member of the team opened or that Triage Factory opened for the team, narrowed by states and draft, and ordered by sort_key or, without one, by when each was last polled, newest first. A caller who is not on the team gets an empty list.",
	PathParams:  map[string]string{"team_id": teamIDPathDoc},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

func (s *Server) handleTeamPRList(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.requireOrg(w, r)
	if !ok {
		return
	}
	userID := ClaimsFrom(r.Context()).Subject
	teamID, ok := s.az.TeamIDFromPath(w, r, "teams/prs", orgID, userID)
	if !ok {
		return
	}
	if !s.az.VerifyTeamInOrg(w, r, orgID, userID, teamID) {
		return
	}

	req, ok := opTeamPRList.Decode(w, r)
	if !ok {
		return
	}
	var v httpx.Validation
	filter := resolvePRListFilter(&v, req)
	page := httpx.ResolvePage(&v, req.PageRequest, prListFingerprint("team-prs", teamID, filter), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		prs   []domain.PRSummaryRow
		total int
	)
	if err := s.tx.WithReadTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Member identities are host-scoped, so the org's configured GitHub
		// host rides into the read — raw, exactly as the roster passes it; the
		// store canonicalizes it.
		orgSet, e := tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return e
		}
		prs, total, e = tx.TeamPRs.TeamPRs(r.Context(), orgID, teamID, orgSet.GitHubBaseURL,
			filter,
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "teams/prs", err)
		return
	}
	opTeamPRList.Write(w, page, prs, total)
}

func (s *Server) registerTeamPRRoutes() {
	// The team's pull requests — the PR page's team arm, and (as its
	// count-only read) the Overview's OPEN PRS figure. Same member-level
	// gate as the flow node beside it.
	s.op(opTeamPRList, s.handleTeamPRList)
}
