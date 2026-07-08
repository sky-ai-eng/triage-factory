package slack

import (
	_ "embed"

	"github.com/sky-ai-eng/triage-factory/internal/ai"
)

// slackToolsReference is the agent-facing docs for the `exec slack` verb
// family (TFAC-596). Registered against ai.ToolsReferenceFor("slack") so
// Spawner.setupSlack (internal/delegate/delegate.go) layers it in after
// ai.GHToolsTemplate for every Slack-thread run — see that function's doc.
// Carries {{BINARY_PATH}}/{{RUN_URL}} placeholders like GHToolsTemplate/
// JiraToolsTemplate; the same {{TOOLS_REFERENCE}} pre-pass resolves them.
//
//go:embed prompts/slack.txt
var slackToolsReference string

func init() {
	ai.RegisterToolsReference("slack", slackToolsReference)
}
