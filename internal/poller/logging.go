package poller

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the poller package (see internal/logging). "poller"
// covers scheduler/config-load lifecycle shared by both sources; "github" and
// "jira/poller" cover their respective per-org poll cycles; "github-groups"
// covers the GitHub-team mapping reconcile floor run each GitHub cycle.
var (
	pollerLog       = logging.Component("poller")
	githubLog       = logging.Component("github")
	jiraLog         = logging.Component("jira/poller")
	githubGroupsLog = logging.Component("github-groups")
)
