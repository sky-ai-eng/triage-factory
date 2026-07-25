package agentloop

import (
	_ "embed" // powers toolDefinitionsJSON
	"encoding/json"
	"fmt"
	"sync"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// toolDefinitionsJSON is the model-facing schema for the seven in-jail
// tools, generated verbatim from the compiled harness
// (`tf-harness-tools --definitions`). It is vendored rather than derived at
// build time so a Go build never depends on a Rust toolchain, and pinned by
// a drift test that regenerates it when cargo is available.
//
// These strings are part of the parity contract: the descriptions
// co-evolved with the coding-agent system prompt, so editing one here
// without the other changes model behavior for the worse.
//
//go:embed tools/definitions.json
var toolDefinitionsJSON []byte

// ToolStopBlueprint is the one flow-control tool. It executes loop-side and
// never enters the sandbox — it is how a run reaches decideBlueprintStep now
// that the SDK's JSON completion envelope is gone from this path.
//
// Every deliberate ending goes through this single name, with `type` naming
// which one. The alternative — a tool per ending — put the destructive
// ending on the zero-effort path: handing off to the next step needed an
// explicit call while ending the entire workflow needed only silence, so the
// prompt had to spend a paragraph arguing against the easier option.
// Stopping now means `continue`, and the tool is only ever reached to do
// something other than the safe default.
//
// The name says what it acts on, because that is what makes its absence
// obvious: a conversation with no blueprint has nothing for it to stop.
const ToolStopBlueprint = "stop_blueprint"

// stop_blueprint's `type` argument. The values are the stored RunOutcome
// vocabulary verbatim rather than a second, friendlier set of words: a
// translation table between what the model says and what the row holds is a
// thing that drifts.
//
// RunOutcomeContinue is deliberately absent. It is what an ordinary stop
// means, so offering it here would reintroduce the ambiguity this tool
// exists to remove.
const (
	stopTypeFinish = string(domain.RunOutcomeFinish)
	stopTypeAbort  = string(domain.RunOutcomeAbort)
)

var (
	toolDefsOnce sync.Once
	toolDefs     []schemas.ChatTool
	toolDefsErr  error
)

// SandboxTools returns the seven in-jail tool definitions in the harness's
// own order. Parsed once and shared: the slice is treated as immutable by
// every caller (assembly copies before appending flow-control tools).
func SandboxTools() ([]schemas.ChatTool, error) {
	toolDefsOnce.Do(func() {
		toolDefs, toolDefsErr = parseToolDefinitions(toolDefinitionsJSON)
	})
	return toolDefs, toolDefsErr
}

// parseToolDefinitions maps the harness's `{name, description, parameters}`
// records onto bifrost's function-tool shape.
func parseToolDefinitions(raw []byte) ([]schemas.ChatTool, error) {
	var defs []struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &defs); err != nil {
		return nil, fmt.Errorf("agentloop: parse vendored tool definitions: %w", err)
	}
	out := make([]schemas.ChatTool, 0, len(defs))
	for _, d := range defs {
		// ToolFunctionParameters decodes a JSON Schema object directly, and
		// its OrderedMap properties preserve the harness's key order — so the
		// wire schema is the vendored bytes, not a re-serialization of them.
		// Property order is not merely cosmetic: it is part of the prefix the
		// provider caches, so a map-ordered round trip would churn the cache
		// between processes.
		var params schemas.ToolFunctionParameters
		if len(d.Parameters) > 0 {
			if err := json.Unmarshal(d.Parameters, &params); err != nil {
				return nil, fmt.Errorf("agentloop: tool %q parameters: %w", d.Name, err)
			}
		}
		if params.Type == "" {
			params.Type = "object"
		}
		out = append(out, functionTool(d.Name, d.Description, &params))
	}
	return out, nil
}

// flowControlTools returns the loop-side tool set for this conversation.
//
// A conversation with no blueprint gets none. There is nothing for the tool
// to act on, and its two endings both presuppose an absent human: aborting
// leaves a task open for someone to inherit, and there is no task. In a
// conversation someone is actually having, the way to say "I can't do this"
// is to say it — to the person reading, who is right there.
//
// Within a blueprint the set does not vary by step position, `finish`
// included on a step that is already the last one, where it is an exact
// alias for stopping normally and decideBlueprintStep gives the identical
// answer. A harmless alias costs less than a tool list that changes shape
// mid-workflow and can fall out of step with the prompt describing it.
func flowControlTools(hasBlueprint bool) []schemas.ChatTool {
	if !hasBlueprint {
		return nil
	}
	props := schemas.NewOrderedMap()
	props.Set("type", map[string]any{
		"type": "string",
		"enum": []string{stopTypeFinish, stopTypeAbort},
		"description": "\"finish\" ends the whole task successfully, skipping any workflow steps after this one. " +
			"\"abort\" stops without completing it and leaves the task open for a human.",
	})
	props.Set("reason", map[string]any{
		"type":        "string",
		"description": "Why you are stopping here, and what a human needs to know or do next.",
	})
	return []schemas.ChatTool{functionTool(ToolStopBlueprint,
		"End this run in a way other than simply finishing. You do NOT need this to complete your work normally — "+
			"for that, reply with a message and no tool calls. Reach for this only to abort, or to end an entire "+
			"multi-step workflow early.",
		&schemas.ToolFunctionParameters{
			Type:       "object",
			Properties: props,
			Required:   []string{"type", "reason"},
		})}
}

func functionTool(name, description string, params *schemas.ToolFunctionParameters) schemas.ChatTool {
	return schemas.ChatTool{
		Type: schemas.ChatToolTypeFunction,
		Function: &schemas.ChatToolFunction{
			Name:        name,
			Description: &description,
			Parameters:  params,
		},
	}
}

// flowControlOutcome reports whether call is the flow-control tool and, if
// so, the outcome and reason it carries — the single place the model-facing
// vocabulary meets the one decideBlueprintStep reads.
//
// An unrecognized `type` yields an empty outcome rather than an error:
// isFlowControl still holds, so the caller answers the call in band and the
// model gets to correct itself.
//
// hasBlueprint mirrors registration, so the name means nothing where the
// tool was never offered. A hallucinated call then falls through to the
// sandbox and comes back "unknown tool", which is the right answer — far
// better than silently ending a conversation someone is in the middle of.
func flowControlOutcome(call domain.ToolCall, hasBlueprint bool) (outcome domain.RunOutcome, reason string, isFlowControl bool) {
	if !hasBlueprint || call.Name != ToolStopBlueprint {
		return "", "", false
	}
	reason = stringArg(call.Input, "reason")
	switch stringArg(call.Input, "type") {
	case stopTypeFinish:
		return domain.RunOutcomeFinish, reason, true
	case stopTypeAbort:
		return domain.RunOutcomeAbort, reason, true
	default:
		return "", reason, true
	}
}

func stringArg(input map[string]any, key string) string {
	if v, ok := input[key].(string); ok {
		return v
	}
	return ""
}
