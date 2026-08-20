package server

import (
	"encoding/json"
	"net/http"
	"testing"
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
