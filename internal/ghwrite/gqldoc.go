package ghwrite

import "strings"

// A GraphQL document reader that answers one question — which top-level fields
// does this request's mutation select? — and refuses to answer anything else.
//
// It is not a GraphQL implementation and must not grow into one. Everything
// below a top-level selection set is skipped as balanced punctuation: argument
// values, nested selections, directives, and every string in them are counted
// past without being interpreted. That bound is the point. This code runs in the
// process holding the run's live GitHub credential, on a document an agent
// composed from text the internet wrote, so the smaller its idea of the grammar,
// the smaller the surface that hostile input can reach.
//
// Two constructions are read exactly rather than skipped, because each is a way
// a request can perform an act under a name the reader would otherwise miss —
// and a mutation nobody can name is one the audit log cannot attribute:
//
//   - ALIASES. `x: mergePullRequest(...)` answers under `x`, so the response
//     side has nothing recognizable to key on. The request is the only place the
//     real field name still exists.
//   - FRAGMENTS. A top-level spread moves the field names into another
//     definition, and an inline fragment moves them one brace deeper. Both are
//     ordinary GraphQL that gh never emits and a hand-written call may. So a
//     top-level spread is followed into its definition, an inline fragment's
//     selections are read at the level they belong to, and what neither can
//     account for stays honestly unresolved.
//
// Following a spread does NOT widen what is interpreted: a fragment's top level
// is read exactly like an operation's — names only, everything under it skipped
// — and resolution touches each definition at most once, so a document cannot
// spend this process's time by nesting or by pointing fragments at each other.

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
//
// Hitting it is recorded rather than silently absorbed, because the refusal
// policy reads these names: a document that padded its selection until the
// gated field fell off the end would otherwise be a mutation nothing could see.
const maxTopLevelFields = 16

// gqlSelection is one top-level selection set as the reader could see it.
type gqlSelection struct {
	// fields are the field names selected directly here, aliases resolved. An
	// inline fragment's selections land here too: it introduces no new level of
	// selection, only a type condition, so its fields are this level's fields.
	fields []string
	// spreads names the fragments spread at this level, for the resolver to
	// follow.
	spreads []string
	// unresolvable marks a selection this reader will not account for, whatever
	// else it found: an inline fragment conditioned on a type other than the
	// mutation root. Collecting those fields would name an act from a branch the
	// server never executes.
	unresolvable bool
	// truncated marks a selection that named more fields than the reader keeps.
	// Distinct from unresolvable: nothing about the document defeated the
	// reader, it simply says more than a row can hold. Both leave the reading
	// partial, which is what the caller acts on.
	truncated bool
}

// gqlOperation is one operation definition as the reader could see it.
type gqlOperation struct {
	// typ is the operation keyword; an anonymous shorthand document is a query.
	typ string
	// name is the operation's name, empty when anonymous.
	name string
	// sel is the operation's own top-level selection, before any spread it names
	// has been followed.
	sel gqlSelection
}

// gqlFragment is one fragment definition: what it selects, and what it is
// conditioned on.
type gqlFragment struct {
	// on is the type condition. Only a fragment on the mutation root can
	// contribute fields to a mutation's top level; anything else spread there is
	// a document the server rejects, and reading its fields would name an act
	// that never ran.
	on  string
	sel gqlSelection
}

// gqlDocument is a parsed document: its operations, and the fragments they may
// spread.
type gqlDocument struct {
	ops       []gqlOperation
	fragments map[string]gqlFragment
}

// mutationRootType is the type a fragment must be conditioned on to contribute
// fields to a mutation's top level. GitHub's schema uses the spec's default
// name.
const mutationRootType = "Mutation"

// maxInlineFragmentDepth bounds how deep inline fragments may nest before the
// reader stops accounting for them. Real documents use one level; the bound is
// what keeps a crafted document from driving the read arbitrarily deep.
const maxInlineFragmentDepth = 8

