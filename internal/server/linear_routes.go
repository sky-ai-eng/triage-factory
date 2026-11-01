package server

func (s *Server) registerLinearRoutes() {
	// The org's Linear catalog — its teams, and each team's workflow states —
	// proxied live under the org's Linear service credential and addressed at
	// the org, so a caller in several orgs reads each without moving their
	// active org. Member-gated; see the handler file.
	s.apiMutating("POST /api/orgs/{org_id}/linear/teams/list", s.handleLinearTeamsList)
	s.api("GET /api/orgs/{org_id}/linear/teams/{linear_team_id}", s.handleLinearTeamGet)
	s.apiMutating("POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/list", s.handleLinearStatesList)
	s.api("GET /api/orgs/{org_id}/linear/teams/{linear_team_id}/states/{state_id}", s.handleLinearStateGet)
	// The labels an issue in a Linear team can carry, which Linear event
	// predicates name by id; see linear_labels_handler.go.
	s.apiMutating("POST /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/list", s.handleLinearLabelsList)
	s.api("GET /api/orgs/{org_id}/linear/teams/{linear_team_id}/labels/{label_id}", s.handleLinearLabelGet)
	// The workspace's people, whom Linear event predicates name by user id;
	// see linear_users_handler.go.
	s.apiMutating("POST /api/orgs/{org_id}/linear/users/list", s.handleLinearUsersList)
	s.api("GET /api/orgs/{org_id}/linear/users/{linear_user_id}", s.handleLinearUserGet)

	// The org's Linear credential, the same pair again (linear_access.go),
	// plus the status read its settings card and the setup wizard render.
	s.api("GET /api/orgs/{org_id}/linear/access", s.handleLinearAccessGet)
	s.apiMutating("PUT /api/orgs/{org_id}/linear/access/credential", s.handleLinearCredentialPut)
	s.apiMutating("DELETE /api/orgs/{org_id}/linear/access/credential", s.handleLinearCredentialDelete)
	// Which tracked Linear teams the credential reads, and which are private:
	// the card's warning before a credential switch and its banner after one.
	s.api("GET /api/orgs/{org_id}/linear/access/team-visibility", s.handleLinearTeamVisibility)
	// The org's own Linear OAuth app (linear_app_handlers.go): the status read
	// the app card renders, and the admin's store and delete.
	s.api("GET /api/orgs/{org_id}/linear/app", s.handleLinearAppStatus)
	s.apiMutating("POST /api/orgs/{org_id}/linear/app", s.handleLinearAppImport)
	s.apiMutating("DELETE /api/orgs/{org_id}/linear/app", s.handleLinearAppDelete)
	// The workspace install ceremony (linear_install.go). Both legs are
	// top-level navigations — the start from the card, the callback from
	// Linear's redirect — so they ride s.api (session, no CSRF wrap) and carry
	// their own state-cookie CSRF defense. The callback is static because
	// Linear matches redirect URIs exactly and one app serves every org; its
	// org comes from the signed cookie. Both paths are registered on app
	// owners' Linear apps, so a rename breaks every existing app.
	s.api("GET /api/orgs/{org_id}/linear/install/start", s.handleLinearInstallStart)
	s.api("GET /api/linear/install/callback", s.handleLinearInstallCallback)
	// Per-user Linear access (linear_connect.go): the gate's status read, the
	// key paste, the user's own disconnect, and one-click Connect. Any org
	// member binds or withdraws their own. The Connect pair rides s.api for
	// the install pair's reasons, and its callback path is frozen the same
	// way: it is the second redirect URI registered on every Linear app.
	s.api("GET /api/orgs/{org_id}/linear/identity", s.handleLinearIdentityStatus)
	s.apiMutating("POST /api/orgs/{org_id}/linear/identity/api-key", s.handleLinearIdentityAPIKey)
	s.apiMutating("DELETE /api/orgs/{org_id}/linear/identity", s.handleLinearIdentityDelete)
	s.api("GET /api/orgs/{org_id}/linear/connect/start", s.handleLinearConnectStart)
	s.api("GET /api/linear/connect/callback", s.handleLinearConnectCallback)
}
