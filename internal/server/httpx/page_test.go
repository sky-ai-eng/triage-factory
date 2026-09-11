package httpx

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// resolveFaults runs ResolvePage and reports the error items it accumulated,
// so each case reads as "this request, these faults".
func resolveFaults(t *testing.T, req PageRequest, fingerprint string, max int) (Page, []ErrorItem) {
	t.Helper()
	var v Validation
	page := ResolvePage(&v, req, fingerprint, max)
	return page, v.items
}

// psize spells an explicit page_size the way a decoded body carries one.
func psize(n int) *int { return &n }

// TestResolvePage_Defaults pins the absent-means-default half of the
// contract: an empty paging block is the first page at the default size.
func TestResolvePage_Defaults(t *testing.T) {
	page, faults := resolveFaults(t, PageRequest{}, "fp", 0)
	if len(faults) != 0 {
		t.Fatalf("faults on an empty paging block: %+v", faults)
	}
	if page.Limit != DefaultPageSize || page.Offset != 0 {
		t.Errorf("page = %+v, want limit %d / offset 0", page, DefaultPageSize)
	}
}

// TestResolvePage_SizeIsValidatedNotClamped is the reason page_size is
// checked at all: a caller that asks for 500 rows and silently gets 200 reads
// the short page as the whole answer.
func TestResolvePage_SizeIsValidatedNotClamped(t *testing.T) {
	cases := []struct {
		name      string
		size      int
		max       int
		wantFault bool
		wantLimit int
	}{
		{"in range", 10, 0, false, 10},
		{"at the max", MaxPageSize, 0, false, MaxPageSize},
		{"over the max", MaxPageSize + 1, 0, true, DefaultPageSize},
		{"negative", -1, 0, true, DefaultPageSize},
		{"over a route's own lower max", 100, 25, true, DefaultPageSize},
		{"within a route's own lower max", 25, 25, false, 25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, faults := resolveFaults(t, PageRequest{PageSize: psize(tc.size)}, "fp", tc.max)
			if tc.wantFault {
				if len(faults) != 1 || faults[0].Reason != ReasonOutOfRange || faults[0].Field != "page_size" {
					t.Fatalf("faults = %+v, want one OUT_OF_RANGE on page_size", faults)
				}
				return
			}
			if len(faults) != 0 {
				t.Fatalf("unexpected faults: %+v", faults)
			}
			if page.Limit != tc.wantLimit {
				t.Errorf("limit = %d, want %d", page.Limit, tc.wantLimit)
			}
		})
	}
}

