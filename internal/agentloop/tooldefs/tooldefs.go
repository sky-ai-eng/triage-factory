// Package tooldefs is the registry of every tool a native conversation can
// call. One file per tool, each holding the tool's whole definition: what it
// is called, what it does, the shape of its arguments, and — for the ones
// the loop resolves itself — the code that runs.
//
// Two families live here because the model cannot tell them apart and
// neither should the code that assembles a request. Most tools are
// implemented by the compiled harness resident in the jail; the loop only
// forwards their calls over a socket, so their entry carries no Handler.
// A tool with a Handler never enters the sandbox: the loop answers it
// directly. Which family a tool belongs to is a property of the tool, not a
// name check at the dispatch site.
//
// The definitions here are the ones the model sees. For a jail-implemented
// tool that makes them a promise about code living in another language, so
// the harness ships its own copy and a drift test compares the two — see
// tooldefs_drift_test.go. Change a description here and the implementation
// there does not change with it.
package tooldefs

import (
	"github.com/maximhq/bifrost/core/schemas"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// Tool is one entry in the registry.
type Tool struct {
	// Name is the wire name the model calls and, for a jail tool, the name
	// the harness dispatches on.
	Name string
	// Description is the model's only account of what this tool does. It is
	// part of the prefix providers cache, so editing it invalidates every
	// warm conversation in the fleet — cheap, but not free.
	Description string
	// Params is the argument schema, always an object.
	Params Schema
	// Handler resolves the call loop-side. A nil Handler means the call is
	// dispatched into the jail.
	Handler Handler
}

// Handler answers a call without leaving the orchestrator. It receives the
// decoded arguments and returns what the model should see, plus — for a tool
// whose job is to end the run — the terminal outcome to record.
type Handler func(args map[string]any) Outcome

// Outcome is a loop-side tool's result.
type Outcome struct {
	// Content is the tool-result text the model reads.
	Content string
	// IsError marks the result as a failure. A handler that refuses a
	// malformed call sets this and leaves Terminal empty: the model is
	// told what it owes and the run continues.
	IsError bool
	// Terminal, when non-empty, ends the run with this outcome. Empty means
	// the call was answered and the conversation carries on, which is what
	// an ordinary loop-side tool does.
	Terminal domain.ConversationOutcome
	// Reason accompanies Terminal and is persisted as the run's
	// outcome_reason: why the run stopped where it did.
	Reason string
	// Summary accompanies Terminal and is persisted as the run's
	// result_summary: the account of the work itself, as distinct from the
	// reason for stopping.
	Summary string
}

// Schema is a JSON Schema node, carrying only what a tool argument needs.
// Fields are ordered because property order reaches the provider verbatim
// and is therefore part of the cached prefix.
type Schema struct {
	Type        string
	Description string
	// Enum constrains a string to a fixed set.
	Enum []string
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

// FlowControl returns the loop-side tools available to this conversation.
//
// A conversation with no blueprint gets none. There is nothing for
// stop_blueprint to act on, and both of its endings presuppose an absent
// human: aborting leaves a task open for someone to inherit, finishing early
// skips steps that were planned. In a conversation someone is actually
// having, the way to say "I can't do this" is to say it.
func FlowControl(hasBlueprint bool) []Tool {
	if !hasBlueprint {
		return nil
	}
	return []Tool{StopBlueprint}
}

// LoopSide returns the handler-backed tool with this name, if the
// conversation has it. It mirrors FlowControl exactly, so a name the model
// invented where the tool was never offered resolves to nothing and is
// dispatched into the jail like any other unknown tool — which answers it
// honestly instead of silently ending a conversation mid-sentence.
func LoopSide(name string, hasBlueprint bool) (Tool, bool) {
	for _, t := range FlowControl(hasBlueprint) {
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
