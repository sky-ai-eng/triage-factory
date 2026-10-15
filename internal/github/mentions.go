package github

import "strings"

// Mentions returns the users and teams a GitHub markdown body @-mentions, in
// the order they are first mentioned and without repeats: a user as its login
// and a team as "org/slug", the shape PRSnapshot.ReviewRequests holds
// reviewers in. Both are lower-cased, since GitHub matches logins and team
// slugs without regard to case. A body that mentions nobody returns an empty,
// non-nil slice.
//
// A mention follows GitHub's pattern: an @ that does not follow a letter,
// digit or underscore, then a login, or a login, a slash and a team slug. A
// login followed by a slash and no slug, or by a dot or underscore and a
// letter, is not a mention, which keeps file paths and dotted names out.
// Underscores around a mention that render as emphasis do not stop it.
//
// Nothing is a mention in a fenced or indented code block, in inline code, on
// a quoted (">") line, which repeats what someone else wrote, in an HTML
// comment, which GitHub does not render, or in an http(s) URL.
//
// It reads the body line by line rather than as a full CommonMark parse, so
// some layouts fall to the wrong side. A paragraph indented four spaces under
// a list item after a blank line reads as an indented code block, and inline
// code that spans a line break reads as text.
func Mentions(markdown string) []string {
	s := mentionScanner{seen: map[string]bool{}, found: []string{}}
	for _, line := range strings.Split(markdown, "\n") {
		s.line(strings.TrimSuffix(line, "\r"))
	}
	return s.found
}

type mentionScanner struct {
	// fence is the marker run of the open fenced code block, "" outside one.
	fence string
	// inComment is set while an HTML comment is open.
	inComment bool
	// inParagraph is set after a line of prose. An indented line then
	// continues the paragraph rather than opening a code block.
	inParagraph bool
	seen        map[string]bool
	found       []string
}

func (s *mentionScanner) line(line string) {
	if s.inComment {
		s.inline(line)
		return
	}
	width, rest := leadingIndent(line)
	if s.fence != "" {
		if closesFence(rest, s.fence) {
			s.fence = ""
		}
		return
	}
	switch {
	case strings.TrimSpace(rest) == "":
		s.inParagraph = false
	case width >= 4 && !s.inParagraph:
		// An indented code block.
	case openingFence(rest) != "":
		s.fence, s.inParagraph = openingFence(rest), false
	case width < 4 && strings.HasPrefix(rest, ">"):
		s.inParagraph = false
	default:
		s.inline(rest)
		s.inParagraph = !isATXHeading(rest)
	}
}

// inline scans one line of prose for mentions, after removing its inline code
// spans and HTML comments. An HTML comment left open carries to the next
// line.
func (s *mentionScanner) inline(text string) {
	var prose strings.Builder
	for i := 0; i < len(text); {
		switch {
		case s.inComment:
			end := strings.Index(text[i:], "-->")
			if end < 0 {
				i = len(text)
				continue
			}
			s.inComment = false
			i += end + len("-->")
			prose.WriteByte(' ')
		case strings.HasPrefix(text[i:], "<!--"):
			s.inComment = true
			i += len("<!--")
		case text[i] == '`':
			n := runLength(text[i:], '`')
			if end, ok := closingBackticks(text, i+n, n); ok {
				i = end
				prose.WriteByte(' ')
				continue
			}
			// No closing run of the same length: the backticks are text.
			prose.WriteString(text[i : i+n])
			i += n
		default:
			prose.WriteByte(text[i])
			i++
		}
	}
	s.mentions(prose.String())
}

