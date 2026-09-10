package db

import (
	"database/sql"
	"strings"
)

// ListOpts is the pagination window every paginated store read takes. It is
// deliberately dialect-neutral and filter-neutral: the filters are the read's
// own options type, this is only "which slice of that result set."
//
// Limit is the maximum number of rows to return and is always > 0 on a
// request-path call — the HTTP kernel resolves an absent page_size to its
// default before the store ever sees it. A zero Limit is treated by the impls
// as "no window" (every matching row), which is what the few internal callers
// that page nothing want; it is never reachable from a list route.
//
// Offset is the number of matching rows to skip. Offset paging is what the
// list contract's opaque page token encodes today; see internal/server/httpx's
// pageToken for why that choice is reversible.
//
// CountOnly asks the read for its filtered total and nothing else: the impl
// runs the count query it would have run anyway and returns an empty page
// without touching the row query. Limit and Offset are ignored when it is
// set. It exists for the explicit page_size: 0 request (httpx.Page.CountOnly
// hands it through) — the zero ListOpts stays "no window", so the two
// zero-adjacent meanings can't collide. Every List impl must honor it; one
// that doesn't would silently answer a count request with an unwindowed
// fetch of every matching row.
type ListOpts struct {
	Limit     int
	Offset    int
	CountOnly bool
}

// Unwindowed is the zero ListOpts, spelled so a call site that wants every
// matching row SAYS so.
//
// A read is legitimately unwindowed when its answer is a decision rather than
// a page: which teams the caller may act as, whether a referenced blueprint is
// visible to them, whether any handler still covers an event type. Those need
// the whole set — a page of it would answer a different question, and answer
// it wrongly (the row that decides the case is as likely to be on page two).
//
// Nothing that becomes an HTTP response body may use it. A route that returns
// rows resolves a page through httpx.ResolvePage and passes that window; the
// ratchet test in internal/server enforces the difference, which is the whole
// reason this constant exists rather than a bare `ListOpts{}` literal that
// reads the same at both kinds of call site.
var Unwindowed = ListOpts{}

// Facet is one value of a grouped-by column and the number of rows carrying
// it under a read's filters. It is the shape of a *synthetic* read — numbers
// about rows rather than rows — so it deliberately carries no page: a facet
// groups a closed vocabulary (an event type, a source), and the answer is
// bounded by that vocabulary rather than by a window the caller chose.
//
// The counts a facet reports must be the ones its sibling list would return
// under the same filters, which is why every facet read renders the list's
// own WHERE rather than a second one of its own.
type Facet struct {
	Value string
	Count int
}

// ScanFacets drains a (value, count) result into Facets. Both dialects share
// it so neither can disagree with the other about the empty answer: a filter
// nothing matches is an empty slice, never nil, the same way every other list
// read in this package answers.
func ScanFacets(rows *sql.Rows) ([]Facet, error) {
	out := []Facet{}
	for rows.Next() {
		var f Facet
		if err := rows.Scan(&f.Value, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// LikeEscape escapes the LIKE metacharacters in a literal so it matches
// itself. The escape character it inserts is a single backslash, and the
// statement must declare that same character — the clause is exactly
//
//	ESCAPE '\'
//
// one backslash between the quotes. Spelling it `ESCAPE '\\'` is not an
// escaped backslash: neither dialect treats a backslash as special inside a
// string literal, so those quotes hold a TWO-character string, and SQLite
// rejects it outright ("ESCAPE expression must be a single character"). That
// exact mistake shipped in the backfill-candidates predicate, where the query
// lived in a Go raw string and the doubling read as ordinary Go escaping.
// Declaring the character is still required rather than assumed, because
// neither dialect defaults to one.
func LikeEscape(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == '\\' || r == '%' || r == '_' {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
