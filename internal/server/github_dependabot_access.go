package server

import (
	"net/http"
)

// dependabotAccessJSON counts the org's tracked repositories on its GitHub
// host by whether its credential can read their Dependabot alerts. Unchecked
// is the tracked repositories the poll has not probed yet, or that no
// credential of the org reads.
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
	c, err := s.dependabotAccess.CountTrackedSystem(ctx, orgID, host)
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
