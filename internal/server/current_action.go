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
// The agent's paths are absolute paths into its worktree, so the composed line
// collapses them to worktree-relative before anything else — "Editing
// internal/server/agent.go", not the temp-root mouthful. That has to happen
// HERE and not in a client, because the cap below is applied on this side of
// the wire: a ~70-rune worktree prefix would spend half the cap and push the
// half that names the file into the ellipsis before any client could strip it.
//
// Native tool names come from tooldefs so a rename there carries here rather
// than silently falling into the unknown arm. The PascalCase spellings beside
// them are the Claude Code SDK's, which this package cannot import and which
// are fixed by that SDK anyway.
func currentAction(calls []domain.ToolCall, worktree string) string {
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
	return oneLine(stripWorktree(line, worktree))
}

// stripWorktree collapses the run's absolute worktree paths down to paths
// relative to the worktree root. It runs over the whole composed line rather
// than only the structured path arguments, because paths ride inside bash
// commands too. The semantics deliberately match the client's transcript
// helper (frontend/src/lib/worktree.ts) so the same tool call reads
// identically on the run station and on a run row — change one and the other
// must follow.
//
// macOS resolves the temp root through the /private symlink, so the agent's
// paths may carry a /private prefix the stored worktree path omits (or vice
// versa); both variants are consumed, the /private form first so the bare
// path it contains is not matched inside it. A bare match — the root itself,
// no trailing slash — renders as ".", the shell idiom for "right here", so
// `cd <root>` reads as `cd .` rather than losing its argument.
func stripWorktree(text, worktree string) string {
	if text == "" || worktree == "" {
		return text
	}
	wt := strings.TrimRight(worktree, "/")
	if wt == "" {
		return text
	}
	bare := wt
	if strings.HasPrefix(wt, "/private/") {
		bare = wt[len("/private"):]
	}
	for _, v := range []string{"/private" + bare, bare} {
		text = strings.ReplaceAll(text, v+"/", "")
		text = strings.ReplaceAll(text, v, ".")
	}
	return text
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