// mentions adds the handles text mentions.
func (s *mentionScanner) mentions(text string) {
	for i := 0; i < len(text); i++ {
		if hasURLPrefix(text[i:]) {
			for i < len(text) && !isSpace(text[i]) {
				i++
			}
			continue
		}
		if text[i] != '@' || !mentionMayStart(text, i) {
			continue
		}
		start := i + 1
		if start >= len(text) || !isAlnum(text[start]) {
			continue
		}
		end := start
		for end < len(text) && (isAlnum(text[end]) || text[end] == '-') {
			end++
		}
		login := text[start:end]
		i = end - 1
		if end < len(text) && text[end] == '/' {
			slugStart := end + 1
			if slugStart >= len(text) || !isAlnum(text[slugStart]) {
				continue
			}
			slugEnd := slugStart
			for slugEnd < len(text) && (isWordByte(text[slugEnd]) || text[slugEnd] == '-') {
				slugEnd++
			}
			// A slug ends on a letter, digit or underscore.
			s.add(login + "/" + strings.TrimRight(text[slugStart:slugEnd], "-"))
			i = slugEnd - 1
			continue
		}
		if endsUserMention(text[end:]) {
			// A login cannot end in a hyphen, so trailing ones are
			// punctuation: "@hank- thoughts?" mentions hank.
			s.add(strings.TrimRight(login, "-"))
		}
	}
}

func (s *mentionScanner) add(handle string) {
	handle = strings.ToLower(handle)
	if !s.seen[handle] {
		s.seen[handle] = true
		s.found = append(s.found, handle)
	}
}

// mentionMayStart reports whether the @ at text[at] can open a mention: it
// does not follow a letter, digit or underscore. Underscores that open
// emphasis, with no word character before them, render as markup rather than
// text, so an @ after them can.
func mentionMayStart(text string, at int) bool {
	before := at - 1
	for before >= 0 && text[before] == '_' {
		before--
	}
	if before == at-1 {
		return before < 0 || !isWordByte(text[before])
	}
	return before < 0 || !isAlnum(text[before])
}

// endsUserMention reports whether what follows a login lets it be a mention:
// nothing, a character other than a letter, digit, underscore or dot, or dots
// followed by nothing or by such a character. Underscores that close emphasis,
// with no letter or digit after them, are markup and end it too.
func endsUserMention(after string) bool {
	for _, ch := range []byte{'.', '_'} {
		n := runLength(after, ch)
		if n == 0 {
			continue
		}
		if n == len(after) {
			return true
		}
		if ch == '.' {
			return !isWordByte(after[n])
		}
		return !isAlnum(after[n])
	}
	return true
}

// closingBackticks finds the run of exactly n backticks that closes a code
// span opened before from, and returns the index just past it.
func closingBackticks(text string, from, n int) (int, bool) {
	for i := from; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		run := runLength(text[i:], '`')
		if run == n {
			return i + run, true
		}
		i += run
	}
	return 0, false
}

// openingFence returns the marker run that opens a fenced code block on the
// line, or "" when the line opens none. A backtick fence's info string cannot
// hold a backtick; such a line is inline code.
func openingFence(rest string) string {
	for _, ch := range []byte{'`', '~'} {
		n := runLength(rest, ch)
		if n < 3 {
			continue
		}
		if ch == '`' && strings.Contains(rest[n:], "`") {
			return ""
		}
		return rest[:n]
	}
	return ""
}

// closesFence reports whether the line closes the fenced code block opened
// with fence: a run of the same character at least as long, and nothing after
// it but whitespace.
func closesFence(rest, fence string) bool {
	n := runLength(rest, fence[0])
	return n >= len(fence) && strings.TrimSpace(rest[n:]) == ""
}

// isATXHeading reports whether the line is a "#" heading. A paragraph does not
// continue past one.
func isATXHeading(rest string) bool {
	n := runLength(rest, '#')
	return n >= 1 && n <= 6 && (n == len(rest) || rest[n] == ' ' || rest[n] == '\t')
}

// leadingIndent returns the width of the line's leading whitespace, a tab
// advancing to the next multiple of four, and the line after it.
func leadingIndent(line string) (width int, rest string) {
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case ' ':
			width++
		case '\t':
			width += 4 - width%4
		default:
			return width, line[i:]
		}
	}
	return width, ""
}

func hasURLPrefix(text string) bool {
	for _, scheme := range []string{"http://", "https://"} {
		if len(text) >= len(scheme) && strings.EqualFold(text[:len(scheme)], scheme) {
			return true
		}
	}
	return false
}

func runLength(text string, ch byte) int {
	n := 0
	for n < len(text) && text[n] == ch {
		n++
	}
	return n
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isWordByte(c byte) bool { return isAlnum(c) || c == '_' }

func isSpace(c byte) bool { return c == ' ' || c == '\t' }
