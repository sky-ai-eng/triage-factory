package main

import (
	"strings"
	"unicode/utf8"
)

// Comment detection walks each file from byte 0 rather than judging a diff's
// +/- lines in isolation, because whether a line is a comment is a fact about
// the state it sits in, not about how it reads on its own. The middle line of
// a JSX {/* ... */} block, a line inside a Go raw string that happens to start
// with "//", and a URL in a string literal are all indistinguishable from
// their own text; they are unambiguous once you have scanned what came before.
//
// A line counts as a comment only when it carries NO code, so `x := 1 // why`
// is a code line. That is how cloc and every other counter treats trailing
// commentary, and it keeps the four buckets summing to something a reviewer
// can reconcile against the diff.

type lineKind uint8

const (
	lineBlank lineKind = iota
	lineComment
	lineCode
)

// quoteMode says what a single quote means in a language: a string delimiter
// (TypeScript), a rune/char literal (Go, Rust — where it is also a lifetime
// tick), or ordinary punctuation.
type quoteMode uint8

const (
	quoteNone quoteMode = iota
	quoteString
	quoteChar
)

type lang struct {
	name        string
	lineComment []string
	blockOpen   string
	blockClose  string
	nestedBlock bool // Rust nests /* /* */ */
	doubleQuote bool
	singleQuote quoteMode
	backtick    bool // ` opens a multi-line raw (Go) or template (JS) string
	rawHash     bool // Rust r"..." / r#"..."#

	// jsxBraces treats the braces in {/* … */} as part of the comment rather
	// than as code. Without it every JSX comment block reports its first and
	// last lines as code, because the brace is the only thing on them.
	jsxBraces bool
}

func (l lang) hasComments() bool { return len(l.lineComment) > 0 || l.blockOpen != "" }

var (
	langNone   = lang{name: "none"}
	langGo     = lang{name: "go", lineComment: []string{"//"}, blockOpen: "/*", blockClose: "*/", doubleQuote: true, singleQuote: quoteChar, backtick: true}
	langRust   = lang{name: "rust", lineComment: []string{"//"}, blockOpen: "/*", blockClose: "*/", nestedBlock: true, doubleQuote: true, singleQuote: quoteChar, rawHash: true}
	langJS     = lang{name: "js", lineComment: []string{"//"}, blockOpen: "/*", blockClose: "*/", doubleQuote: true, singleQuote: quoteString, backtick: true, jsxBraces: true}
	langCSS    = lang{name: "css", blockOpen: "/*", blockClose: "*/", doubleQuote: true, singleQuote: quoteString}
	langSQL    = lang{name: "sql", lineComment: []string{"--"}, blockOpen: "/*", blockClose: "*/", doubleQuote: true, singleQuote: quoteString}
	langHash   = lang{name: "hash", lineComment: []string{"#"}, doubleQuote: true, singleQuote: quoteString}
	langMarkup = lang{name: "markup", blockOpen: "<!--", blockClose: "-->"}
	langGoMod  = lang{name: "gomod", lineComment: []string{"//"}}
)

// langByExt maps an extension to its comment grammar. Anything absent falls
// through to langNone, which reports every non-blank line as code — the right
// answer for JSON, CSV, and the prompt .txt blocks, none of which have a
// comment form.
var langByExt = map[string]lang{
	".go":   langGo,
	".rs":   langRust,
	".ts":   langJS,
	".tsx":  langJS,
	".js":   langJS,
	".jsx":  langJS,
	".mjs":  langJS,
	".cjs":  langJS,
	".css":  langCSS,
	".sql":  langSQL,
	".sh":   langHash,
	".bash": langHash,
	".zsh":  langHash,
	".yml":  langHash,
	".yaml": langHash,
	".toml": langHash,
	".py":   langHash,
	".rb":   langHash,
	".html": langMarkup,
	".xml":  langMarkup,
	".svg":  langMarkup,
	".mod":  langGoMod,
}

var langByBase = map[string]lang{
	"Dockerfile":         langHash,
	"Makefile":           langHash,
	".gitignore":         langHash,
	".dockerignore":      langHash,
	".prettierignore":    langHash,
	".env.example":       langHash,
	"CODEOWNERS":         langHash,
	".golangci.yml":      langHash,
	"prepare-commit-msg": langHash,
	"pre-push":           langHash,
}

// langFor picks a grammar from the path alone. Extensionless files that turn
// out to be scripts are corrected by langForContent once the blob is in hand.
func langFor(p string) lang {
	if l, ok := langByBase[baseName(p)]; ok {
		return l
	}
	if l, ok := langByExt[extOf(p)]; ok {
		return l
	}
	return langNone
}

// langForContent upgrades a path-derived guess using the file itself. The only
// case that matters here is the extensionless shell script — git hooks and
// entrypoints — which is otherwise indistinguishable from a data blob.
func langForContent(p string, src []byte) lang {
	l := langFor(p)
	if l.name == langNone.name && strings.HasPrefix(string(src), "#!") {
		return langHash
	}
	return l
}

type scanState struct {
	blockDepth int
	raw        string // closing delimiter of an open multi-line raw string
}

// scanFile returns one lineKind per line, indexed from 0 for line 1.
func scanFile(l lang, src []byte) []lineKind {
	lines := strings.Split(string(src), "\n")
	// A trailing newline leaves a phantom empty element; dropping it keeps the
	// slice aligned with git's 1-based line numbers.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	out := make([]lineKind, len(lines))
	var st scanState
	for i, ln := range lines {
		out[i] = scanLine(l, strings.TrimSuffix(ln, "\r"), &st)
	}
	return out
}

