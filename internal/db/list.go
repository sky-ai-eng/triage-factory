package db

import "strings"

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
type ListOpts struct {
	Limit  int
	Offset int
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

// LikeEscape escapes the LIKE metacharacters in a literal so it matches
// itself. Callers pair it with `ESCAPE '\\'` in the statement. Both dialects
// treat `\\` as an ordinary character in a string literal by default, which is
// why the escape character must be declared explicitly rather than assumed.
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
