package server

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the HTTP server package (see internal/logging). Each
// var carries a component= attribute that replaces the old "[prefix]" log
// tags. auth/server and jira/server are scoped to distinguish them from the
// canonical auth and jira packages that own those component names.
var (
	approvalDiscardLog = logging.Component("approval-discard")
	artifactsLog       = logging.Component("artifacts")
	authLog            = logging.Component("auth/server")
	dashboardLog       = logging.Component("dashboard")
	delegateSpawnLog   = logging.Component("delegate-spawn")
	eventHandlersLog   = logging.Component("event_handlers")
	externalActionLog  = logging.Component("external-action")
	failedEventsLog    = logging.Component("failed-events")
	factoryLog         = logging.Component("factory")
	githubAccessLog    = logging.Component("github-access")
	githubAppLog       = logging.Component("github-app")
	githubConnectLog   = logging.Component("github-connect")
	githubGroupsLog    = logging.Component("github-groups")
	githubIdentityLog  = logging.Component("github-identity")
	headlessLog        = logging.Component("headless")
	invitesLog         = logging.Component("invites")
	jiraLog            = logging.Component("jira/server")
	jiraAppLog         = logging.Component("jira-app")
	jiraConnectLog     = logging.Component("jira-connect")
	jiraIdentityLog    = logging.Component("jira-identity")
	jiraRuleLog        = logging.Component("jira-rule")
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
	teamsLog           = logging.Component("teams")
)
