package delegate

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the delegate package (see internal/logging).
// "delegate" covers the spawner/run lifecycle; "jira" covers the board →
// Jira lifecycle mirror; "blueprint" covers the blueprint orchestrator's
// finalize/cancel/resume paths; "dispatch" covers the run-queue dispatcher
// and the blueprint state-machine reactor.
var (
	delegateLog  = logging.Component("delegate")
	jiraLog      = logging.Component("jira")
	blueprintLog = logging.Component("blueprint")
	dispatchLog  = logging.Component("dispatch")
)
