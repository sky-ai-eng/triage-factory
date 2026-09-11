package httpx

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
)

// --- list pagination ---
//
// Every paginated read on /api/* speaks this one contract: the request body
// carries `page_size` + `page_token` (PageRequest, embedded in the route's own
// filter struct), and the response is `{items, next_page_token, total_count}`
// (WriteList). A route resolves the two into a Page — the store-facing window,
// a size plus a position — with ResolvePage, which validates rather than repairs:
// a page_size outside the range is a 400, never a clamp, so a caller asking
// for 500 rows learns it didn't get them.

const (
	// DefaultPageSize is the window a list body with no page_size gets.
	DefaultPageSize = 50
	// MaxPageSize is the largest window a list route accepts. A route with a
	// genuine reason to allow more passes its own max to ResolvePage — it
	// declares the difference, never widens silently.
	MaxPageSize = 200
)

// PageRequest is the paging half of a list body. Embed it in the route's
// filter struct so both halves decode from one JSON object:
//
//	type taskListRequest struct {
//	    Statuses []string `json:"statuses"`
//	    httpx.PageRequest
//	}
//
// PageSize is a pointer because absent and zero are different requests —
// the same absent-vs-explicit distinction the PATCH contract draws with
// json.RawMessage. Absent (nil) takes the default window. An explicit 0 is
// the count-only read: no items, total_count under the same filters —
// stat() on the directory instead of read() — so a caller rendering a
// figure doesn't pay for a page of rows it discards. Every other value
// outside 1..max is rejected.
type PageRequest struct {
	PageSize  *int   `json:"page_size"`
	PageToken string `json:"page_token"`
}

// Page is the resolved window a store read takes. It also carries the filter
// fingerprint the request was validated against, so WriteList mints the next
// token against the same filter set the caller asked for — the two halves of
// the token contract can't drift apart because only ResolvePage constructs one.
type Page struct {
	Limit  int
	Offset int

	// After is the keyset position a store-backed list resumes from: the
	// previous page's last row rendered as one string per ORDER BY term. It
	// is the alternative to Offset, never a companion — a route that mints
	// keyset tokens leaves Offset at 0 and vice versa — and it is what makes
	// a page correct on a result set that mutates while it is being read.
	// Empty on the first page and on every route that still pages by offset.
	After []string

	// CountOnly marks the explicit page_size: 0 request. Limit is 0 then,
	// and 0 means "no window" to a store (db.Unwindowed), so a handler must
	// hand this flag to db.ListOpts rather than let a zero Limit through —
	// the impls run only their count query and return an empty page.
	CountOnly bool

	// Cursor is the upstream position a proxy list resumes from — see
	// WriteProxyList. Empty on the first page and on every store-backed list,
	// which pages by Offset instead.
	Cursor string

	fingerprint string
}

// pageToken is the decoded token payload. It is deliberately opaque on the
// wire (base64url of this JSON), which is what let the keyset form below land
// without moving anything a client sees: no client is entitled to read or
// construct one, so the payload is ours to change. Anything a client can parse
// is something it will eventually depend on.
//
// F is a short hash of the canonicalized filter set the token was minted with.
// A token is only valid for that filter set: without the check, page 2 of one
// query could be requested with page 1's token of another, and the position
// would silently address a different result set.
//
// O, C and K are the three mutually exclusive positions a token can carry, and
// decodePageToken refuses a token holding more than one:
//
//   - O is an offset — how many matching rows to skip. Simple and uniform
//     across arbitrary filter/sort combinations, and wrong on a result set
//     that mutates between pages: a row removed above the cut makes page 2
//     skip one, a row inserted above it makes page 2 repeat one.
//   - K is a keyset — the previous page's last row rendered as one string per
//     ORDER BY term, which a store compares against the same tuple to resume
//     exactly where the last page stopped, whatever happened above the cut.
//     It is per-route: a route opts in by minting through WriteListKeyset.
//   - C is an upstream position for a proxy list (see WriteProxyList), which
//     has no offset of its own to advance.
type pageToken struct {
	O int      `json:"o"`
	F string   `json:"f"`
	C string   `json:"c,omitempty"`
	K []string `json:"k,omitempty"`
}