// TestResolvePage_TokenRoundTrip walks the token the way a client does: the
// token WriteList minted for one page resolves to the next page's offset.
func TestResolvePage_TokenRoundTrip(t *testing.T) {
	page, _ := resolveFaults(t, PageRequest{PageSize: psize(2)}, "fp", 0)
	rec := httptest.NewRecorder()
	WriteList(rec, page, []string{"a", "b"}, 5)

	var body struct {
		Items         []string `json:"items"`
		NextPageToken string   `json:"next_page_token"`
		TotalCount    int      `json:"total_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	if body.TotalCount != 5 || len(body.Items) != 2 {
		t.Fatalf("envelope = %+v, want 2 items of 5", body)
	}
	if body.NextPageToken == "" {
		t.Fatal("no next_page_token minted for a partial page")
	}

	next, faults := resolveFaults(t, PageRequest{PageSize: psize(2), PageToken: body.NextPageToken}, "fp", 0)
	if len(faults) != 0 {
		t.Fatalf("round-tripped token rejected: %+v", faults)
	}
	if next.Offset != 2 {
		t.Errorf("offset = %d, want 2", next.Offset)
	}
}

// TestWriteList_LastPageAndEmpty pins the two ends: the page that exhausts
// the result set carries no token, and an empty page serializes items as []
// rather than null.
func TestWriteList_LastPageAndEmpty(t *testing.T) {
	page, _ := resolveFaults(t, PageRequest{PageSize: psize(3)}, "fp", 0)

	rec := httptest.NewRecorder()
	WriteList(rec, page, []string{"a", "b", "c"}, 3)
	if got := rec.Body.String(); strings.Contains(got, "next_page_token") {
		t.Errorf("last page carries a token: %s", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	WriteList[string](rec, page, nil, 0)
	if got := rec.Body.String(); !strings.Contains(got, `"items":[]`) {
		t.Errorf("empty page = %s, want items:[]", got)
	}
}

// TestResolvePage_RejectsUnusableTokens covers both ways a token can be
// wrong: unreadable, and readable but minted for another query. The second is
// the one that matters — without it, page 2 of one filter set silently
// addresses another's rows.
func TestResolvePage_RejectsUnusableTokens(t *testing.T) {
	valid := encodePageToken(pageToken{O: 40, F: "fp"})
	negative := base64.RawURLEncoding.EncodeToString([]byte(`{"o":-5,"f":"fp"}`))
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("not json"))

	cases := []struct {
		name  string
		token string
	}{
		{"not base64", "not-a-token!"},
		{"not JSON", notJSON},
		{"negative offset", negative},
		{"minted for another filter set", encodePageToken(pageToken{O: 40, F: "other"})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, faults := resolveFaults(t, PageRequest{PageToken: tc.token}, "fp", 0)
			if len(faults) != 1 || faults[0].Reason != ReasonInvalidParam || faults[0].Field != "page_token" {
				t.Fatalf("faults = %+v, want one INVALID_PARAM on page_token", faults)
			}
		})
	}

	if page, faults := resolveFaults(t, PageRequest{PageToken: valid}, "fp", 0); len(faults) != 0 || page.Offset != 40 {
		t.Errorf("valid token: page = %+v, faults = %+v", page, faults)
	}
}

// TestResolvePage_KeysetTokenRoundTrip walks the keyset form the way a client
// does: the token WriteListKeyset minted for one page resolves to the position
// the next page resumes after, and nothing about the wire shape says which
// form it is.
func TestResolvePage_KeysetTokenRoundTrip(t *testing.T) {
	page, _ := resolveFaults(t, PageRequest{PageSize: psize(2)}, "fp", 0)
	rec := httptest.NewRecorder()
	WriteListKeyset(rec, page, []string{"a", "b"}, 5, []string{"0", "2026-09-10T12:00:00Z", "b"})

	var body struct {
		Items         []string `json:"items"`
		NextPageToken string   `json:"next_page_token"`
		TotalCount    int      `json:"total_count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	if body.TotalCount != 5 || len(body.Items) != 2 {
		t.Fatalf("envelope = %+v, want 2 items of 5", body)
	}
	if body.NextPageToken == "" {
		t.Fatal("no next_page_token minted for a page with a next key")
	}

	next, faults := resolveFaults(t, PageRequest{PageSize: psize(2), PageToken: body.NextPageToken}, "fp", 0)
	if len(faults) != 0 {
		t.Fatalf("round-tripped keyset token rejected: %+v", faults)
	}
	if want := []string{"0", "2026-09-10T12:00:00Z", "b"}; !slices.Equal(next.After, want) {
		t.Errorf("after = %v, want %v", next.After, want)
	}
	if next.Offset != 0 {
		t.Errorf("offset = %d, want 0 — a keyset token carries no offset", next.Offset)
	}
}

// TestWriteListKeyset_LastPageHasNoToken pins the one signal a keyset page has
// for "there is no page after this": a nil key. It cannot be derived from the
// total the way an offset page derives it, which is the whole reason the
// caller passes it.
func TestWriteListKeyset_LastPageHasNoToken(t *testing.T) {
	page, _ := resolveFaults(t, PageRequest{PageSize: psize(3)}, "fp", 0)

	// A short page of a much larger total still ends the walk when the caller
	// says so: total is the filtered total, not a position.
	rec := httptest.NewRecorder()
	WriteListKeyset(rec, page, []string{"a", "b", "c"}, 900, nil)
	if got := rec.Body.String(); strings.Contains(got, "next_page_token") {
		t.Errorf("last page carries a token: %s", got)
	}

	// And a key with no rows to anchor it mints nothing either.
	rec = httptest.NewRecorder()
	WriteListKeyset[string](rec, page, nil, 0, []string{"stale"})
	if got := rec.Body.String(); strings.Contains(got, "next_page_token") {
		t.Errorf("empty page carries a token: %s", got)
	}
}

// TestResolvePage_RejectsMultiPositionTokens pins the exclusivity of the three
// positions a token can carry. A token holding two was not minted here, and
// honoring either one would page from a position nothing produced.
func TestResolvePage_RejectsMultiPositionTokens(t *testing.T) {
	cases := map[string]pageToken{
		"offset and keyset": {O: 20, F: "fp", K: []string{"a"}},
		"cursor and keyset": {F: "fp", C: "upstream", K: []string{"a"}},
		"offset and cursor": {O: 20, F: "fp", C: "upstream"},
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			_, faults := resolveFaults(t, PageRequest{PageToken: encodePageToken(tok)}, "fp", 0)
			if len(faults) != 1 || faults[0].Reason != ReasonInvalidParam || faults[0].Field != "page_token" {
				t.Fatalf("faults = %+v, want one INVALID_PARAM on page_token", faults)
			}
		})
	}

	// The fingerprint still gates a keyset token the same way it gates an
	// offset one — the position form is not what a token is bound by.
	_, faults := resolveFaults(t, PageRequest{PageToken: encodePageToken(pageToken{F: "other", K: []string{"a"}})}, "fp", 0)
	if len(faults) != 1 || faults[0].Reason != ReasonInvalidParam {
		t.Fatalf("faults = %+v, want one INVALID_PARAM for a keyset token minted elsewhere", faults)
	}
}

