package server

import (
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestMovedRoutes_OldPathsAreGone pins the half of a route move that is easy to
// skip: deleting the address it moved from. An alias kept "briefly" is a second
// way to do one thing, and the pair drifts — so every path here must 404, and a
// caller that hasn't been updated learns it at the door rather than by reading
// stale data from a route nobody maintains.
//
// The org and team segments carry the local sentinels because that is what a
// local-mode caller would address; the point is that the PATH doesn't resolve,
// not that the scope doesn't exist.
func TestMovedRoutes_OldPathsAreGone(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	org := "/api/orgs/" + runmode.LocalDefaultOrgID
	team := runmode.LocalDefaultTeamID

	for _, c := range []struct{ method, path string }{
		// The workspace-create route: local provisioning is POST /api/orgs.
		{http.MethodPost, "/api/setup/start"},
		// The team-settings siblings, now child collections of the team.
		{http.MethodGet, "/api/settings/team/" + team + "/repos"},
		{http.MethodPut, "/api/settings/team/" + team + "/repos"},
		{http.MethodGet, "/api/settings/team/" + team + "/github-groups"},
		{http.MethodPut, "/api/settings/team/" + team + "/github-groups"},
		// The caller's own settings, now under /api/me.
		{http.MethodGet, "/api/settings/user"},
		{http.MethodPost, "/api/settings/user"},
		// The vestigial `access` grouping node under github.
		{http.MethodPut, org + "/github/access/pat"},
		{http.MethodDelete, org + "/github/access/pat"},
		{http.MethodPost, org + "/github/access/pat-preflight"},
		{http.MethodPost, org + "/github/access/switch-to-pat"},
		// Skills were never a separate row; the imports write prompts.
		{http.MethodPost, "/api/skills/upload"},
		{http.MethodPost, "/api/skills/import"},
		// Usage, now addressed by the scope each read reports on.
		{http.MethodGet, "/api/usage/me"},
		{http.MethodGet, "/api/usage/org"},
		{http.MethodGet, "/api/usage/org/ops"},
		{http.MethodPost, "/api/usage/org/access-log/list"},
		{http.MethodPost, "/api/usage/org/actions/list"},
		{http.MethodPost, "/api/usage/org/artifacts/list"},
		{http.MethodPost, "/api/usage/org/team-caps/list"},
		{http.MethodGet, "/api/usage/teams/" + team},
		{http.MethodPost, "/api/usage/teams/" + team + "/actions/list"},
		{http.MethodPost, "/api/usage/teams/" + team + "/artifacts/list"},
		{http.MethodPut, "/api/usage/teams/" + team + "/cap"},
		// The factory drop is POST /api/tasks + POST /api/tasks/{id}/delegate.
		{http.MethodPost, "/api/factory/delegate"},
	} {
		rec := doJSON(t, s, c.method, c.path, map[string]any{})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 (route moved)", c.method, c.path, rec.Code)
		}
	}
}

// TestMovedRoutes_NewPathsAnswer is the other half: each destination is
// mounted and reachable. It asserts only "not 404" — what each route answers is
// its own test's business — so this catches a move that deleted an address
// without landing the new one.
func TestMovedRoutes_NewPathsAnswer(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	org := "/api/orgs/" + runmode.LocalDefaultOrgID
	team := "/api/teams/" + runmode.LocalDefaultTeamID

	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/api/orgs"},
		{http.MethodGet, team + "/github-repos"},
		{http.MethodGet, team + "/github-groups"},
		{http.MethodGet, "/api/me/settings"},
		{http.MethodPatch, "/api/me/settings"},
		{http.MethodGet, "/api/me/usage"},
		{http.MethodGet, org + "/usage"},
		{http.MethodGet, org + "/usage/ops"},
		{http.MethodGet, team + "/usage"},
	} {
		rec := doJSON(t, s, c.method, c.path, map[string]any{})
		if rec.Code == http.StatusNotFound {
			t.Errorf("%s %s = 404; the route should be mounted here now", c.method, c.path)
		}
	}
}
