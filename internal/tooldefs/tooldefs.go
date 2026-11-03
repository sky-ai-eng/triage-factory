// Package tooldefs is the registry of every tool TF defines for an agent. One
// file per tool, each holding the tool's whole definition: what it is called,
// what it does, the shape of its arguments, and, for the ones TF answers
// itself, the code that runs.
//
// Two families live here, and which one a tool belongs to is a property of the
// tool, not a name check at a dispatch site.
//
// Sandbox tools (Sandbox) are implemented by the compiled harness resident in
// a native run's jail. The loop only forwards their calls over a socket, so
// their entries carry no Handler. They are native-only: an SDK run uses Claude
// Code's built-in tools instead. The definitions here are the ones the model
// sees, which makes them a promise about code living in another language, so
// the harness ships its own copy and TestToolDefinitions_MatchHarness in
// internal/agentloop compares the two. Change a description here and the
// implementation there does not change with it.
//
// TF tools (TFTools) are answered by TF in both runtimes, through their
// Handler. The native loop calls it directly and never sends the call into the
// sandbox; an SDK run reaches it through the wrapper's in-process MCP server,
// which relays each call to the stream reader in internal/agentproc. TFTools
// is the only place that decides which of them a run is offered. Today that is
// only stop_blueprint.
//
// A Handler reads its arguments and the identity of the conversation making
// the call, and nothing else: anything more a tool needs reaches it on Run as
// an interface its caller implements, so this package imports no store.
package tooldefs

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Tool is one entry in the registry.
type Tool struct {
	// Name is the name the model calls and, for a sandbox tool, the name the
	// harness dispatches on. An SDK run sees a TF tool under its MCP name
	// (mcp__tf__<Name>), which the stream reader strips before a row is
	// stored, so every stored row carries Name in both runtimes.
	Name string
	// Description is the model's only account of what this tool does. It is
	// part of the prefix providers cache, so editing it invalidates every
	// warm conversation in the fleet — cheap, but not free.
	Description string
	// Params is the argument schema, always an object.
	Params Schema
	// Handler answers the call on TF's side. A nil Handler means the jail
	// answers it.
	Handler Handler
}

// Handler answers a call to a TF tool and returns what the model should see,
// plus, for a tool whose job is to end the run, the terminal outcome to
// record. ctx ends with the run, and on an SDK run also when the call is
// cancelled or outlives its deadline; a handler that does slow work stops when
// it ends.
type Handler func(ctx context.Context, call Call) Outcome

// Call is one invocation of a TF tool.
type Call struct {
	// ID is the call's tool_use id: the id on the assistant message's tool
	// call and on the result that answers it.
	ID string
	// Args is the decoded argument object.
	Args map[string]any
	// Run is the conversation making the call.
	Run Run
}

// Run is the conversation a call belongs to.
type Run struct {
	OrgID          string
	ConversationID string
	// ClaimID is the executor engagement that made the call.
	ClaimID string
}

// Outcome is a TF tool's result.
type Outcome struct {
	// Content is the tool-result text the model reads.
	Content string
	// IsError marks the result as a failure. A handler that refuses a
	// malformed call sets this and leaves Terminal empty: the model is
	// told what it owes and the run continues.
	IsError bool
	// Terminal, when non-empty, ends the run with this outcome. Empty means
	// the call was answered and the conversation carries on, which is what
	// an ordinary TF tool does. Only the native loop acts on it; no tool
	// that sets it is offered to an SDK run.
	Terminal domain.ConversationOutcome
	// Reason accompanies Terminal and is persisted as the run's
	// outcome_reason: why the run stopped where it did.
	Reason string
	// Summary accompanies Terminal and is persisted as the run's
	// result_summary: the account of the work itself, as distinct from the
	// reason for stopping.
	Summary string
}

// Facts are what decides which TF tools a conversation is offered.
type Facts struct {
	// Runtime is the conversation's conversations.runtime value:
	// domain.ConversationRuntimeSDK or domain.ConversationRuntimeNative.
	Runtime string
	// HasBlueprint reports that the conversation executes a blueprint
	// against a task.
	HasBlueprint bool
}

// Schema is a JSON Schema node, carrying only what a tool argument needs.
// Fields are ordered because property order reaches the provider verbatim
// and is therefore part of the cached prefix.
type Schema struct {
	Type        string
	Description string
	// Enum constrains a string to a fixed set.
	Enum []string
	// MinLength is a string's minimum length in characters; zero is none.
	MinLength int
	// Fields and Required describe an object.
	Fields   []Field
	Required []string
	// Items describes an array's element.
	Items *Schema
}

// Field is one named property of an object schema.
type Field struct {
	Name string
	Schema
}

// obj and str are shorthand for the two shapes that dominate these files.
func obj(required []string, fields ...Field) Schema {
	return Schema{Type: "object", Fields: fields, Required: required}
}

