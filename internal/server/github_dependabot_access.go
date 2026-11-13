package server

import (
	"net/http"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// dependabotAccessJSON counts the org's tracked repositories on its GitHub
// host by whether its credential can read their Dependabot alerts. Unchecked
// is the tracked repositories with no current answer: the poll has not
// probed them yet, or no credential of the org has read them for longer than
// domain.DependabotAccessStaleAfter.
type dependabotAccessJSON struct {
	Readable     int `json:"readable"`
	NoPermission int `json:"no_permission"`
	Disabled     int `json:"disabled"`
	Unchecked    int `json:"unchecked"`
}

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
	writeJSON(w, http.StatusOK, dependabotAccessJSON{
		Readable:     c.Readable,
		NoPermission: c.NoPermission,
		Disabled:     c.Disabled,
		Unchecked:    c.Unchecked,
	})
}
