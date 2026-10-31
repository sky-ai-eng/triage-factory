package poller

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the poller package (see internal/logging). "poller"
// covers scheduler/config-load lifecycle shared by every source; "github",
// "jira/poller" and "linear/poller" cover their per-org poll cycles; "github-groups"
// covers the GitHub-team mapping reconcile floor run each GitHub cycle.
var (
	pollerLog       = logging.Component("poller")
	githubLog       = logging.Component("github")
	jiraLog         = logging.Component("jira/poller")
	linearLog       = logging.Component("linear/poller")
	githubGroupsLog = logging.Component("github-groups")
)
