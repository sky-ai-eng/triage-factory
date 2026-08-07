package jira

import (
	"strings"
)

// Atlassian Document Format is the rich-text encoding Jira Cloud's REST v3
// requires for every rich-text field on a write — a comment body, an issue
// description. v3 rejects the plain string v2 accepts, and v2 rejects ADF, so
// the shape is a function of the Config's APIVersion and nothing else (see
// Config.richTextValue).
//
// The input side is Jira wiki markup, because that is the dialect the product
// asks agents to write and the dialect a v2 write already lands in: on v2 Jira
// itself parses the markup server-side, so a v3 write that shipped the same
// text as one flat paragraph would render every "h2." and "{code}" literally —
// the same comment reading worse on Cloud than on Data Center. Converting here
// keeps the two deployments' rendered output equivalent, which is what lets the
// executor's proxy client move off its uniform v2 pin.
//
// The conversion is deliberately partial. It covers the constructs the shipped
// Jira-formatting guidance teaches plus the unambiguous block forms; anything
// it does not recognize survives as literal text in a paragraph. Losing styling
// is recoverable, silently dropping a sentence is not, so every branch is
// written to keep the characters and give up only the markup.

// adfDoc is the root node of an ADF document. Version is the format revision
// and is required on the root; Jira rejects a body without it.
type adfDoc struct {
	Type    string    `json:"type"`
	Version int       `json:"version"`
	Content []adfNode `json:"content"`
}

