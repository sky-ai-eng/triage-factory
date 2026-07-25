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

// Flow-control tool names. These execute loop-side and never enter the
// sandbox — they are how a blueprint step reaches decideBlueprintStep now
// that the SDK's JSON completion envelope is gone from this path.
const (
	// ToolAbort is the voluntary stop: the agent is functioning but
	// choosing not to continue. Always registered; the reason is required
	// because "why" is the only thing worth collecting from a deliberate
	// stop (an involuntary death is a `failed` conversation, not this).
	ToolAbort = "abort"
	// ToolContinue hands off to the next blueprint step. Registered ONLY on
	// a non-terminal step — the addendum prompt that introduces it is
	// appended on exactly those steps, so a model never sees a tool its
	// instructions don't mention.
	ToolContinue = "continue"
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

// flowControlTools returns the loop-side tools for this step. `abort` is
// always present; `continue` only on a non-terminal blueprint step.
func flowControlTools(nonTerminalStep bool) []schemas.ChatTool {
	abortProps := schemas.NewOrderedMap()
	abortProps.Set("reason", map[string]any{
		"type":        "string",
		"description": "Why you are stopping, and what a human needs to do next.",
	})
	abort := functionTool(ToolAbort,
		"Stop this run deliberately without completing the task. Use this only when you are functioning correctly but have concluded the work cannot or should not proceed — a blocker you cannot resolve, a task that turns out to be already done, or an instruction you should not follow. State plainly why, so a human can pick it up.",
		&schemas.ToolFunctionParameters{
			Type:       "object",
			Properties: abortProps,
			Required:   []string{"reason"},
		})
	if !nonTerminalStep {
		return []schemas.ChatTool{abort}
	}
	contProps := schemas.NewOrderedMap()
	contProps.Set("summary", map[string]any{
		"type":        "string",
		"description": "What you did, in one or two sentences, for the step that picks this up.",
	})
	cont := functionTool(ToolContinue,
		"Hand this workflow off to its next step. Call this when your own step is done and the remaining work belongs to the step that follows.",
		&schemas.ToolFunctionParameters{
			Type:       "object",
			Properties: contProps,
			Required:   []string{"summary"},
		})
	return []schemas.ChatTool{cont, abort}
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

// flowControlOutcome maps a flow-control call onto the terminal outcome
// vocabulary decideBlueprintStep reads. ok is false for any other tool name,
// which is what makes this the single place the mapping lives.
func flowControlOutcome(call domain.ToolCall) (outcome domain.RunOutcome, summary, reason string, ok bool) {
	switch call.Name {
	case ToolContinue:
		return domain.RunOutcomeContinue, stringArg(call.Input, "summary"), "", true
	case ToolAbort:
		return domain.RunOutcomeAbort, "", stringArg(call.Input, "reason"), true
	default:
		return "", "", "", false
	}
}

func stringArg(input map[string]any, key string) string {
	if v, ok := input[key].(string); ok {
		return v
	}
	return ""
}
