package ghwrite

import "strings"

// A GraphQL document reader that answers one question — which top-level fields
// does this request's mutation select? — and refuses to answer anything else.
//
// It is not a GraphQL implementation and must not grow into one. Everything
// below the operation's own selection set is skipped as balanced punctuation:
// argument values, nested selections, directives, and every string in them are
// counted past without being interpreted. That bound is the point. This code
// runs in the process holding the run's live GitHub credential, on a document
// an agent composed from text the internet wrote, so the smaller its idea of
// the grammar, the smaller the surface that hostile input can reach.
//
// What it does need to be exact about is the field NAME, because an alias
// (`x: mergePullRequest(...)`) is the one evasion the response side cannot
// catch — a response keyed under `x` names nothing recognizable. So aliases are
// resolved here, at the only place the real name is still visible.

// gqlOperationType names the three operation keywords. Only mutations are
// audited; the other two are recognized so a document that mixes them can still
// be resolved to the one operation that ran.
const (
	gqlOperationQuery        = "query"
	gqlOperationMutation     = "mutation"
	gqlOperationSubscription = "subscription"
)

// maxTopLevelFields bounds how many field names one operation contributes. A
// document selecting more than this is already unclassifiable — the table names
// single acts, and anything past one field takes the fallback — so the excess
// would only pad an audit row's detail.
const maxTopLevelFields = 16

// gqlOperation is one operation definition as the reader could see it.
type gqlOperation struct {
	// typ is the operation keyword; an anonymous shorthand document is a query.
	typ string
	// name is the operation's name, empty when anonymous.
	name string
	// fields are the top-level selection's field names, aliases resolved.
	fields []string
	// spread marks a top-level fragment spread or inline fragment. The fields it
	// would contribute live in another definition (or in none, for a spread of a
	// fragment the document never defines), and this reader deliberately does not
	// follow them — so an operation carrying one is never fully described by its
	// fields alone.
	spread bool
}

// parseOperations reads a document's top level. ok is false for anything it
// cannot walk exactly: an unknown definition keyword, an unterminated string, a
// selection set that never closes. A false here means "recorded as
// unclassified", never "assumed harmless".
func parseOperations(doc string) (ops []gqlOperation, ok bool) {
	sc := &gqlScanner{src: doc}
	for {
		tok, more := sc.next()
		if !more {
			break
		}
		switch {
		case tok.punct("{"):
			// Shorthand: a bare selection set is a query by the spec, which is
			// exactly why the mutation keyword is a reliable prescreen.
			if !sc.skipBalanced("{", "}") {
				return nil, false
			}
			ops = append(ops, gqlOperation{typ: gqlOperationQuery})
		case tok.name("fragment"):
			if !skipFragmentDefinition(sc) {
				return nil, false
			}
		case tok.name(gqlOperationQuery), tok.name(gqlOperationMutation), tok.name(gqlOperationSubscription):
			op, parsed := parseOperation(sc, tok.text)
			if !parsed {
				return nil, false
			}
			ops = append(ops, op)
		default:
			return nil, false
		}
	}
	if sc.bad {
		return nil, false
	}
	return ops, true
}

// parseOperation reads one operation definition with its keyword already
// consumed: an optional name, optional variable definitions, optional
// directives, then the selection set this reader actually cares about.
func parseOperation(sc *gqlScanner, typ string) (gqlOperation, bool) {
	op := gqlOperation{typ: typ}
	if tok, ok := sc.peek(); ok && tok.kind == gqlKindName {
		sc.next()
		op.name = tok.text
	}
	if tok, ok := sc.peek(); ok && tok.punct("(") {
		sc.next()
		if !sc.skipBalanced("(", ")") {
			return op, false
		}
	}
	if !skipDirectives(sc) {
		return op, false
	}
	tok, ok := sc.next()
	if !ok || !tok.punct("{") {
		return op, false
	}
	fields, spread, parsed := selectionFields(sc)
	op.fields, op.spread = fields, spread
	return op, parsed
}