// FilterFingerprint returns a short, stable hash of a list route's filter set.
// Pass a struct (or any JSON-marshalable value) holding the canonicalized
// filters — slices sorted and deduped, timestamps normalized — so two requests
// that mean the same query fingerprint the same. It is a mistake-detector, not
// a security boundary: a collision costs a caller a page of the wrong query,
// which is exactly what an unvalidated token costs today.
func FilterFingerprint(filters any) string {
	raw, err := json.Marshal(filters)
	if err != nil {
		// Marshal only fails on shapes the canonical filter structs don't have
		// (channels, funcs, cycles). Fall back to the Go rendering rather than
		// fingerprinting the empty string, which would make every filter set
		// look identical.
		raw = []byte(fmt.Sprintf("%#v", filters))
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:6])
}

// ResolvePage validates a list body's paging fields and returns the window the
// store read takes. Faults are appended to v (the caller flushes them with the
// route's other field errors, so one response reports every problem):
//
//   - page_size outside 1..maxPageSize → OUT_OF_RANGE. Not clamped: a silently
//     truncated window is a wrong answer that looks like a right one.
//   - an unparseable page_token, or one minted for a different filter set →
//     INVALID_PARAM.
//
// maxPageSize of 0 means MaxPageSize. fingerprint is the caller's
// FilterFingerprint over its canonicalized filters.
//
// The returned Page is meaningful only when v flushed nothing; on a fault it
// is the default window, which the caller never reaches.
func ResolvePage(v *Validation, req PageRequest, fingerprint string, maxPageSize int) Page {
	if maxPageSize <= 0 {
		maxPageSize = MaxPageSize
	}
	limit, countOnly := DefaultPageSize, false
	switch {
	case req.PageSize == nil:
		// Absent: the default window.
	case *req.PageSize == 0:
		// Explicit zero: the count-only read. The store still computes the
		// filtered total; it just never runs the page query.
		limit, countOnly = 0, true
	case *req.PageSize < 0 || *req.PageSize > maxPageSize:
		v.OutOfRange("page_size", fmt.Sprintf("page_size must be between 0 and %d; 0 returns only total_count", maxPageSize))
	default:
		limit = *req.PageSize
	}

	offset, cursor := 0, ""
	var after []string
	if req.PageToken != "" {
		tok, err := decodePageToken(req.PageToken)
		switch {
		case err != nil:
			v.Add(ErrorItem{
				Reason:  ReasonInvalidParam,
				Message: "page_token is not a valid page token",
				Field:   "page_token",
			})
		case tok.F != fingerprint:
			v.Add(ErrorItem{
				Reason:  ReasonInvalidParam,
				Message: "page_token was issued for a different set of filters; restart from the first page",
				Field:   "page_token",
			})
		default:
			offset, cursor, after = tok.O, tok.C, tok.K
		}
	}
	return Page{Limit: limit, Offset: offset, After: after, CountOnly: countOnly, Cursor: cursor, fingerprint: fingerprint}
}

func encodePageToken(tok pageToken) string {
	raw, err := json.Marshal(tok)
	if err != nil {
		// pageToken is scalars and a string slice; Marshal cannot fail. An
		// empty token reads as "last page", which is the safe degrade if it
		// somehow does.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodePageToken(s string) (pageToken, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return pageToken{}, err
	}
	var tok pageToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return pageToken{}, err
	}
	if tok.O < 0 {
		return pageToken{}, fmt.Errorf("negative offset %d", tok.O)
	}
	// The three positions are alternatives. A token carrying two of them was
	// not minted here, and guessing which one to honor is how a caller ends up
	// paging a result set from a position nothing produced.
	//
	// A zero O is "no offset", not "offset zero": the two are the same eight
	// bytes on the wire, and a minted keyset token carries no O at all. Only
	// the first page is at offset zero, and the first page is the one nobody
	// needs a token for — so reading a zero as a position held would refuse
	// every keyset token there is.
	positions := 0
	for _, held := range []bool{tok.O != 0, tok.C != "", len(tok.K) > 0} {
		if held {
			positions++
		}
	}
	if positions > 1 {
		return pageToken{}, fmt.Errorf("page token carries %d positions, want at most 1", positions)
	}
	return tok, nil
}

