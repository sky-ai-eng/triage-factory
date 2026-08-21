package slack

import (
	_ "embed"

	"github.com/sky-ai-eng/triage-factory/internal/agentprompt"
)

// slackToolsReference is the agent-facing docs for the `exec slack` verb
// family. Registered against agentprompt.ToolsReferenceFor("slack") so
// Spawner.toolsReferenceFor (internal/delegate/delegate.go) can compose it in
// via agentprompt.ToolsReferenceForSources whenever the run's org has Slack
// available — see that function's doc. Carries {{BINARY_PATH}}/{{RUN_URL}}
// placeholders like the core GitHub/Jira references; the same
// {{TOOLS_REFERENCE}} pre-pass resolves them.
//
//go:embed prompts/slack.txt
var slackToolsReference string

func init() {
	agentprompt.RegisterToolsReference("slack", slackToolsReference)
}