// selectionFields collects the field names of a selection set whose opening
// brace is already consumed, stopping at the matching close. Each selection is
// read only as far as its name: arguments, directives, and any nested selection
// are skipped whole.
func selectionFields(sc *gqlScanner) (fields []string, spread bool, ok bool) {
	for {
		tok, more := sc.next()
		if !more {
			return fields, spread, false
		}
		if tok.punct("}") {
			return fields, spread, true
		}
		if tok.punct("...") {
			spread = true
			if !skipFragmentSelection(sc) {
				return fields, spread, false
			}
			continue
		}
		if tok.kind != gqlKindName {
			return fields, spread, false
		}

		field := tok.text
		if next, more := sc.peek(); more && next.punct(":") {
			// An alias: what precedes the colon is the caller's label for the
			// result, and what follows is the field actually invoked.
			sc.next()
			real, more := sc.next()
			if !more || real.kind != gqlKindName {
				return fields, spread, false
			}
			field = real.text
		}
		if len(fields) < maxTopLevelFields {
			fields = append(fields, field)
		}

		if next, more := sc.peek(); more && next.punct("(") {
			sc.next()
			if !sc.skipBalanced("(", ")") {
				return fields, spread, false
			}
		}
		if !skipDirectives(sc) {
			return fields, spread, false
		}
		if next, more := sc.peek(); more && next.punct("{") {
			sc.next()
			if !sc.skipBalanced("{", "}") {
				return fields, spread, false
			}
		}
	}
}

// skipFragmentSelection consumes what follows a `...` inside a selection set —
// a named spread, or an inline fragment with or without a type condition.
func skipFragmentSelection(sc *gqlScanner) bool {
	if tok, ok := sc.peek(); ok && tok.name("on") {
		sc.next()
		cond, more := sc.next()
		if !more || cond.kind != gqlKindName {
			return false
		}
	} else if ok && tok.kind == gqlKindName {
		sc.next()
	}
	if !skipDirectives(sc) {
		return false
	}
	if tok, ok := sc.peek(); ok && tok.punct("{") {
		sc.next()
		return sc.skipBalanced("{", "}")
	}
	return true
}

// skipFragmentDefinition consumes a top-level `fragment Name on Type { … }`
// with the keyword already read. Its selections are never collected: a
// definition is not an operation, and this reader does not resolve the spreads
// that would pull it into one.
func skipFragmentDefinition(sc *gqlScanner) bool {
	name, ok := sc.next()
	if !ok || name.kind != gqlKindName {
		return false
	}
	on, ok := sc.next()
	if !ok || !on.name("on") {
		return false
	}
	cond, ok := sc.next()
	if !ok || cond.kind != gqlKindName {
		return false
	}
	if !skipDirectives(sc) {
		return false
	}
	brace, ok := sc.next()
	if !ok || !brace.punct("{") {
		return false
	}
	return sc.skipBalanced("{", "}")
}

// skipDirectives consumes any run of `@name(args)`. Directives can appear at
// almost every position in the grammar and none of them change which field was
// invoked, so they are skipped everywhere rather than parsed anywhere.
func skipDirectives(sc *gqlScanner) bool {
	for {
		tok, ok := sc.peek()
		if !ok || !tok.punct("@") {
			return true
		}
		sc.next()
		name, more := sc.next()
		if !more || name.kind != gqlKindName {
			return false
		}
		if next, more := sc.peek(); more && next.punct("(") {
			sc.next()
			if !sc.skipBalanced("(", ")") {
				return false
			}
		}
	}
}

// gqlKind is the token classes this reader distinguishes. Strings and numbers
// collapse into one "value" class because nothing here ever reads their
// contents — they exist so that a brace inside a comment body can never be
// mistaken for structure.
type gqlKind uint8

const (
	gqlKindName gqlKind = iota
	gqlKindPunct
	gqlKindValue
)

type gqlToken struct {
	kind gqlKind
	text string
}

func (t gqlToken) punct(s string) bool { return t.kind == gqlKindPunct && t.text == s }
func (t gqlToken) name(s string) bool  { return t.kind == gqlKindName && t.text == s }

// gqlScanner tokenizes a document. bad records that scanning hit something it
// could not tokenize — an unterminated string, a stray byte — which the parser
// reports as an unreadable document rather than guessing past.
type gqlScanner struct {
	src     string
	pos     int
	bad     bool
	held    gqlToken
	holding bool
}

// next returns the next token; ok is false at the end of the document and on a
// scan error, which bad distinguishes.
func (s *gqlScanner) next() (gqlToken, bool) {
	if s.holding {
		s.holding = false
		return s.held, true
	}
	return s.scan()
}

