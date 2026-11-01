package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// TestMeSettings_RejectsUnknownField pins strict decoding on the PATCH: a
// caller that misspells a field learns it, rather than having the value
// silently dropped and reading a 200 as "saved".
func TestMeSettings_RejectsUnknownField(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{"theme": "dark"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with an unknown field = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, "UNKNOWN_FIELD", "theme")
}

// meSettingsOverviewSeenAt reads the marker off a settings response, and reports
// whether the key was present at all: a never-opened marker is the JSON literal
// null, which a client must be able to tell from an omitted key.
func meSettingsOverviewSeenAt(t *testing.T, rec *httptest.ResponseRecorder) (value string, present bool) {
	t.Helper()
	var body struct {
		UserSettings map[string]json.RawMessage `json:"user_settings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	raw, ok := body.UserSettings["overview_seen_at"]
	if !ok {
		return "", false
	}
	return string(raw), true
}

// TestMeSettings_OverviewSeenAt_SetKeepClear walks the marker from never written
// to set, kept by bodies that do not name it, and cleared back to null, with each
// PATCH answering what the following GET does.
func TestMeSettings_OverviewSeenAt_SetKeepClear(t *testing.T) {
	s := newTestServer(t)

	initial := doJSON(t, s, http.MethodGet, "/api/me/settings", nil)
	if initial.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", initial.Code, initial.Body.String())
	}
	value, present := meSettingsOverviewSeenAt(t, initial)
	if !present {
		t.Fatalf("GET omits overview_seen_at entirely; body=%s", initial.Body.String())
	}
	// A user's GitHub / Jira identity belongs to the identity reads, which key
	// it by host; a copy here would drift when the org's host changes.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(initial.Body.Bytes(), &top); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"github_username", "jira_account_id"} {
		if _, ok := top[field]; ok {
			t.Errorf("response carries %q; it belongs to the identity reads", field)
		}
	}
	if value != "null" {
		t.Errorf("overview_seen_at before any write = %s, want null (never opened)", value)
	}

	seen := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	set := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{
		"user_settings": map[string]any{"overview_seen_at": seen.Format(time.RFC3339)},
	})
	if set.Code != http.StatusOK {
		t.Fatalf("PATCH set = %d, want 200; body=%s", set.Code, set.Body.String())
	}
	stored, _ := meSettingsOverviewSeenAt(t, set)

	// The write answers what a read would.
	afterSet := doJSON(t, s, http.MethodGet, "/api/me/settings", nil)
	if afterSet.Body.String() != set.Body.String() {
		t.Errorf("PATCH body = %s, want the follow-up GET's %s", set.Body.String(), afterSet.Body.String())
	}
	var got time.Time
	if err := json.Unmarshal([]byte(stored), &got); err != nil {
		t.Fatalf("stored overview_seen_at %s is not a JSON timestamp: %v", stored, err)
	}
	if !got.Equal(seen) {
		t.Errorf("stored overview_seen_at = %v, want %v", got, seen)
	}

	// A body that names user_settings but not this field keeps it. The row is
	// still written, so this breaks if the store writes the request rather than
	// the request applied onto the stored row.
	keep := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{"user_settings": map[string]any{}})
	if keep.Code != http.StatusOK {
		t.Fatalf("PATCH keep = %d, want 200; body=%s", keep.Code, keep.Body.String())
	}
	if kept, _ := meSettingsOverviewSeenAt(t, keep); kept != stored {
		t.Errorf("overview_seen_at after a body that doesn't name it = %s, want the stored %s", kept, stored)
	}

	// A body that names no settings object at all leaves the row alone, and
	// still answers the resource.
	untouched := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{})
	if got, _ := meSettingsOverviewSeenAt(t, untouched); got != stored {
		t.Errorf("overview_seen_at after an empty PATCH = %s, want the stored %s", got, stored)
	}
	if after := doJSON(t, s, http.MethodGet, "/api/me/settings", nil); after.Body.String() != untouched.Body.String() {
		t.Errorf("empty PATCH body = %s, want the follow-up GET's %s", untouched.Body.String(), after.Body.String())
	}

	cleared := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{
		"user_settings": map[string]any{"overview_seen_at": nil},
	})
	if cleared.Code != http.StatusOK {
		t.Fatalf("PATCH clear = %d, want 200; body=%s", cleared.Code, cleared.Body.String())
	}
	value, present = meSettingsOverviewSeenAt(t, cleared)
	if !present || value != "null" {
		t.Errorf("overview_seen_at after an explicit null = %s (present=%t), want null", value, present)
	}
	if afterClear := doJSON(t, s, http.MethodGet, "/api/me/settings", nil); afterClear.Body.String() != cleared.Body.String() {
		t.Errorf("PATCH clear body = %s, want the follow-up GET's %s", cleared.Body.String(), afterClear.Body.String())
	}
}

// TestMeSettings_OverviewSeenAt_RejectsMalformedValue pins the shape fault:
// the marker is an RFC3339 instant, and anything else is a 400 naming the
// field rather than a value parsed loosely into some other moment.
func TestMeSettings_OverviewSeenAt_RejectsMalformedValue(t *testing.T) {
	s := newTestServer(t)

	for _, value := range []any{"yesterday", "2026-08-29", 1756500000, true} {
		rec := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{
			"user_settings": map[string]any{"overview_seen_at": value},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH overview_seen_at=%v = %d, want 400; body=%s", value, rec.Code, rec.Body.String())
			continue
		}
		httpxtest.AssertFirstError(t, rec, "INVALID_FIELD", "user_settings.overview_seen_at")
	}

	// The value never reached the row.
	if got, _ := meSettingsOverviewSeenAt(t, doJSON(t, s, http.MethodGet, "/api/me/settings", nil)); got != "null" {
		t.Errorf("overview_seen_at after refused writes = %s, want null", got)
	}
}

// TestMeSettings_RejectsNonObjectSettings: user_settings must be an object.
// null is refused rather than read as "absent" (a 200 that wrote nothing) or as
// "reset everything" (a reset that would widen with every pref added).
func TestMeSettings_RejectsNonObjectSettings(t *testing.T) {
	s := newTestServer(t)

	// Seed a marker, so a refused body that was silently treated as "absent"
	// would be invisible in the row and only the status code would show it.
	seen := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if rec := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{
		"user_settings": map[string]any{"overview_seen_at": seen.Format(time.RFC3339)},
	}); rec.Code != http.StatusOK {
		t.Fatalf("seeding PATCH = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	for _, body := range []string{
		`{"user_settings": null}`,
		`{"user_settings": 5}`,
		`{"user_settings": "overview"}`,
		`{"user_settings": []}`,
		`{"user_settings": true}`,
	} {
		req := httptest.NewRequest(http.MethodPatch, "/api/me/settings", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s = %d, want 400; body=%s", body, rec.Code, rec.Body.String())
			continue
		}
		httpxtest.AssertFirstError(t, rec, "INVALID_FIELD", "user_settings")
	}

	// And the stored marker is untouched by any of them.
	got, _ := meSettingsOverviewSeenAt(t, doJSON(t, s, http.MethodGet, "/api/me/settings", nil))
	var stored time.Time
	if err := json.Unmarshal([]byte(got), &stored); err != nil {
		t.Fatalf("overview_seen_at %s is not a timestamp after the refused bodies: %v", got, err)
	}
	if !stored.Equal(seen) {
		t.Errorf("overview_seen_at = %v after refused bodies, want the seeded %v", stored, seen)
	}
}

// TestMeSettings_OverviewSeenAt_RejectsUnknownNestedField pins that strict
// decoding reaches inside the settings object too — a misspelled per-user pref
// is a named 400, not a value silently dropped behind a 200.
func TestMeSettings_OverviewSeenAt_RejectsUnknownNestedField(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{
		"user_settings": map[string]any{"overview_seen": "2026-08-29T18:40:00Z"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with a misspelled nested field = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, "UNKNOWN_FIELD", "overview_seen")
}

// TestMeSettings_OverviewSeenAt_RejectsFutureValue: a marker beyond the clock
// skew window is a 422, since nothing would walk a future marker back; one
// within the window is accepted.
func TestMeSettings_OverviewSeenAt_RejectsFutureValue(t *testing.T) {
	s := newTestServer(t)

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	rec := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{
		"user_settings": map[string]any{"overview_seen_at": future},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH with a future marker = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, "OUT_OF_RANGE", "user_settings.overview_seen_at")
	if got, _ := meSettingsOverviewSeenAt(t, doJSON(t, s, http.MethodGet, "/api/me/settings", nil)); got != "null" {
		t.Errorf("overview_seen_at after a refused future write = %s, want null", got)
	}

	// Inside the skew window a fast clock still saves: the client's now is the
	// only clock that knows a human looked, and refusing ordinary drift would
	// make the marker fail for a machine that is merely slightly ahead.
	skewed := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	ok := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{
		"user_settings": map[string]any{"overview_seen_at": skewed.Format(time.RFC3339)},
	})
	if ok.Code != http.StatusOK {
		t.Fatalf("PATCH within clock skew = %d, want 200; body=%s", ok.Code, ok.Body.String())
	}
}