func scanLine(l lang, s string, st *scanState) lineKind {
	var hasCode, hasComment bool
	for i := 0; i < len(s); {
		switch {
		case st.blockDepth > 0:
			if l.nestedBlock && strings.HasPrefix(s[i:], l.blockOpen) {
				st.blockDepth++
				hasComment = true
				i += len(l.blockOpen)
				continue
			}
			if strings.HasPrefix(s[i:], l.blockClose) {
				st.blockDepth--
				hasComment = true
				i += len(l.blockClose)
				if l.jsxBraces && st.blockDepth == 0 && i < len(s) && s[i] == '}' {
					i++
				}
				continue
			}
			if !isSpace(s[i]) {
				hasComment = true
			}
			i++

		case st.raw != "":
			if strings.HasPrefix(s[i:], st.raw) {
				hasCode = true
				i += len(st.raw)
				st.raw = ""
				continue
			}
			if !isSpace(s[i]) {
				hasCode = true
			}
			i++

		default:
			if markerAt(s[i:], l.lineComment) {
				hasComment = true
				i = len(s)
				continue
			}
			if l.jsxBraces && s[i] == '{' && strings.HasPrefix(s[i+1:], l.blockOpen) {
				i++
				continue
			}
			if l.blockOpen != "" && strings.HasPrefix(s[i:], l.blockOpen) {
				st.blockDepth = 1
				hasComment = true
				i += len(l.blockOpen)
				continue
			}
			if l.rawHash && (s[i] == 'r' || s[i] == 'b') && !identByte(prevByte(s, i)) {
				if n, closer := rustRawStart(s[i:]); n > 0 {
					st.raw = closer
					hasCode = true
					i += n
					continue
				}
			}
			if l.backtick && s[i] == '`' {
				st.raw = "`"
				hasCode = true
				i++
				continue
			}
			if l.doubleQuote && s[i] == '"' {
				hasCode = true
				i += consumeQuoted(s[i:], '"')
				continue
			}
			if s[i] == '\'' {
				switch l.singleQuote {
				case quoteString:
					hasCode = true
					i += consumeQuoted(s[i:], '\'')
					continue
				case quoteChar:
					if n := charLiteralLen(s[i:]); n > 0 {
						hasCode = true
						i += n
						continue
					}
				}
			}
			if !isSpace(s[i]) {
				hasCode = true
			}
			i++
		}
	}

	switch {
	case hasCode:
		return lineCode
	case hasComment:
		return lineComment
	default:
		return lineBlank
	}
}

// classifyText is the fallback for when a file's blob could not be read. It
// sees one line with no surrounding context, so it can only go by the shape of
// the text — including a bare "*", which is how gofmt and prettier continue a
// block comment.
func classifyText(l lang, s string) lineKind {
	t := strings.TrimSpace(s)
	if t == "" {
		return lineBlank
	}
	if !l.hasComments() {
		return lineCode
	}
	for _, m := range l.lineComment {
		if strings.HasPrefix(t, m) {
			return lineComment
		}
	}
	if l.blockOpen != "" && (strings.HasPrefix(t, l.blockOpen) || strings.HasPrefix(t, l.blockClose)) {
		return lineComment
	}
	if l.blockOpen == "/*" && strings.HasPrefix(t, "*") {
		return lineComment
	}
	return lineCode
}

func markerAt(s string, markers []string) bool {
	for _, m := range markers {
		if strings.HasPrefix(s, m) {
			return true
		}
	}
	return false
}

// consumeQuoted returns the byte length of the string literal opening at s[0].
// An unterminated double quote consumes the rest of the line (the line is
// malformed or continued, and nothing after it can be read as a comment); an
// unterminated single quote consumes just itself, because in practice it is an
// English apostrophe in JSX or YAML prose.
func consumeQuoted(s string, q byte) int {
	for i := 1; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == q {
			return i + 1
		}
	}
	if q == '\'' {
		return 1
	}
	return len(s)
}

// charLiteralLen distinguishes a rune/char literal from a Rust lifetime tick:
// a literal closes within a few bytes, a lifetime never closes at all. Bounding
// the search is what stops `&'a str` from swallowing the rest of the line.
func charLiteralLen(s string) int {
	if len(s) < 3 || s[0] != '\'' {
		return 0
	}
	if s[1] == '\\' {
		for j := 2; j < len(s) && j < 12; j++ {
			if s[j] == '\'' {
				return j + 1
			}
		}
		return 0
	}
	r, size := utf8.DecodeRuneInString(s[1:])
	if r == utf8.RuneError && size <= 1 {
		return 0
	}
	if 1+size < len(s) && s[1+size] == '\'' {
		return 1 + size + 1
	}
	return 0
}

// rustRawStart matches r"…" / r#"…"# / br#"…"# and returns the opening length
// plus the delimiter that closes it.
func rustRawStart(s string) (int, string) {
	i := 0
	if i < len(s) && s[i] == 'b' {
		i++
	}
	if i >= len(s) || s[i] != 'r' {
		return 0, ""
	}
	i++
	hashes := 0
	for i < len(s) && s[i] == '#' {
		hashes++
		i++
	}
	if i >= len(s) || s[i] != '"' {
		return 0, ""
	}
	return i + 1, `"` + strings.Repeat("#", hashes)
}

func prevByte(s string, i int) byte {
	if i == 0 {
		return 0
	}
	return s[i-1]
}

func identByte(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\v' || c == '\f'
}