// adfNode is one node of the document tree. The zero value of each optional
// field is omitted so the emitted JSON carries only what a node actually has —
// a text node has no content array, a paragraph has no text.
type adfNode struct {
	Type    string         `json:"type"`
	Text    string         `json:"text,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Marks   []adfMark      `json:"marks,omitempty"`
	Content []adfNode      `json:"content,omitempty"`
}

// adfMark is an inline annotation on a text node (strong, em, code, link).
type adfMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// wikiToADF renders Jira wiki markup as an ADF document. It never fails: text
// it cannot classify becomes a paragraph, so the caller always has a body Jira
// will accept.
func wikiToADF(s string) adfDoc {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	content := adfBlocks(lines)
	if len(content) == 0 {
		// A doc with no content is not valid ADF, and an all-whitespace body is
		// still a body the caller chose to send.
		content = []adfNode{{Type: "paragraph"}}
	}
	return adfDoc{Type: "doc", Version: 1, Content: content}
}

// adfBlocks converts a run of lines into block-level nodes. It is recursive
// only through quote blocks, whose interior is itself a sequence of blocks.
func adfBlocks(lines []string) []adfNode {
	var out []adfNode
	for i := 0; i < len(lines); {
		line := lines[i]

		if strings.TrimSpace(line) == "" {
			i++
			continue
		}

		if lang, ok, closing := codeFenceOpen(line); ok {
			body, next := collectUntil(lines, i+1, closing)
			out = append(out, codeBlockNode(lang, body))
			i = next
			continue
		}

		if strings.TrimSpace(line) == "{quote}" {
			body, next := collectUntil(lines, i+1, "{quote}")
			inner := quotableBlocks(adfBlocks(body))
			if len(inner) == 0 {
				inner = []adfNode{{Type: "paragraph"}}
			}
			out = append(out, adfNode{Type: "blockquote", Content: inner})
			i = next
			continue
		}

		if rest, ok := strings.CutPrefix(line, "bq. "); ok {
			out = append(out, adfNode{Type: "blockquote", Content: []adfNode{paragraphNode([]string{rest})}})
			i++
			continue
		}

		if level, rest, ok := headingPrefix(line); ok {
			out = append(out, adfNode{
				Type:    "heading",
				Attrs:   map[string]any{"level": level},
				Content: adfInline(rest),
			})
			i++
			continue
		}

		if isHorizontalRule(line) {
			out = append(out, adfNode{Type: "rule"})
			i++
			continue
		}

		if items, next := collectListItems(lines, i); len(items) > 0 {
			out = append(out, buildLists(items)...)
			i = next
			continue
		}

		// Everything else is prose: consecutive lines belong to one paragraph,
		// with their line breaks preserved, until a blank line or a line that
		// starts a block of its own.
		start := i
		for i < len(lines) && !startsBlock(lines[i]) {
			i++
		}
		out = append(out, paragraphNode(lines[start:i]))
	}
	return out
}

// quotableBlocks narrows blocks to what ADF permits inside a blockquote,
// which is a strictly smaller set than the document root allows: a heading
// becomes a paragraph, a rule is dropped, and a nested quote is spliced in
// flat. Jira rejects the whole write on a schema violation, so a quoted
// heading has to degrade rather than travel.
func quotableBlocks(blocks []adfNode) []adfNode {
	var out []adfNode
	for _, b := range blocks {
		switch b.Type {
		case "heading":
			out = append(out, adfNode{Type: "paragraph", Content: b.Content})
		case "blockquote":
			out = append(out, b.Content...)
		case "rule":
		default:
			out = append(out, b)
		}
	}
	return out
}

// startsBlock reports whether a line ends the paragraph being accumulated —
// either because it is blank or because it opens a block of its own.
func startsBlock(line string) bool {
	if strings.TrimSpace(line) == "" {
		return true
	}
	if _, ok, _ := codeFenceOpen(line); ok {
		return true
	}
	if strings.TrimSpace(line) == "{quote}" || strings.HasPrefix(line, "bq. ") {
		return true
	}
	if _, _, ok := headingPrefix(line); ok {
		return true
	}
	if isHorizontalRule(line) {
		return true
	}
	_, _, _, ok := listItemPrefix(line)
	return ok
}

// collectUntil returns the lines from start up to (not including) the first
// line whose trimmed form is closer, and the index just past that closer. An
// unterminated block runs to the end rather than erroring — the text is still
// the user's, and rejecting it would fail a write over a missing "{code}".
func collectUntil(lines []string, start int, closer string) ([]string, int) {
	for i := start; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == closer {
			return lines[start:i], i + 1
		}
	}
	return lines[start:], len(lines)
}

// codeFenceOpen recognizes both fence dialects a body can arrive in: Jira's
// "{code}" / "{code:lang}" / "{noformat}" and Markdown's "```lang". Both are
// accepted because neither can be mistaken for the other, and an agent writing
// a code block is the case where literal markup in the rendered comment hurts
// most. It returns the language (empty when unspecified) and the closer that
// ends the block.
func codeFenceOpen(line string) (lang string, ok bool, closer string) {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "{noformat}":
		return "", true, "{noformat}"
	case trimmed == "{code}":
		return "", true, "{code}"
	case strings.HasPrefix(trimmed, "{code:") && strings.HasSuffix(trimmed, "}"):
		return codeParamsLanguage(trimmed[len("{code:") : len(trimmed)-1]), true, "{code}"
	case strings.HasPrefix(trimmed, "```"):
		return strings.TrimSpace(trimmed[3:]), true, "```"
	}
	return "", false, ""
}