// resolveTopLevelFields expands an operation's selection into the field names it
// actually invokes, following each spread into its definition. unreadable is
// empty when the whole selection was accounted for, and otherwise names why it
// was not — a spread the reader could not follow, or a selection wider than it
// keeps — in which case the caller records the mutation as unresolved rather
// than describing it from a partial reading.
//
// Both outcomes matter equally to the refusal policy, and neither is allowed to
// mask the other: a partial reading is exactly how a gated mutation becomes
// invisible, whether it was hidden behind an undefined fragment or pushed off
// the end of a padded selection set.
//
// Each fragment is followed at most once. That is what makes the work linear in
// the document rather than exponential in its nesting, and it is also why a
// cycle terminates: the second visit is skipped, not recursed into.
func resolveTopLevelFields(sel gqlSelection, fragments map[string]gqlFragment) (fields []string, unreadable string) {
	visited := make(map[string]bool)
	queue := []gqlSelection{sel}
	truncated := false

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.unresolvable {
			unreadable = GraphQLUnresolvedFragment
		}
		truncated = truncated || current.truncated
		for _, field := range current.fields {
			if len(fields) >= maxTopLevelFields {
				truncated = true
				break
			}
			fields = append(fields, field)
		}
		for _, name := range current.spreads {
			if visited[name] {
				continue
			}
			visited[name] = true
			fragment, defined := fragments[name]
			if !defined || fragment.on != mutationRootType {
				unreadable = GraphQLUnresolvedFragment
				continue
			}
			queue = append(queue, fragment.sel)
		}
	}
	if unreadable == "" && truncated {
		unreadable = GraphQLTooManyFields
	}
	return fields, unreadable
}

// parseDocument reads a document's top level: its operations, and the fragments
// they may spread. ok is false for anything it cannot walk exactly — an unknown
// definition keyword, an unterminated string, a selection set that never closes.
// A false here means "recorded as unclassified", never "assumed harmless".
func parseDocument(doc string) (gqlDocument, bool) {
	sc := &gqlScanner{src: doc}
	parsed := gqlDocument{fragments: map[string]gqlFragment{}}
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
				return gqlDocument{}, false
			}
			parsed.ops = append(parsed.ops, gqlOperation{typ: gqlOperationQuery})
		case tok.name("fragment"):
			name, fragment, walked := parseFragmentDefinition(sc)
			if !walked {
				return gqlDocument{}, false
			}
			// A redefined fragment is an invalid document the server rejects.
			// Keeping the first is arbitrary either way; what matters is that the
			// operation referencing it still resolves to something recorded.
			if _, seen := parsed.fragments[name]; !seen {
				parsed.fragments[name] = fragment
			}
		case tok.name(gqlOperationQuery), tok.name(gqlOperationMutation), tok.name(gqlOperationSubscription):
			op, walked := parseOperation(sc, tok.text)
			if !walked {
				return gqlDocument{}, false
			}
			parsed.ops = append(parsed.ops, op)
		default:
			return gqlDocument{}, false
		}
	}
	if sc.bad {
		return gqlDocument{}, false
	}
	return parsed, true
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
	sel, parsed := selectionFields(sc, 0)
	op.sel = sel
	return op, parsed
}

