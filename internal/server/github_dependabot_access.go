package server

import (
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// dependabotAccessJSON counts the org's tracked repositories on its GitHub
// host by whether its credential can read their Dependabot alerts. Unchecked
// is the tracked repositories with no current answer: the poll has not
// probed them yet, or no credential of the org has read them for longer than
// domain.DependabotAccessStaleAfter.
type dependabotAccessJSON struct {
	Readable     int `json:"readable" doc:"How many tracked repositories the organization's GitHub credential can read Dependabot alerts on."`
	NoPermission int `json:"no_permission" doc:"How many tracked repositories the credential lacks the permission to read Dependabot alerts on."`
	Disabled     int `json:"disabled" doc:"How many tracked repositories have Dependabot alerts turned off."`
	Unchecked    int `json:"unchecked" doc:"How many tracked repositories have no current answer, because polling has not checked them yet or has not read them recently."`
}

var opGitHubDependabotAccess = httpx.NewJSONOp[httpx.NoBody, dependabotAccessJSON](httpx.OpSpec{
	Pattern:     "GET /api/orgs/{org_id}/github/dependabot-access",
	ID:          "get_github_dependabot_access",
	Summary:     "Count Dependabot alert access on tracked repositories",
	Description: "Counts the repositories the organization's teams track on its GitHub host by whether its GitHub credential can read their Dependabot alerts, as polling last found them.",
	PathParams:  map[string]string{"org_id": "The organization's id."},
	Hints:       httpx.Hints{ReadOnly: httpx.HintYes, OpenWorld: httpx.HintNo},
})

// handleGitHubDependabotAccess serves the counts. Org-admin, because the
// counts describe the org's credential, which only an admin can change, and
// they cover every team's tracked repositories. An org with no GitHub URL
// tracks nothing anyone could count, and is a 409 NOT_CONFIGURED.
//
// GET /api/orgs/{org_id}/github/dependabot-access
func (s *Server) handleGitHubDependabotAccess(w http.ResponseWriter, r *http.Request) {
	orgID, _, ok := s.az.RequireOrgAdmin(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	host, ok := s.requireOrgGitHubHost(ctx, w, orgID, "dependabot-access")
	if !ok {
		return
	}
	// System read: the admin gate above authorized orgID.
	c, err := s.dependabotAccess.CountTrackedSystem(ctx, orgID, host, time.Now().Add(-domain.DependabotAccessStaleAfter))
	if err != nil {
		internalError(w, "dependabot-access", err)
		return
	}
	opGitHubDependabotAccess.Write(w, dependabotAccessJSON{
		Readable:     c.Readable,
		NoPermission: c.NoPermission,
		Disabled:     c.Disabled,
		Unchecked:    c.Unchecked,
	})
}
