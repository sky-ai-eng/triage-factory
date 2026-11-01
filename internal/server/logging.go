package server

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the HTTP server package (see internal/logging). Each
// var carries a component= attribute that replaces the old "[prefix]" log
// tags. auth/server, jira/server and linear/server are scoped to distinguish
// them from the canonical auth, jira and linear packages that own those
// component names.
var (
	approvalDiscardLog = logging.Component("approval-discard")
	authLog            = logging.Component("auth/server")
	dashboardLog       = logging.Component("dashboard")
	delegateSpawnLog   = logging.Component("delegate-spawn")
	eventHandlersLog   = logging.Component("event_handlers")
	githubAccessLog    = logging.Component("github-access")
	githubAppLog       = logging.Component("github-app")
	githubConnectLog   = logging.Component("github-connect")
	githubGroupsLog    = logging.Component("github-groups")
	githubIdentityLog  = logging.Component("github-identity")
	invitesLog         = logging.Component("invites")
	jiraLog            = logging.Component("jira/server")
	jiraAppLog         = logging.Component("jira-app")
	jiraConnectLog     = logging.Component("jira-connect")
	jiraIdentityLog    = logging.Component("jira-identity")
	jiraRuleLog        = logging.Component("jira-rule")
	linearAppLog       = logging.Component("linear-app")
	linearConnectLog   = logging.Component("linear-connect")
	linearInstallLog   = logging.Component("linear-install")
	linearLog          = logging.Component("linear/server")
	linearRuleLog      = logging.Component("linear-rule")
	membershipLog      = logging.Component("membership")
	orgsLog            = logging.Component("orgs")
	reposLog           = logging.Component("repos")
	seatsLog           = logging.Component("license/seats")
	serverLog          = logging.Component("server")
	settingsLog        = logging.Component("settings")
	settingsOrgLog     = logging.Component("settings/org")
	setupLog           = logging.Component("setup")
	stockLog           = logging.Component("stock")
	taskActionLog      = logging.Component("task-action")
	tasksLog           = logging.Component("tasks")
	workLog            = logging.Component("work")
	teamsLog           = logging.Component("teams")
)
