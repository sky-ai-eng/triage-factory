package server

func (s *Server) registerJiraRoutes() {
	// The org's Jira catalog — its projects, and each project's workflow
	// statuses — proxied live under the org's Jira service credential and
	// addressed at the org, so a caller in several orgs reads each without
	// moving their active org. Member-gated; both lists are proxy lists, so
	// total_count is null. Its GitHub sibling reads a mirror instead; see the
	// handler file for why the two differ.
	s.apiMutating("POST /api/orgs/{org_id}/jira/projects/list", s.handleJiraProjectsList)
	s.api("GET /api/orgs/{org_id}/jira/projects/{jira_id}", s.handleJiraProjectGet)
	s.apiMutating("POST /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/list", s.handleJiraStatusesList)
	s.api("GET /api/orgs/{org_id}/jira/projects/{jira_id}/statuses/{status_id}", s.handleJiraStatusGet)
	// The instance's priorities and each project's issue types, which Jira
	// event predicates name by id. Proxied and member-gated like the routes
	// above; see jira_issue_fields_handler.go.
	s.apiMutating("POST /api/orgs/{org_id}/jira/priorities/list", s.handleJiraPrioritiesList)
	s.api("GET /api/orgs/{org_id}/jira/priorities/{priority_id}", s.handleJiraPriorityGet)
	s.apiMutating("POST /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/list", s.handleJiraIssueTypesList)
	s.api("GET /api/orgs/{org_id}/jira/projects/{jira_id}/issue-types/{issue_type_id}", s.handleJiraIssueTypeGet)

	// Per-user Jira access — the Jira sibling of the GitHub identity flow
	// (jira_connect.go). status reports connected from a STORED credential
	// (Jira's user level holds access, not just identity); the PAT path
	// validates the token, STORES it (per-user vault scope), and derives the
	// user's Jira identity. DC = paste-a-PAT; connect_available now reflects
	// whether an Atlassian OAuth app resolves (the Cloud Connect gate). Any
	// org member binds their own access.
	s.api("GET /api/orgs/{org_id}/jira/identity", s.handleJiraIdentityStatus)
	s.apiMutating("POST /api/orgs/{org_id}/jira/identity/pat", s.handleJiraIdentityPAT)

	// Per-user "Connect Jira" Cloud OAuth (3LO) — the one-click counterpart of
	// the paste path, gated on connect_available (an Atlassian app resolves).
	// start redirects to auth.atlassian.com/authorize; callback exchanges the
	// code, resolves the cloud_id, whoamis, and STORES the rotating refresh
	// token (source='connect_oauth'). Both are GETs reached via top-level
	// navigation, so they ride s.api (withSession, no CSRF wrap) and carry
	// their own OAuth state-cookie CSRF defense — the GitHub Connect pattern.
	//
	// Frozen for the same reason as the GitHub connect pair in
	// registerGitHubRoutes: the
	// callback is the redirect_uri registered on the Atlassian OAuth app,
	// external config an operator owns, so renaming it breaks Connect Jira
	// for every already-configured app until its owner updates it.
	s.api("GET /api/orgs/{org_id}/jira/connect/start", s.handleJiraConnectStart)
	s.api("GET /api/orgs/{org_id}/jira/connect/callback", s.handleJiraConnectCallback)

	// Per-org Atlassian OAuth (3LO) app config — the credential layer the
	// per-user "Connect Jira" flow above runs against. The Jira sibling of the
	// GitHub App import card: an admin enters a bring-your-own Atlassian app
	// (client_id + client_secret), which becomes the per-org override over the
	// deployment app (multi) / is the app itself (local). status is any-member
	// (the card renders for everyone); import + delete are admin (gated inside
	// the handler). The two mutators are JSON fetches from the SPA, so they
	// ride apiMutating (CSRF).
	s.api("GET /api/orgs/{org_id}/jira/app", s.handleJiraAppStatus)
	s.apiMutating("POST /api/orgs/{org_id}/jira/app", s.handleJiraAppImport)
	s.apiMutating("DELETE /api/orgs/{org_id}/jira/app", s.handleJiraAppDelete)
}