// codeParamsLanguage pulls the language out of a "{code:...}" parameter list.
// Jira accepts both the bare form ("{code:go}") and named parameters
// ("{code:title=Example|language=go}"), so both are read and anything else is
// ignored rather than guessed at.
func codeParamsLanguage(params string) string {
	for _, p := range strings.Split(params, "|") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		name, value, named := strings.Cut(p, "=")
		if !named {
			return p
		}
		if strings.EqualFold(strings.TrimSpace(name), "language") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// codeBlockNode builds a codeBlock. Its interior is never inline-parsed —
// markup characters inside code are code, not formatting.
func codeBlockNode(lang string, body []string) adfNode {
	node := adfNode{Type: "codeBlock"}
	if lang != "" {
		node.Attrs = map[string]any{"language": lang}
	}
	if text := strings.Join(body, "\n"); text != "" {
		node.Content = []adfNode{{Type: "text", Text: text}}
	}
	return node
}

// headingPrefix matches Jira's "hN. " heading form.
func headingPrefix(line string) (level int, rest string, ok bool) {
	if len(line) < 4 || line[0] != 'h' || line[1] < '1' || line[1] > '6' || line[2] != '.' {
		return 0, "", false
	}
	if line[3] != ' ' && line[3] != '\t' {
		return 0, "", false
	}
	return int(line[1] - '0'), strings.TrimSpace(line[4:]), true
}

// isHorizontalRule matches Jira's "----" rule, which must be a line of dashes
// on its own. Three dashes are Markdown's rule but Jira's strikethrough
// delimiter, so only the four-plus form is treated as a rule.
func isHorizontalRule(line string) bool {
	t := strings.TrimSpace(line)
	return len(t) >= 4 && strings.Trim(t, "-") == ""
}

// wikiListItem is one line of a list, flattened: depth is the marker run's
// length (Jira nests by repeating the marker), ordered distinguishes "#" from
// "*"/"-".
type wikiListItem struct {
	depth   int
	ordered bool
	text    string
}

// listItemPrefix matches a list line. The marker run MUST be followed by
// whitespace, which is what keeps a Markdown-style "**emphasis**" opening a
// line from being read as a second-level bullet — an agent writing Markdown
// into a Jira body is common enough that the ambiguity is worth closing.
// A mixed run ("*#") takes its kind from the last marker, as Jira does.
func listItemPrefix(line string) (depth int, ordered bool, text string, ok bool) {
	i := 0
	for i < len(line) && (line[i] == '*' || line[i] == '#' || line[i] == '-') {
		i++
	}
	if i == 0 || i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
		return 0, false, "", false
	}
	return i, line[i-1] == '#', strings.TrimSpace(line[i:]), true
}

// collectListItems consumes the maximal run of consecutive list lines starting
// at start.
func collectListItems(lines []string, start int) ([]wikiListItem, int) {
	var items []wikiListItem
	i := start
	for ; i < len(lines); i++ {
		depth, ordered, text, ok := listItemPrefix(lines[i])
		if !ok {
			break
		}
		items = append(items, wikiListItem{depth: depth, ordered: ordered, text: text})
	}
	return items, i
}

// buildLists turns a flat run of list lines into the nested bulletList /
// orderedList nodes ADF wants. A run that switches kind at the same depth
// becomes two sibling lists, which is how Jira renders it too.
func buildLists(items []wikiListItem) []adfNode {
	var out []adfNode
	pos := 0
	for pos < len(items) {
		out = append(out, buildList(items, &pos, items[pos].depth))
	}
	return out
}

func buildList(items []wikiListItem, pos *int, depth int) adfNode {
	ordered := items[*pos].ordered
	list := adfNode{Type: listTypeName(ordered)}
	for *pos < len(items) {
		it := items[*pos]
		if it.depth < depth {
			break
		}
		if it.depth > depth && len(list.Content) > 0 {
			// A deeper marker nests under the item that precedes it.
			child := buildList(items, pos, it.depth)
			last := &list.Content[len(list.Content)-1]
			last.Content = append(last.Content, child)
			continue
		}
		// A run that opens deeper than its parent has no item to nest under, so
		// it is flattened to this level rather than dropped.
		if it.depth == depth && it.ordered != ordered {
			break
		}
		*pos++
		list.Content = append(list.Content, adfNode{
			Type:    "listItem",
			Content: []adfNode{paragraphNode([]string{it.text})},
		})
	}
	return list
}

func listTypeName(ordered bool) string {
	if ordered {
		return "orderedList"
	}
	return "bulletList"
}

// paragraphNode renders one or more source lines as a single paragraph, with
// the line breaks kept as hardBreak nodes so a multi-line body does not
// collapse into one run-on line.
func paragraphNode(lines []string) adfNode {
	var content []adfNode
	for i, line := range lines {
		if i > 0 {
			content = append(content, adfNode{Type: "hardBreak"})
		}
		content = append(content, adfInline(line)...)
	}
	return adfNode{Type: "paragraph", Content: content}
}

