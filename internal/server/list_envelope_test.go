package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// listEnvelope is the wire shape every POST /…/list read answers with, as the
// tests read it. TotalCount is a pointer so a test can tell "the server
// counted zero" from "the server sent null" — the proxy-list contract turns on
// exactly that difference (see httpx.WriteProxyList).
type listEnvelope[T any] struct {
	Items         []T    `json:"items"`
	NextPageToken string `json:"next_page_token"`
	TotalCount    *int   `json:"total_count"`
}

// Total is the counted total, or -1 when the response sent null. Tests that
// only care about a counted list compare against it directly.
func (e listEnvelope[T]) Total() int {
	if e.TotalCount == nil {
		return -1
	}
	return *e.TotalCount
}

// decodeList asserts a 200 and decodes the list envelope. It is the one place
// the envelope is parsed in tests, so a route that answers a bare array or a
// bespoke `{"things": [...]}` shape fails here rather than passing with a
// zero-valued Items.
func decodeList[T any](t *testing.T, rec *httptest.ResponseRecorder) listEnvelope[T] {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out listEnvelope[T]
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode list envelope: %v (body=%s)", err, rec.Body.String())
	}
	if out.Items == nil {
		t.Fatalf("items is null; an empty page must serialize as [] (body=%s)", rec.Body.String())
	}
	return out
}
