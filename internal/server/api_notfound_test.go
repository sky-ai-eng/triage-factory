package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestUnknownAPIPathReturnsJSON404 is the TFAC-409 item-5 contract: an
// unregistered /api/* path returns a JSON 404 rather than falling through to
// the greedy "/" SPA fallback (which would answer 200 + index.html and mask the
// typo). A registered route must still win. A known path called with an
// unregistered method also lands on the /api/ catch-all as a JSON 404 — note
// the pre-existing "/" catch-all already prevented a true 405 here (it matches
// every method), so the only change is the previous 200-SPA-HTML becoming a
// cleaner 404-JSON, never a regression of a working 405.
func TestUnknownAPIPathReturnsJSON404(t *testing.T) {
	s := newTestServer(t)

	t.Run("unknown api path is json 404", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodGet, "/api/does-not-exist", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json (SPA HTML would be text/html)", ct)
		}
		if body := rec.Body.String(); !strings.Contains(body, "not found") || strings.Contains(body, "<html") {
			t.Errorf("body = %q, want a JSON not-found error, not SPA HTML", body)
		}
	})

	t.Run("unknown nested api path is json 404", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/does-not-exist/bogus", nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("registered route still wins", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodGet, "/api/health", nil)
		if rec.Code != http.StatusOK {
			t.Errorf("/api/health status = %d, want 200 — catch-all must not shadow real routes", rec.Code)
		}
	})

	t.Run("wrong method on known route is json 404 not spa html", func(t *testing.T) {
		// /api/health is GET-only. A DELETE matches neither "GET /api/health"
		// (method) nor any other concrete route, so it lands on the /api/
		// catch-all: a JSON 404, not the old 200 + index.html.
		rec := doJSON(t, s, http.MethodDelete, "/api/health", nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("DELETE /api/health status = %d, want 404", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
	})
}