// selectionFields collects a selection set whose opening brace is already
// consumed, stopping at the matching close. Each field is read only as far as
// its name: arguments, directives, and any nested selection are skipped whole.
//
// depth counts inline fragments already entered, since those recurse at the same
// selection level rather than under it.
func selectionFields(sc *gqlScanner, depth int) (sel gqlSelection, ok bool) {
	for {
		tok, more := sc.next()
		if !more {
			return sel, false
		}
		if tok.punct("}") {
			return sel, true
		}
		if tok.punct("...") {
			if !sel.readFragmentSelection(sc, depth) {
				return sel, false
			}
			continue
		}
		if tok.kind != gqlKindName {
			return sel, false
		}

		field := tok.text
		if next, more := sc.peek(); more && next.punct(":") {
			// An alias: what precedes the colon is the caller's label for the
			// result, and what follows is the field actually invoked.
			sc.next()
			real, more := sc.next()
			if !more || real.kind != gqlKindName {
				return sel, false
			}
			field = real.text
		}
		if len(sel.fields) < maxTopLevelFields {
			sel.fields = append(sel.fields, field)
		} else {
			sel.truncated = true
		}

		if next, more := sc.peek(); more && next.punct("(") {
			sc.next()
			if !sc.skipBalanced("(", ")") {
				return sel, false
			}
		}
		if !skipDirectives(sc) {
			return sel, false
		}
		if next, more := sc.peek(); more && next.punct("{") {
			sc.next()
			if !sc.skipBalanced("{", "}") {
				return sel, false
			}
		}
	}
}

// readFragmentSelection consumes what follows a `...` inside a selection set: a
// named spread, which is recorded for the resolver to follow, or an inline
// fragment, whose selections belong to THIS level and are read into it.
//
// An inline fragment conditioned on anything but the mutation root is marked
// unresolvable rather than read. Its fields would name an act on a branch the
// server never takes, and a wrong verb in an audit log is worse than an honest
// refusal to name one.
func (sel *gqlSelection) readFragmentSelection(sc *gqlScanner, depth int) bool {
	condition := ""
	if tok, ok := sc.peek(); ok && tok.name("on") {
		sc.next()
		cond, more := sc.next()
		if !more || cond.kind != gqlKindName {
			return false
		}
		condition = cond.text
	} else if ok && tok.kind == gqlKindName {
		// A named spread. Its fields live in a definition the resolver follows.
		sc.next()
		sel.spreads = append(sel.spreads, tok.text)
		return skipDirectives(sc)
	}
	if !skipDirectives(sc) {
		return false
	}
	tok, ok := sc.peek()
	if !ok || !tok.punct("{") {
		// A type condition with no selection set: invalid, and nothing to read.
		return true
	}
	sc.next()

	if depth >= maxInlineFragmentDepth || (condition != "" && condition != mutationRootType) {
		sel.unresolvable = true
		return sc.skipBalanced("{", "}")
	}
	inner, walked := selectionFields(sc, depth+1)
	if !walked {
		return false
	}
	sel.merge(inner)
	return true
}

// merge folds an inline fragment's selection into the level that contains it.
func (sel *gqlSelection) merge(inner gqlSelection) {
	for _, field := range inner.fields {
		if len(sel.fields) >= maxTopLevelFields {
			sel.truncated = true
			break
		}
		sel.fields = append(sel.fields, field)
	}
	sel.spreads = append(sel.spreads, inner.spreads...)
	sel.unresolvable = sel.unresolvable || inner.unresolvable
	sel.truncated = sel.truncated || inner.truncated
}

// parseFragmentDefinition reads a top-level `fragment Name on Type { … }` with
// the keyword already consumed. Unlike an operation's, its selection is kept:
// an operation that spreads it is invoking these fields, and following that
// spread is the only way to name what such a request actually did.
func parseFragmentDefinition(sc *gqlScanner) (name string, fragment gqlFragment, ok bool) {
	ident, more := sc.next()
	if !more || ident.kind != gqlKindName {
		return "", fragment, false
	}
	on, more := sc.next()
	if !more || !on.name("on") {
		return "", fragment, false
	}
	cond, more := sc.next()
	if !more || cond.kind != gqlKindName {
		return "", fragment, false
	}
	if !skipDirectives(sc) {
		return "", fragment, false
	}
	brace, more := sc.next()
	if !more || !brace.punct("{") {
		return "", fragment, false
	}
	sel, walked := selectionFields(sc, 0)
	if !walked {
		return "", fragment, false
	}
	return ident.text, gqlFragment{on: cond.text, sel: sel}, true
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
