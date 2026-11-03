package agentloop

import (
	"context"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/tooldefs"
)

// ToolStopBlueprint is the flow-control tool's wire name, re-exported so the
// engine and its tests name it without reaching past the registry.
const ToolStopBlueprint = tooldefs.StopBlueprintName

// SandboxTools returns the jail-implemented tool definitions in the
// harness's own order.
func SandboxTools() []schemas.ChatTool {
	return tooldefs.ChatTools(tooldefs.Sandbox())
}

// tfToolFacts are what tooldefs decides a native engagement's TF tools from.
func tfToolFacts(hasBlueprint bool) tooldefs.Facts {
	return tooldefs.Facts{Runtime: domain.ConversationRuntimeNative, HasBlueprint: hasBlueprint}
}

// tfTools returns the definitions of the TF tools this engagement is offered.
func tfTools(params Params) []schemas.ChatTool {
	return tooldefs.ChatTools(tooldefs.TFTools(tfToolFacts(params.HasBlueprint)))
}

// resolveLoopSide answers a call to a TF tool without leaving the
// orchestrator, reporting false for anything the jail owns.
//
// The lookup goes through the offered set, so a name means nothing where the
// tool was never offered: a hallucinated call falls through to the sandbox
// and comes back "unknown tool", which is the right answer.
func resolveLoopSide(ctx context.Context, call domain.ToolCall, params Params) (tooldefs.Outcome, bool) {
	tool, ok := tooldefs.TFTool(call.Name, tfToolFacts(params.HasBlueprint))
	if !ok {
		return tooldefs.Outcome{}, false
	}
	return tool.Handler(ctx, tooldefs.Call{
		ID:   call.ID,
		Args: call.Input,
		Run: tooldefs.Run{
			OrgID:          params.OrgID,
			ConversationID: params.ConversationID,
			ClaimID:        params.ClaimID,
		},
	}), true
}