// listResponse is the wire shape every list read answers with. items is never
// null — an empty page is `[]`, so a client can iterate the field without a
// nil check. next_page_token is omitted on the last page rather than sent
// empty, and total_count is the filtered total, not the length of this page.
//
// total_count is a pointer so a proxy list can send JSON null: the field is
// always present (a client reads one shape everywhere) and null says "this
// resource cannot count itself" rather than claiming zero. See WriteProxyList.
type listResponse[T any] struct {
	Items         []T    `json:"items"`
	NextPageToken string `json:"next_page_token,omitempty"`
	TotalCount    *int   `json:"total_count"`
}

// WriteList writes the list envelope for one page: the items, the filtered
// total, and the token for the next page when one exists. page is the Page
// ResolvePage returned for this request, which is what ties the next token to
// the same filter set — a caller cannot mint a token for filters it didn't ask
// for, because it never touches the fingerprint itself.
func WriteList[T any](w http.ResponseWriter, page Page, items []T, total int) {
	if items == nil {
		items = []T{}
	}
	next := ""
	if end := page.Offset + len(items); end < total && len(items) > 0 {
		next = encodePageToken(pageToken{O: end, F: page.fingerprint})
	}
	WriteJSON(w, http.StatusOK, listResponse[T]{Items: items, NextPageToken: next, TotalCount: &total})
}

// WriteListKeyset writes the list envelope for one page of a keyset-paged
// read — the same wire shape WriteList writes, minting a keyset position
// instead of an offset. A route opts in by calling this instead; the tokens
// the two mint are interchangeable to a client and distinguishable only to
// ResolvePage, so converting one route leaves every other list alone.
//
// nextKey is the ORDER BY tuple of this page's last row — one string per term,
// in the order's own term order — or nil when there is no page after this one.
// A nil nextKey is the ONLY "last page" signal here: unlike WriteList there is
// no offset to compare against total, because a keyset page does not know its
// own position in the result set, and must not — a row deleted above the cut
// is exactly what makes that arithmetic lie. So the caller proves it: it asks
// the store for one row past the window and passes nil when that row wasn't
// there.
//
// The key is not reflected off T. A route knows which columns its order ran
// on; this writer would have to guess, and a guess that compiles is a guess
// that pages wrong.
func WriteListKeyset[T any](w http.ResponseWriter, page Page, items []T, total int, nextKey []string) {
	if items == nil {
		items = []T{}
	}
	next := ""
	if len(nextKey) > 0 && len(items) > 0 {
		next = encodePageToken(pageToken{F: page.fingerprint, K: nextKey})
	}
	WriteJSON(w, http.StatusOK, listResponse[T]{Items: items, NextPageToken: next, TotalCount: &total})
}

// NextPageToken mints the token for the page after this one, or "" when this
// is the last. It is the token half of WriteList, exported for the rare route
// whose response body cannot be the flat list envelope — the conversation list
// groups its rows by task id, and a map is not a slice — so it writes its own
// shape while still speaking the same paging contract.
//
// pageLen is how many rows this response carries; total is the filtered total.
// Prefer WriteList: a route that mints its own token is a route that can get
// the arithmetic wrong.
func NextPageToken(page Page, pageLen, total int) string {
	if end := page.Offset + pageLen; end < total && pageLen > 0 {
		return encodePageToken(pageToken{O: end, F: page.fingerprint})
	}
	return ""
}

// WriteProxyList writes the list envelope for a page this server did not
// count: the rows come from an upstream API that pages by its own cursor and
// reports no total. nextCursor is the upstream position the following page
// resumes from, or "" when the upstream said there isn't one; it is wrapped in
// the same opaque page_token as an offset, fingerprinted against the same
// filter set, so the client's loop is byte-identical to a store-backed list's.
//
// **total_count is null on a proxy list, and that is contract, not omission.**
// The alternative would be counting the upstream ourselves — walking every
// remaining page on the first request — which is exactly the fetch-the-world
// read pagination exists to retire. A client renders "showing N" rather than
// "N of M" here; a null total is never a zero total, and never a truncation
// signal (next_page_token is the only "there is more" signal on any list).
func WriteProxyList[T any](w http.ResponseWriter, page Page, items []T, nextCursor string) {
	if items == nil {
		items = []T{}
	}
	next := ""
	if nextCursor != "" && len(items) > 0 {
		next = encodePageToken(pageToken{F: page.fingerprint, C: nextCursor})
	}
	WriteJSON(w, http.StatusOK, listResponse[T]{Items: items, NextPageToken: next})
}
