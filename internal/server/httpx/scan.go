package httpx

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// --- scanned proxy lists ---
//
// Some proxy lists narrow rows their upstream cannot: GitHub's branches
// endpoint takes no search, and a Linear search is re-checked literally. Such
// a list cannot answer one upstream page per request, because the rows that
// survive the narrowing are a fraction of it: a page of matches would come
// back short, and a page with none would end the walk while later pages still
// match. ScanUpstream fills the list's page from as many upstream pages as it
// takes, and the cursor it answers resumes exactly where it stopped.

// MaxScanReads bounds the upstream pages one request reads. A request that
// reaches it answers what it found, empty or not, with a token that resumes
// the scan, so a sparse match costs the caller more requests rather than a
// request that runs unbounded.
const MaxScanReads = 10

// ScanCursor is a scanned list's position: the upstream page to read next
// ("" is the first) and how many of that page's kept rows have already been
// answered.
type ScanCursor struct {
	upstream string
	skip     int
}

// ScanCursorFrom decodes page.Cursor as a ScanCursor. A cursor this server
// did not mint is an INVALID_PARAM fault on page_token.
func ScanCursorFrom(v *Validation, page Page) ScanCursor {
	if page.Cursor == "" {
		return ScanCursor{}
	}
	skip, upstream, ok := strings.Cut(page.Cursor, ":")
	n, err := strconv.Atoi(skip)
	if !ok || err != nil || n < 0 {
		v.Add(ErrorItem{
			Reason:  ReasonInvalidParam,
			Message: "page_token does not address this list; restart from the first page",
			Field:   "page_token",
		})
		return ScanCursor{}
	}
	return ScanCursor{upstream: upstream, skip: n}
}

func (c ScanCursor) String() string {
	return strconv.Itoa(c.skip) + ":" + c.upstream
}

// errScanStalled is an upstream answering the cursor it was asked for as the
// page after it, which would read the same page forever.
var errScanStalled = errors.New("upstream listing cursor did not advance")

// ScanUpstream fills one page of a scanned list, from up to MaxScanReads
// upstream pages. read answers one upstream page from an upstream cursor ("" is
// the first) and the cursor of the page after it ("" when there is none); keep
// is the list's narrowing. It returns at most limit kept rows, in the
// upstream's order, and the cursor the next page resumes from: "" when the
// upstream has no more, and otherwise a position that may sit partway through
// an upstream page. Pass that cursor to WriteScannedProxyList.
func ScanUpstream[T any](from ScanCursor, limit int, keep func(T) bool, read func(upstream string) ([]T, string, error)) ([]T, string, error) {
	out := make([]T, 0, max(limit, 0))
	if limit <= 0 {
		return out, "", nil
	}
	at := from
	for range MaxScanReads {
		rows, next, err := read(at.upstream)
		if err != nil {
			return nil, "", err
		}
		kept := make([]T, 0, len(rows))
		for _, row := range rows {
			if keep(row) {
				kept = append(kept, row)
			}
		}
		// A page that lost rows since the cursor was minted has fewer to skip.
		skip := min(at.skip, len(kept))
		kept = kept[skip:]
		if room := limit - len(out); len(kept) > room {
			out = append(out, kept[:room]...)
			return out, ScanCursor{upstream: at.upstream, skip: skip + room}.String(), nil
		}
		out = append(out, kept...)
		if next == "" {
			return out, "", nil
		}
		if next == at.upstream {
			return nil, "", fmt.Errorf("%w: %q", errScanStalled, next)
		}
		at = ScanCursor{upstream: next}
		if len(out) == limit {
			break
		}
	}
	return out, at.String(), nil
}

// WriteScannedProxyList is WriteProxyList for a scanned list. It mints a token
// for an empty page too: there, an empty page with a token is an answer, that
// MaxScanReads upstream pages held no match and the scan goes on from where
// they ended. ScanUpstream only answers one after advancing the upstream
// cursor, so following the tokens always reaches the upstream's end.
func WriteScannedProxyList[T any](w http.ResponseWriter, page Page, items []T, next string) {
	if items == nil {
		items = []T{}
	}
	token := ""
	if next != "" {
		token = encodePageToken(pageToken{F: page.fingerprint, C: next})
	}
	WriteJSON(w, http.StatusOK, listResponse[T]{Items: items, NextPageToken: token})
}
