package pollbench

import (
	"fmt"
	"math"
	"strconv"
	"unicode"
)

// gqlCost is the points a GraphQL query costs under GitHub's documented rule:
// count the requests every connection in the document needs, assuming each
// one reaches its first or last limit, so a connection nested under another
// costs one request per item of its parent; divide by 100 and round; the
// minimum is 1. A connection is a field with a first or last argument, or one
// that selects only totalCount, edges or pageInfo. nodes(ids:) is not a
// connection, but whatever it selects is fetched once per id, so its ids
// multiply the requests beneath it as a connection's limit does.
//
// It parses the document the client sends, so a change to a query's
// connections changes its cost here as it would on GitHub.
func gqlCost(query string, variables map[string]any) (int, error) {
	doc, err := parseGQL(query)
	if err != nil {
		return 0, err
	}
	requests := 0.0
	for _, op := range doc.operations {
		requests += doc.requests(op, 1, variables, map[string]bool{})
	}
	return max(1, int(math.Round(requests/100))), nil
}

type gqlField struct {
	name string
	args map[string]gqlValue
	sel  []gqlSelection
}

// gqlSelection is one entry of a selection set: a field, a fragment spread,
// or an inline fragment's own selection.
type gqlSelection struct {
	field  *gqlField
	spread string
	inline []gqlSelection
}

// gqlValue is an argument's value, as far as the cost needs it: an integer, a
// variable, or a list literal's length.
type gqlValue struct {
	isInt   bool
	i       int
	varName string
	listLen int
}

type gqlDoc struct {
	operations [][]gqlSelection
	fragments  map[string][]gqlSelection
}

func (d *gqlDoc) requests(sel []gqlSelection, mult float64, vars map[string]any, expanding map[string]bool) float64 {
	total := 0.0
	for _, s := range sel {
		switch {
		case s.field != nil:
			f := s.field
			child := mult
			if limit, ok := f.limit(vars); ok || f.selectsOnlyConnectionFields() {
				total += mult
				if ok {
					child = mult * float64(limit)
				}
			} else if f.name == "nodes" {
				if n, ok := f.listArg("ids", vars); ok {
					child = mult * float64(n)
				}
			}
			total += d.requests(f.sel, child, vars, expanding)
		case s.spread != "":
			if expanding[s.spread] {
				continue
			}
			expanding[s.spread] = true
			total += d.requests(d.fragments[s.spread], mult, vars, expanding)
			delete(expanding, s.spread)
		default:
			total += d.requests(s.inline, mult, vars, expanding)
		}
	}
	return total
}

// find is the first field named name anywhere in the document.
func (d *gqlDoc) find(name string) *gqlField {
	var walk func([]gqlSelection) *gqlField
	walk = func(sel []gqlSelection) *gqlField {
		for _, s := range sel {
			switch {
			case s.field != nil && s.field.name == name:
				return s.field
			case s.field != nil:
				if f := walk(s.field.sel); f != nil {
					return f
				}
			default:
				if f := walk(s.inline); f != nil {
					return f
				}
			}
		}
		return nil
	}
	for _, op := range d.operations {
		if f := walk(op); f != nil {
			return f
		}
	}
	for _, frag := range d.fragments {
		if f := walk(frag); f != nil {
			return f
		}
	}
	return nil
}

// limit is the field's first or last argument.
func (f *gqlField) limit(vars map[string]any) (int, bool) {
	for _, name := range []string{"first", "last"} {
		v, ok := f.args[name]
		if !ok {
			continue
		}
		if v.isInt {
			return v.i, true
		}
		if n, ok := vars[v.varName].(float64); ok {
			return int(n), true
		}
	}
	return 0, false
}

func (f *gqlField) listArg(name string, vars map[string]any) (int, bool) {
	v, ok := f.args[name]
	switch {
	case !ok:
		return 0, false
	case v.varName != "":
		list, ok := vars[v.varName].([]any)
		return len(list), ok
	default:
		return v.listLen, true
	}
}

// selectsOnlyConnectionFields reports a connection read with no limit: one
// that asks for its totalCount, edges or pageInfo and nothing else.
func (f *gqlField) selectsOnlyConnectionFields() bool {
	if len(f.sel) == 0 {
		return false
	}
	for _, s := range f.sel {
		if s.field == nil {
			return false
		}
		switch s.field.name {
		case "totalCount", "edges", "pageInfo":
		default:
			return false
		}
	}
	return true
}

// --- parsing -----------------------------------------------------------------

type gqlParser struct {
	toks []string
	pos  int
}

func parseGQL(src string) (*gqlDoc, error) {
	toks, err := lexGQL(src)
	if err != nil {
		return nil, err
	}
	p := &gqlParser{toks: toks}
	doc := &gqlDoc{fragments: map[string][]gqlSelection{}}
	for p.pos < len(p.toks) {
		switch tok := p.peek(); tok {
		case "{":
			sel, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, sel)
		case "query", "mutation", "subscription":
			p.next()
			if isGQLName(p.peek()) {
				p.next()
			}
			if p.peek() == "(" {
				if err := p.skipBalanced("(", ")"); err != nil {
					return nil, err
				}
			}
			p.skipDirectives()
			sel, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, sel)
		case "fragment":
			p.next()
			name := p.next()
			if p.next() != "on" {
				return nil, fmt.Errorf("graphql: fragment %s has no type condition", name)
			}
			p.next()
			p.skipDirectives()
			sel, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[name] = sel
		default:
			return nil, fmt.Errorf("graphql: unexpected %q at the top level", tok)
		}
	}
	return doc, nil
}