// TestResolveKeysetPage_RefusesEveryOtherForm pins the pairing: a route that
// mints keyset tokens accepts keyset tokens, so there is no input that makes
// it page the way it no longer pages. The offset case is the one that matters
// — it would otherwise page perfectly well, and wrongly.
func TestResolveKeysetPage_RefusesEveryOtherForm(t *testing.T) {
	cases := map[string]pageToken{
		"an offset from before the conversion": {O: 40, F: "fp"},
		"a proxy cursor":                       {F: "fp", C: "upstream"},
		"a token carrying no position at all":  {F: "fp"},
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			var v Validation
			ResolveKeysetPage(&v, PageRequest{PageToken: encodePageToken(tok)}, "fp", 0)
			if len(v.items) != 1 || v.items[0].Reason != ReasonInvalidParam || v.items[0].Field != "page_token" {
				t.Fatalf("faults = %+v, want one INVALID_PARAM on page_token", v.items)
			}
		})
	}

	// Its own form passes, and so does no token at all — the first page needs
	// none, which is what a refused caller restarts with.
	var v Validation
	page := ResolveKeysetPage(&v, PageRequest{PageToken: encodePageToken(pageToken{F: "fp", K: []string{"a", "b"}})}, "fp", 0)
	if len(v.items) != 0 || !slices.Equal(page.After, []string{"a", "b"}) {
		t.Errorf("keyset token: page = %+v, faults = %+v", page, v.items)
	}
	v = Validation{}
	if page := ResolveKeysetPage(&v, PageRequest{}, "fp", 0); len(v.items) != 0 || page.Limit != DefaultPageSize {
		t.Errorf("first page: page = %+v, faults = %+v", page, v.items)
	}

	// The offset door still takes what it always took, so converting one route
	// leaves every other list alone.
	v = Validation{}
	if page := ResolvePage(&v, PageRequest{PageToken: encodePageToken(pageToken{O: 40, F: "fp"})}, "fp", 0); len(v.items) != 0 || page.Offset != 40 {
		t.Errorf("offset door: page = %+v, faults = %+v", page, v.items)
	}
}

// TestFilterFingerprint_DistinguishesFilterSets pins what the token binding
// rests on: equal filters fingerprint equally, different ones don't.
func TestFilterFingerprint_DistinguishesFilterSets(t *testing.T) {
	type filters struct {
		Statuses []string `json:"statuses"`
		Only     bool     `json:"only"`
	}
	a := FilterFingerprint(filters{Statuses: []string{"queued"}, Only: true})
	same := FilterFingerprint(filters{Statuses: []string{"queued"}, Only: true})
	if a != same {
		t.Errorf("equal filter sets fingerprinted differently: %q vs %q", a, same)
	}
	for _, other := range []filters{
		{Statuses: []string{"queued"}},
		{Statuses: []string{"done"}, Only: true},
		{Statuses: nil, Only: true},
	} {
		if got := FilterFingerprint(other); got == a {
			t.Errorf("%+v fingerprinted the same as the reference set (%q)", other, got)
		}
	}
}

// TestResolvePage_CountOnly pins the stat()-vs-read() half of the paging
// contract: an explicit page_size of 0 is a count request, distinguishable
// from an absent field only because PageSize decodes through a pointer.
func TestResolvePage_CountOnly(t *testing.T) {
	page, faults := resolveFaults(t, PageRequest{PageSize: psize(0)}, "fp", 0)
	if len(faults) != 0 {
		t.Fatalf("explicit zero faulted: %+v", faults)
	}
	if !page.CountOnly || page.Limit != 0 {
		t.Fatalf("page = %+v, want CountOnly with limit 0", page)
	}

	// The absent field stays the default window — the pointer is what keeps
	// these two requests from collapsing into one.
	page, _ = resolveFaults(t, PageRequest{}, "fp", 0)
	if page.CountOnly || page.Limit != DefaultPageSize {
		t.Fatalf("absent page_size = %+v, want default window without CountOnly", page)
	}
}

// TestWriteList_CountOnlyEnvelope: a count-only page is the ordinary envelope
// with no items — same shape, no token (nothing to resume), real total.
func TestWriteList_CountOnlyEnvelope(t *testing.T) {
	page, _ := resolveFaults(t, PageRequest{PageSize: psize(0)}, "fp", 0)
	rec := httptest.NewRecorder()
	WriteList[string](rec, page, nil, 42)
	if got := rec.Body.String(); !strings.Contains(got, `"items":[]`) || !strings.Contains(got, `"total_count":42`) {
		t.Errorf("count-only envelope = %s, want empty items with total 42", got)
	}
	if got := rec.Body.String(); strings.Contains(got, "next_page_token") {
		t.Errorf("count-only envelope minted a token: %s", got)
	}
}
