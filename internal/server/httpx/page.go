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
// (WriteList). A route resolves the two into a Page — the store-facing
// limit/offset window — with ResolvePage, which validates rather than repairs:
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
// PageSize 0 means "absent" — the zero value and an omitted field are the same
// request, and the default applies. An explicit 0 is therefore indistinguishable
// from absent and also yields the default; every other out-of-range value is
// rejected.
type PageRequest struct {
	PageSize  int    `json:"page_size"`
	PageToken string `json:"page_token"`
}

// Page is the resolved window a store read takes. It also carries the filter
// fingerprint the request was validated against, so WriteList mints the next
// token against the same filter set the caller asked for — the two halves of
// the token contract can't drift apart because only ResolvePage constructs one.
type Page struct {
	Limit  int
	Offset int

	fingerprint string
}

// pageToken is the decoded token payload. It is deliberately opaque on the
// wire (base64url of this JSON): offset-based paging is what the stores
// implement today — simple, uniform across arbitrary filter/sort combinations,
// and correct in both dialects — but a keyset cursor can replace this payload
// later without changing the wire contract, because no client is entitled to
// read or construct one. Anything a client can parse is something it will
// eventually depend on.
//
// F is a short hash of the canonicalized filter set the token was minted with.
// A token is only valid for that filter set: without the check, page 2 of one
// query could be requested with page 1's token of another, and the offset would
// silently address a different result set.
type pageToken struct {
	O int    `json:"o"`
	F string `json:"f"`
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
	limit := DefaultPageSize
	switch {
	case req.PageSize == 0:
		// Absent (or an explicit zero, which is the same JSON): default.
	case req.PageSize < 0 || req.PageSize > maxPageSize:
		v.OutOfRange("page_size", fmt.Sprintf("page_size must be between 1 and %d", maxPageSize))
	default:
		limit = req.PageSize
	}

	offset := 0
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
			offset = tok.O
		}
	}
	return Page{Limit: limit, Offset: offset, fingerprint: fingerprint}
}

func encodePageToken(tok pageToken) string {
	raw, err := json.Marshal(tok)
	if err != nil {
		// pageToken is two scalar fields; Marshal cannot fail. An empty token
		// reads as "last page", which is the safe degrade if it somehow does.
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
	return tok, nil
}

// listResponse is the wire shape every list read answers with. items is never
// null — an empty page is `[]`, so a client can iterate the field without a
// nil check. next_page_token is omitted on the last page rather than sent
// empty, and total_count is the filtered total, not the length of this page.
type listResponse[T any] struct {
	Items         []T    `json:"items"`
	NextPageToken string `json:"next_page_token,omitempty"`
	TotalCount    int    `json:"total_count"`
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
	WriteJSON(w, http.StatusOK, listResponse[T]{Items: items, NextPageToken: next, TotalCount: total})
}