// peek reads the next token without consuming it.
func (s *gqlScanner) peek() (gqlToken, bool) {
	if s.holding {
		return s.held, true
	}
	tok, ok := s.scan()
	if !ok {
		return gqlToken{}, false
	}
	s.held, s.holding = tok, true
	return tok, true
}

// skipBalanced consumes tokens until the delimiter matching an already-consumed
// opener closes. Nesting counts, and because strings are whole tokens, a
// delimiter inside one is invisible here — which is what makes skipping an
// argument list safe when its values are attacker-authored text.
func (s *gqlScanner) skipBalanced(open, close string) bool {
	for depth := 1; ; {
		tok, ok := s.next()
		if !ok {
			return false
		}
		if tok.kind != gqlKindPunct {
			continue
		}
		switch tok.text {
		case open:
			depth++
		case close:
			if depth--; depth == 0 {
				return true
			}
		}
	}
}

// gqlPunctuators are the single-character punctuators of the grammar; `...` is
// scanned ahead of them as the one multi-character token.
const gqlPunctuators = "!$&():=@[]{|}"

// gqlByteOrderMark is ignored wherever whitespace is, per the spec.
const gqlByteOrderMark = "\uFEFF"

func (s *gqlScanner) scan() (gqlToken, bool) {
	s.skipIgnored()
	if s.pos >= len(s.src) {
		return gqlToken{}, false
	}
	c := s.src[s.pos]

	if strings.HasPrefix(s.src[s.pos:], "...") {
		s.pos += 3
		return gqlToken{kind: gqlKindPunct, text: "..."}, true
	}
	if strings.IndexByte(gqlPunctuators, c) >= 0 {
		s.pos++
		return gqlToken{kind: gqlKindPunct, text: string(c)}, true
	}
	if c == '"' {
		return s.scanString()
	}
	if isNameStart(c) {
		start := s.pos
		for s.pos < len(s.src) && isNameContinue(s.src[s.pos]) {
			s.pos++
		}
		return gqlToken{kind: gqlKindName, text: s.src[start:s.pos]}, true
	}
	if c == '-' || (c >= '0' && c <= '9') {
		s.pos++
		for s.pos < len(s.src) && isNumberContinue(s.src[s.pos]) {
			s.pos++
		}
		return gqlToken{kind: gqlKindValue}, true
	}

	// A byte the grammar has no place for. Stopping here (rather than skipping
	// it) is what keeps a document this reader cannot account for out of the
	// classified set.
	s.bad = true
	return gqlToken{}, false
}

// skipIgnored advances past whitespace, commas, comments, and a leading BOM —
// the tokens the spec calls ignored.
func (s *gqlScanner) skipIgnored() {
	for s.pos < len(s.src) {
		switch c := s.src[s.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			s.pos++
		case c == '#':
			for s.pos < len(s.src) && s.src[s.pos] != '\n' {
				s.pos++
			}
		case strings.HasPrefix(s.src[s.pos:], gqlByteOrderMark):
			s.pos += len(gqlByteOrderMark)
		default:
			return
		}
	}
}

// scanString consumes a string value, block or single-line, without keeping it.
// The contents are never read — only their extent matters, so that punctuation
// inside a pull-request body cannot be mistaken for document structure.
func (s *gqlScanner) scanString() (gqlToken, bool) {
	if strings.HasPrefix(s.src[s.pos:], `"""`) {
		rest := s.src[s.pos+3:]
		for i := 0; i+3 <= len(rest); i++ {
			if rest[i] == '\\' {
				i++
				continue
			}
			if strings.HasPrefix(rest[i:], `"""`) {
				s.pos += 3 + i + 3
				return gqlToken{kind: gqlKindValue}, true
			}
		}
		s.bad = true
		return gqlToken{}, false
	}
	for i := s.pos + 1; i < len(s.src); i++ {
		switch s.src[i] {
		case '\\':
			i++
		case '\n':
			// A newline cannot appear in a single-line string, so the quote that
			// opened this one is unterminated.
			s.bad = true
			return gqlToken{}, false
		case '"':
			s.pos = i + 1
			return gqlToken{kind: gqlKindValue}, true
		}
	}
	s.bad = true
	return gqlToken{}, false
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameContinue(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

func isNumberContinue(c byte) bool {
	return (c >= '0' && c <= '9') || c == '.' || c == 'e' || c == 'E' || c == '+' || c == '-'
}