func str(name, description string) Field {
	return Field{Name: name, Schema: Schema{Type: "string", Description: description}}
}

// atLeast sets a string field's minimum length in characters.
func (f Field) atLeast(chars int) Field {
	f.MinLength = chars
	return f
}

func num(name, description string) Field {
	return Field{Name: name, Schema: Schema{Type: "number", Description: description}}
}

func boolean(name, description string) Field {
	return Field{Name: name, Schema: Schema{Type: "boolean", Description: description}}
}

// Sandbox returns the tools the jail implements, in the harness's own order.
// The order is fixed for the same reason property order is: it is part of
// what the provider caches.
func Sandbox() []Tool {
	return []Tool{Read, Bash, Edit, Write, Grep, Find, Ls}
}

// TFTools returns the TF tools a conversation with these facts is offered,
// in a fixed order: the list reaches the provider verbatim and is part of the
// prefix it caches.
//
// stop_blueprint needs a blueprint. A conversation with none has nothing for
// it to act on, and both of its endings presuppose an absent human: aborting
// leaves a task open for someone to inherit, finishing early skips steps that
// were planned. In a conversation someone is actually having, the way to say
// "I can't do this" is to say it.
func TFTools(f Facts) []Tool {
	var out []Tool
	// TODO(TFAC-1333): offer stop_blueprint to SDK runs with a blueprint too.
	// The SDK reader answers a TF tool's call but drops Outcome.Terminal, so
	// an SDK run cannot yet end on one.
	if f.HasBlueprint && f.Runtime == domain.ConversationRuntimeNative {
		out = append(out, StopBlueprint)
	}
	return append(out, offeredForTest()...)
}

// TFTool returns the TF tool with this name, if a conversation with these
// facts is offered it. It reads TFTools, so a name the model invented where
// the tool was never offered resolves to nothing. The native loop then sends
// the call into the jail like any other unknown tool, and the SDK reader
// answers it as unknown: either way it is answered honestly instead of
// silently ending a conversation mid-sentence.
func TFTool(name string, f Facts) (Tool, bool) {
	for _, t := range TFTools(f) {
		if t.Name == name && t.Handler != nil {
			return t, true
		}
	}
	return Tool{}, false
}

// ChatTools converts registry entries to the provider's wire shape.
func ChatTools(tools []Tool) []schemas.ChatTool {
	out := make([]schemas.ChatTool, 0, len(tools))
	for _, t := range tools {
		description := t.Description
		params := t.Params.toolParameters()
		out = append(out, schemas.ChatTool{
			Type: schemas.ChatToolTypeFunction,
			Function: &schemas.ChatToolFunction{
				Name:        t.Name,
				Description: &description,
				Parameters:  params,
			},
		})
	}
	return out
}

// toolParameters renders the top-level object schema. bifrost's OrderedMap
// is what carries declaration order onto the wire; a plain Go map would
// re-sort the keys and churn the cached prefix between processes.
func (s Schema) toolParameters() *schemas.ToolFunctionParameters {
	typ := s.Type
	if typ == "" {
		typ = "object"
	}
	return &schemas.ToolFunctionParameters{
		Type:       typ,
		Properties: fieldsToProperties(s.Fields),
		Required:   s.Required,
	}
}

// JSONSchema renders the argument schema as a plain JSON Schema document: the
// form an SDK run's MCP server builds the tool's input schema from. Property
// order is declaration order at every depth, as it is on the native wire.
//
// Every node is a string, a number, a list of strings or an ordered map of
// those, none of which can fail to marshal, so an error here is a bug in this
// file and panics rather than asking every launch to handle it.
func (s Schema) JSONSchema() json.RawMessage {
	if s.Type == "" {
		s.Type = "object"
	}
	b, err := json.Marshal(s.node())
	if err != nil {
		panic(fmt.Sprintf("tooldefs: render JSON Schema: %v", err))
	}
	return b
}

func fieldsToProperties(fields []Field) *schemas.OrderedMap {
	if len(fields) == 0 {
		return nil
	}
	props := schemas.NewOrderedMap()
	for _, f := range fields {
		props.Set(f.Name, f.node())
	}
	return props
}

// node renders a nested schema. Ordering is preserved at every depth, not
// just the top level, so the whole document is byte-stable across processes.
func (s Schema) node() any {
	n := schemas.NewOrderedMap()
	if s.Description != "" {
		n.Set("description", s.Description)
	}
	n.Set("type", s.Type)
	if len(s.Enum) > 0 {
		n.Set("enum", s.Enum)
	}
	if s.MinLength > 0 {
		n.Set("minLength", s.MinLength)
	}
	if s.Items != nil {
		n.Set("items", s.Items.node())
	}
	if len(s.Fields) > 0 {
		n.Set("properties", fieldsToProperties(s.Fields))
	}
	if len(s.Required) > 0 {
		n.Set("required", s.Required)
	}
	return n
}
