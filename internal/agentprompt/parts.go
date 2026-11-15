package agentprompt

import "strings"

// Part is one named piece of composed prompt text: one framework block, or one
// source's verbs in a tools reference. A conversation keeps the parts its
// system text was composed from, so a later claim tells it what changed part by
// part rather than rewriting its system prompt. The provider binds every
// thinking block to the system prompt it was produced under, so a rewritten one
// makes the conversation's next call fail.
type Part struct {
	// Key identifies the piece across releases and is what two compositions
	// are compared by.
	Key string
	// Name is how an update refers to the piece. For a framework block it is
	// the block's opening tag, which is how the model finds the text it
	// replaces.
	Name string
	Text string
}

// BuildParts returns Build's composition one block per part, in order. The
// parts' texts joined by a blank line, with a trailing newline, are exactly
// Build(spec).
func BuildParts(spec Spec) []Part {
	paths, err := manifest(spec)
	if err != nil {
		panic(err.Error())
	}
	out := make([]Part, len(paths))
	for i, p := range paths {
		text := block(p)
		out[i] = Part{Key: "framework/" + p, Name: openingTag(text, p), Text: text}
	}
	return out
}

// openingTag is the name of the XML-style tag a block opens with, or the
// block's path when its first line is not one.
func openingTag(text, path string) string {
	first, _, _ := strings.Cut(text, "\n")
	first = strings.TrimSpace(first)
	if len(first) > 2 && first[0] == '<' && first[len(first)-1] == '>' && !strings.ContainsAny(first[1:len(first)-1], " <>/") {
		return first[1 : len(first)-1]
	}
	return path
}

// InstructionsUpdate composes the message that tells a conversation which
// parts of its instructions changed since it began: each changed or new part in
// full, and one line for each part that no longer applies. "" when there is
// nothing to say.
func InstructionsUpdate(changed, withdrawn []Part) string {
	if len(changed) == 0 && len(withdrawn) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<instructions_update>\n")
	b.WriteString(block(blockInstructionsUpdate))
	for _, p := range changed {
		b.WriteString("\n\n<part name=\"" + p.Name + "\">\n" + strings.TrimSpace(p.Text) + "\n</part>")
	}
	if len(withdrawn) > 0 {
		b.WriteString("\n")
		for _, p := range withdrawn {
			b.WriteString("\nNo longer in effect: " + p.Name + ".")
		}
	}
	b.WriteString("\n</instructions_update>")
	return b.String()
}
