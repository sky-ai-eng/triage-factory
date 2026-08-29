package server

import (
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop/tooldefs"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// current_action — the one prose line a WORKING conversation shows in place of
// a title, because a conversation has none.
//
// Every other state a conversation can be in already says something honest
// about itself: a claim phase names the setup step it is on, a park reason
// names what stopped it, a terminal names how it ended (see
// internal/domain/conversation_status.go). A conversation that is simply
// running says only "running", and that is the state a reader most wants a
// sentence for.
//
// It is DERIVED, never stored. Conversation state derives from the messages
// table wherever it can — KV-cache warmth and spend both do — and a stored
// column would need a mid-run writer on both engines plus a second source of
// truth for something the transcript already holds. Both runtimes stream
// structured tool calls into `messages`, so the newest assistant message's
// last call is parsed structure that needs no engine change to read.
//
// The last call rather than the first: an assistant turn may batch several,
// and the one it ends on is the one still in flight.

// currentActionMaxLen caps the composed line, in runes. It is a display line
// on one row of a list, not a summary — an authored bash description is six
// words and a file path is short, so the cap only ever bites on a pathological
// argument, which is exactly what it is for.
const currentActionMaxLen = 140

// currentAction composes the line from a conversation's newest assistant
// message's tool calls, or "" when it cannot.
//
// Omit rather than guess is the whole rule here: an unknown tool, a call whose
// describing argument is missing, and no tool calls at all all answer "", and
// the caller then emits no field. A client that gets no field renders the
// state label ("Working"), which is vague but true; a fabricated action is
// neither.
//
// Native tool names come from tooldefs so a rename there carries here rather
// than silently falling into the unknown arm. The PascalCase spellings beside
// them are the Claude Code SDK's, which this package cannot import and which
// are fixed by that SDK anyway.
func currentAction(calls []domain.ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	call := calls[len(calls)-1]

	var line string
	switch call.Name {
	case tooldefs.Bash.Name, "Bash":
		// A shell command is the one tool argument that is not legible to a
		// person as-is, so bash carries an authored summary and it wins. The
		// command is the fallback, prefixed because the bare argv would read
		// as a label rather than as something happening. Present tense only:
		// this line describes a call in flight, so description_past — which
		// the same row may well carry — is deliberately not consulted.
		if summary := toolArg(call.Input, "description"); summary != "" {
			line = summary
		} else if command := firstLine(toolArg(call.Input, "command")); command != "" {
			line = "Running " + command
		}
	case tooldefs.Read.Name, "Read":
		line = phrase("Reading ", toolPath(call.Input))
	case tooldefs.Write.Name, "Write":
		line = phrase("Writing ", toolPath(call.Input))
	case tooldefs.Edit.Name, "Edit":
		line = phrase("Editing ", toolPath(call.Input))
	case tooldefs.Grep.Name, "Grep":
		line = phrase("Searching ", toolArg(call.Input, "pattern"))
	case tooldefs.Find.Name, "Glob":
		line = phrase("Finding ", toolArg(call.Input, "pattern"))
	case tooldefs.Ls.Name:
		line = phrase("Listing ", toolPath(call.Input))
	case tooldefs.StopBlueprintName:
		// The agent's own statement of why it is ending, which is a better
		// sentence than anything composed around it.
		line = toolArg(call.Input, "reason")
	case "Task":
		line = toolArg(call.Input, "description")
	}
	return oneLine(line)
}

// toolPath reads the file or directory argument of a path-taking tool under
// either runtime's spelling: the SDK calls it file_path, tooldefs calls it
// path. Neither runtime sets both, so the order is only a tie-break that never
// happens.
func toolPath(input map[string]any) string {
	if p := toolArg(input, "file_path"); p != "" {
		return p
	}
	return toolArg(input, "path")
}

// toolArg reads one string argument. A missing key, a null, and a non-string
// value are all "" — the tool schemas are advisory and a model can emit
// anything.
func toolArg(input map[string]any, key string) string {
	v, _ := input[key].(string)
	return strings.TrimSpace(v)
}

// phrase joins a verb to its argument, or answers "" when the argument is
// missing. A verb with nothing after it ("Reading") describes no action.
func phrase(verb, arg string) string {
	if arg == "" {
		return ""
	}
	return verb + arg
}

// firstLine is the head of a multi-line value, trimmed. A heredoc's first line
// is what the agent is running; the body is not a label.
func firstLine(s string) string {
	head, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(head)
}

// oneLine flattens whitespace and applies the cap, so what reaches the wire is
// always a single line that fits the row it renders in.
func oneLine(s string) string {
	line := strings.Join(strings.Fields(s), " ")
	if len([]rune(line)) <= currentActionMaxLen {
		return line
	}
	return strings.TrimRight(string([]rune(line)[:currentActionMaxLen-1]), " ") + "…"
}
