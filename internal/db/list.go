package db

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
