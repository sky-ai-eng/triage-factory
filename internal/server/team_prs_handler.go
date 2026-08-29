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

	var req prListRequest
	if !httpx.DecodeJSONStrict(w, r, &req) {
		return
	}
	var v httpx.Validation
	states := canonicalPRStates(&v, req.States)
	page := httpx.ResolvePage(&v, req.PageRequest, prListFingerprint("team-prs", teamID, states), 0)
	if v.Flush(w, http.StatusBadRequest) {
		return
	}

	var (
		prs   []domain.PRSummaryRow
		total int
	)
	if err := s.tx.WithTx(r.Context(), orgID, userID, func(tx db.TxStores) error {
		// Member identities are host-scoped, so the org's configured GitHub
		// host rides into the read — raw, exactly as the roster passes it; the
		// store resolves it, since an unset setting has to look under the
		// public host rather than under "".
		orgSet, e := tx.Orgs.GetSettings(r.Context(), orgID)
		if e != nil {
			return e
		}
		prs, total, e = tx.TeamPRs.TeamPRs(r.Context(), orgID, teamID, orgSet.GitHubBaseURL,
			db.PRListFilter{States: states},
			db.ListOpts{Limit: page.Limit, Offset: page.Offset, CountOnly: page.CountOnly})
		return e
	}); err != nil {
		internalError(w, "teams/prs", err)
		return
	}
	httpx.WriteList(w, page, prs, total)
}
