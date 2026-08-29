package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestMeSettings_PatchAnswersTheResource pins the two things the settings
// route owes a caller: a PATCH answers the settings resource — the same shape
// the GET returns — rather than a status word, and it answers it even when the
// body describes no change, so a client's post-save state is exact either way.
func TestMeSettings_PatchAnswersTheResource(t *testing.T) {
	s := newTestServer(t)

	get := doJSON(t, s, http.MethodGet, "/api/me/settings", nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET /api/me/settings = %d, want 200; body=%s", get.Code, get.Body.String())
	}

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"no_change", map[string]any{}},
		{"writes_settings", map[string]any{"user_settings": map[string]any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPatch, "/api/me/settings", tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("PATCH = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var resp map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, ok := resp["status"]; ok {
				t.Errorf("response carries a status word: %s", rec.Body.String())
			}
			if _, ok := resp["user_settings"]; !ok {
				t.Errorf("response is missing user_settings: %s", rec.Body.String())
			}
			if rec.Body.String() != get.Body.String() {
				t.Errorf("PATCH body = %s, want the GET's %s (a write answers what a read would)", rec.Body.String(), get.Body.String())
			}
		})
	}
}

// TestMeSettings_OmitsIdentityFields pins the split: a user's GitHub / Jira
// identity is what the identity reads answer, host and all. Carrying a copy
// here would be one fact in two namespaces, drifting the moment the org's host
// changes under one of them.
func TestMeSettings_OmitsIdentityFields(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodGet, "/api/me/settings", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"github_username", "jira_account_id"} {
		if _, ok := resp[field]; ok {
			t.Errorf("response carries %q; it belongs to the identity reads", field)
		}
	}
}

// TestMeSettings_RejectsUnknownField pins strict decoding on the PATCH: a
// caller that misspells a field learns it, rather than having the value
// silently dropped and reading a 200 as "saved".
func TestMeSettings_RejectsUnknownField(t *testing.T) {
	s := newTestServer(t)

	rec := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{"theme": "dark"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with an unknown field = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, "UNKNOWN_FIELD", "theme")
}

// meSettingsOverviewSeenAt reads the marker off a settings response, telling
// "the route answered null" (the JSON literal, which is what never-opened
// looks like on the wire) from "the route omitted the key" — a field that can
// legitimately be null must still always be present, or a client cannot tell
// an answer from a version that does not know the question.
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

// TestMeSettings_OverviewSeenAt_SetKeepClear walks the marker through the
// three things a PATCH can say about a field, in the order a real client hits
// them: never written, written, left alone by a body that does not name it,
// and cleared back to never-written.
//
// The last one is the reason the field is read through json.RawMessage rather
// than a *time.Time: "the Overview has never been opened" is a state the page
// renders as its own sentence, so a client has to be able to ask for it.
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

	// The write answers what a read would — the acceptance the whole route is
	// shaped around, and the reason the PATCH hands back the row its own write
	// returned rather than composing an answer of its own.
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

	// A body that names user_settings but not this field keeps it. The write
	// still happens (the row is touched), so this is the case that would break
	// if the store's end-state write were handed the request rather than the
	// stored row applied onto.
	keep := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{"user_settings": map[string]any{}})
	if keep.Code != http.StatusOK {
		t.Fatalf("PATCH keep = %d, want 200; body=%s", keep.Code, keep.Body.String())
	}
	if kept, _ := meSettingsOverviewSeenAt(t, keep); kept != stored {
		t.Errorf("overview_seen_at after a body that doesn't name it = %s, want the stored %s", kept, stored)
	}

	// And a body that names no settings object at all leaves the row alone.
	untouched := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{})
	if got, _ := meSettingsOverviewSeenAt(t, untouched); got != stored {
		t.Errorf("overview_seen_at after an empty PATCH = %s, want the stored %s", got, stored)
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
		assertFirstError(t, rec, "INVALID_FIELD", "user_settings.overview_seen_at")
	}

	// The value never reached the row.
	if got, _ := meSettingsOverviewSeenAt(t, doJSON(t, s, http.MethodGet, "/api/me/settings", nil)); got != "null" {
		t.Errorf("overview_seen_at after refused writes = %s, want null", got)
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
	assertFirstError(t, rec, "UNKNOWN_FIELD", "overview_seen")
}

// TestMeSettings_OverviewSeenAt_RejectsFutureValue pins the range fault. The
// timestamp is the client's own now, so a machine a few seconds fast is
// ordinary and accepted; a value beyond that is a broken clock, and storing it
// would invert the away line until real time caught up — nothing walks a
// future marker back, because the next visit writes an EARLIER timestamp.
func TestMeSettings_OverviewSeenAt_RejectsFutureValue(t *testing.T) {
	s := newTestServer(t)

	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	rec := doJSON(t, s, http.MethodPatch, "/api/me/settings", map[string]any{
		"user_settings": map[string]any{"overview_seen_at": future},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH with a future marker = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	assertFirstError(t, rec, "OUT_OF_RANGE", "user_settings.overview_seen_at")
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