// adfInline parses the inline markup of one line into text nodes carrying
// marks. Recognized: "{{monospace}}", "*strong*", "_emphasis_", and Jira's
// "[label|url]" / "[url]" links.
//
// Marks nest by recursion, so "*_both_*" carries both. Anything that does not
// close, or closes across a word boundary that would make it markup only by
// accident, stays literal — the delimiter characters here are also ordinary
// punctuation in prose and in identifiers (snake_case, a*b), and a false
// positive would delete characters from someone's comment.
func adfInline(s string) []adfNode {
	if s == "" {
		return nil
	}
	var out []adfNode
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, adfNode{Type: "text", Text: lit.String()})
			lit.Reset()
		}
	}

	for i := 0; i < len(s); {
		// Monospace is checked first: its interior is code, so no other mark
		// applies inside it.
		if strings.HasPrefix(s[i:], "{{") {
			if end := strings.Index(s[i+2:], "}}"); end >= 0 {
				inner := s[i+2 : i+2+end]
				if inner != "" {
					flush()
					out = append(out, adfNode{Type: "text", Text: inner, Marks: []adfMark{{Type: "code"}}})
					i += 2 + end + 2
					continue
				}
			}
		}

		if s[i] == '[' {
			if label, href, width, ok := linkAt(s[i:]); ok {
				flush()
				out = append(out, applyMark(adfInline(label), adfMark{Type: "link", Attrs: map[string]any{"href": href}})...)
				i += width
				continue
			}
		}

		if s[i] == '*' || s[i] == '_' {
			if inner, width, ok := markSpanAt(s, i); ok {
				flush()
				out = append(out, applyMark(adfInline(inner), adfMark{Type: markNameFor(s[i])})...)
				i += width
				continue
			}
		}

		lit.WriteByte(s[i])
		i++
	}
	flush()
	return out
}

func markNameFor(delim byte) string {
	if delim == '*' {
		return "strong"
	}
	return "em"
}

// applyMark adds mark to every text node produced for a marked span, so nested
// marks accumulate instead of replacing one another.
func applyMark(nodes []adfNode, mark adfMark) []adfNode {
	for i := range nodes {
		if nodes[i].Type != "text" {
			continue
		}
		nodes[i].Marks = append(nodes[i].Marks, mark)
	}
	return nodes
}

// markSpanAt matches a "*strong*" / "_emphasis_" span opening at index i,
// returning its interior and the total width consumed. The delimiters must sit
// on word boundaries — an opener preceded by a letter or digit is part of a
// word (snake_case), and a closer followed by one is too.
func markSpanAt(s string, i int) (inner string, width int, ok bool) {
	delim := s[i]
	if i > 0 && isWordByte(s[i-1]) {
		return "", 0, false
	}
	if i+1 >= len(s) || s[i+1] == ' ' || s[i+1] == '\t' || s[i+1] == delim {
		return "", 0, false
	}
	for j := i + 1; j < len(s); j++ {
		if s[j] != delim {
			continue
		}
		if s[j-1] == ' ' || s[j-1] == '\t' {
			continue
		}
		if j+1 < len(s) && isWordByte(s[j+1]) {
			continue
		}
		return s[i+1 : j], j - i + 1, true
	}
	return "", 0, false
}

// isWordByte reports whether b can sit inside a word, which is what decides
// whether a delimiter is markup or punctuation. Every continuation byte of a
// multi-byte rune counts, so an accented word is treated like an ASCII one
// without decoding the string.
func isWordByte(b byte) bool {
	return b >= 0x80 || b == '_' ||
		('a' <= b && b <= 'z') || ('A' <= b && b <= 'Z') || ('0' <= b && b <= '9')
}

// linkAt matches Jira's "[label|url]" / "[url]" link opening at the start of s.
// The target must be an absolute http(s) or mailto URL: bare "[SKY-123]" and
// "[TODO]" are ordinary bracketed prose, and Jira's other bracket forms (user
// mentions, anchors, attachments) address things this client cannot resolve.
func linkAt(s string) (label, href string, width int, ok bool) {
	end := strings.IndexByte(s, ']')
	if end < 0 {
		return "", "", 0, false
	}
	body := s[1:end]
	if strings.Contains(body, "[") {
		return "", "", 0, false
	}
	label, target, aliased := strings.Cut(body, "|")
	if !aliased {
		target = body
	}
	target = strings.TrimSpace(target)
	if !isLinkTarget(target) {
		return "", "", 0, false
	}
	if !aliased || strings.TrimSpace(label) == "" {
		label = target
	}
	return label, target, end + 1, true
}

func isLinkTarget(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "mailto:")
}
