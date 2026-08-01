package agentloop

import (
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop/tooldefs"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ToolStopBlueprint is the flow-control tool's wire name, re-exported so the
// engine and its tests name it without reaching past the registry.
const ToolStopBlueprint = tooldefs.StopBlueprintName

// SandboxTools returns the jail-implemented tool definitions in the
// harness's own order.
func SandboxTools() []schemas.ChatTool {
	return tooldefs.ChatTools(tooldefs.Sandbox())
}

// flowControlTools returns the loop-side tool definitions for this
// conversation — none when there is no blueprint to control.
func flowControlTools(hasBlueprint bool) []schemas.ChatTool {
	return tooldefs.ChatTools(tooldefs.FlowControl(hasBlueprint))
}

// resolveLoopSide answers a call without leaving the orchestrator, reporting
// false for anything the jail owns.
//
// hasBlueprint mirrors registration, so a name means nothing where the tool
// was never offered: a hallucinated call falls through to the sandbox and
// comes back "unknown tool", which is the right answer.
func resolveLoopSide(call domain.ToolCall, hasBlueprint bool) (tooldefs.Outcome, bool) {
	tool, ok := tooldefs.LoopSide(call.Name, hasBlueprint)
	if !ok {
		return tooldefs.Outcome{}, false
	}
	return tool.Handler(call.Input), true
}