func (p *gqlParser) peek() string {
	if p.pos >= len(p.toks) {
		return ""
	}
	return p.toks[p.pos]
}

func (p *gqlParser) next() string {
	tok := p.peek()
	p.pos++
	return tok
}

func (p *gqlParser) expect(tok string) error {
	if got := p.next(); got != tok {
		return fmt.Errorf("graphql: expected %q, got %q", tok, got)
	}
	return nil
}

func (p *gqlParser) skipBalanced(open, close string) error {
	depth := 0
	for p.pos < len(p.toks) {
		switch p.next() {
		case open:
			depth++
		case close:
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
	return fmt.Errorf("graphql: unbalanced %q", open)
}

func (p *gqlParser) skipDirectives() {
	for p.peek() == "@" {
		p.next()
		p.next()
		if p.peek() == "(" {
			_ = p.skipBalanced("(", ")")
		}
	}
}

func (p *gqlParser) selectionSet() ([]gqlSelection, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var out []gqlSelection
	for p.peek() != "}" {
		if p.peek() == "" {
			return nil, fmt.Errorf("graphql: unterminated selection set")
		}
		if p.peek() == "..." {
			p.next()
			switch {
			case p.peek() == "on" || p.peek() == "{" || p.peek() == "@":
				if p.peek() == "on" {
					p.next()
					p.next()
				}
				p.skipDirectives()
				sel, err := p.selectionSet()
				if err != nil {
					return nil, err
				}
				out = append(out, gqlSelection{inline: sel})
			default:
				out = append(out, gqlSelection{spread: p.next()})
				p.skipDirectives()
			}
			continue
		}
		f, err := p.field()
		if err != nil {
			return nil, err
		}
		out = append(out, gqlSelection{field: f})
	}
	p.next()
	return out, nil
}

func (p *gqlParser) field() (*gqlField, error) {
	name := p.next()
	if !isGQLName(name) {
		return nil, fmt.Errorf("graphql: expected a field name, got %q", name)
	}
	if p.peek() == ":" {
		p.next()
		name = p.next()
	}
	f := &gqlField{name: name, args: map[string]gqlValue{}}
	if p.peek() == "(" {
		p.next()
		for p.peek() != ")" {
			arg := p.next()
			if err := p.expect(":"); err != nil {
				return nil, err
			}
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			f.args[arg] = v
		}
		p.next()
	}
	p.skipDirectives()
	if p.peek() == "{" {
		sel, err := p.selectionSet()
		if err != nil {
			return nil, err
		}
		f.sel = sel
	}
	return f, nil
}

func (p *gqlParser) value() (gqlValue, error) {
	switch tok := p.next(); tok {
	case "$":
		return gqlValue{varName: p.next()}, nil
	case "[":
		n := 0
		for p.peek() != "]" {
			if p.peek() == "" {
				return gqlValue{}, fmt.Errorf("graphql: unterminated list")
			}
			if _, err := p.value(); err != nil {
				return gqlValue{}, err
			}
			n++
		}
		p.next()
		return gqlValue{listLen: n}, nil
	case "{":
		p.pos--
		return gqlValue{}, p.skipBalanced("{", "}")
	default:
		if i, err := strconv.Atoi(tok); err == nil {
			return gqlValue{isInt: true, i: i}, nil
		}
		return gqlValue{}, nil
	}
}

func isGQLName(tok string) bool {
	if tok == "" {
		return false
	}
	for i, r := range tok {
		if r != '_' && !unicode.IsLetter(r) && (i == 0 || !unicode.IsDigit(r)) {
			return false
		}
	}
	return true
}

// lexGQL splits a document into tokens: punctuators, names, numbers, and
// strings, dropping whitespace, commas and comments.
func lexGQL(src string) ([]string, error) {
	var toks []string
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',':
			i++
		case c == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '.':
			if i+2 >= len(src) || src[i+1] != '.' || src[i+2] != '.' {
				return nil, fmt.Errorf("graphql: stray '.' at %d", i)
			}
			toks = append(toks, "...")
			i += 3
		case c == '"':
			j := i + 1
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(src) {
				return nil, fmt.Errorf("graphql: unterminated string at %d", i)
			}
			toks = append(toks, src[i:j+1])
			i = j + 1
		case c == '-' || (c >= '0' && c <= '9'):
			j := i + 1
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.' || src[j] == 'e' || src[j] == 'E') {
				j++
			}
			toks = append(toks, src[i:j])
			i = j
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i + 1
			for j < len(src) && (src[j] == '_' || src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z' || src[j] >= '0' && src[j] <= '9') {
				j++
			}
			toks = append(toks, src[i:j])
			i = j
		default:
			toks = append(toks, string(c))
			i++
		}
	}
	return toks, nil
}
