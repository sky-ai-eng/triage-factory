package server

import (
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/zalando/go-keyring"
)

// This file covers rules the SPA enforces before it calls, pinned on the
// handler so a headless caller — an API token, an agent, a script — meets the
// same gate. Each test names the frontend check it mirrors.

// TestOrgSettingsPost_BaseURLValidated mirrors the wizard's isHttpUrl gate and
// normalizeBaseUrl step (frontend/src/lib/reachability.ts), which every UI path
// to these columns runs before saving. The handler used to persist whatever
// arrived, so an API caller could store a value that is not a URL at all — and
// the App/PAT host derivation reads the column, not the wizard.
func TestOrgSettingsPost_BaseURLValidated(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()

	for _, field := range []string{"github_base_url", "jira_base_url"} {
		t.Run(field, func(t *testing.T) {
			s := newTestServer(t)
			for _, bad := range []string{
				"github.example.com",              // no scheme
				"ftp://github.example.com",        // wrong scheme
				"https://",                        // no host
				"https://u:p@github.example.com",  // userinfo
				"https://github.example.com?x=1",  // query
				"https://github.example.com#frag", // fragment
				"   ",                             // whitespace only
			} {
				rec := doJSON(t, s, "POST", "/api/settings/org", map[string]any{field: bad})
				if rec.Code != http.StatusBadRequest {
					t.Errorf("POST %s=%q = %d, want 400; body=%s", field, bad, rec.Code, rec.Body.String())
					continue
				}
				assertFirstError(t, rec, httpx.ReasonInvalidField, field)
			}
		})
	}
}

// TestOrgSettingsPost_BaseURLCanonicalized mirrors normalizeBaseUrl: the UI
// trims whitespace and trailing slashes before saving because the column is
// read verbatim downstream. The handler now does it too, so the stored form
// doesn't depend on which client wrote it.
func TestOrgSettingsPost_BaseURLCanonicalized(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()
	s := newTestServer(t)

	postJSONResp(t, s, "/api/settings/org", map[string]any{
		"github_base_url": "  https://github.example.com/  ",
		"jira_base_url":   "https://jira.example.com/context/",
	})

	var gh, jira string
	if err := s.db.QueryRowContext(t.Context(),
		`SELECT COALESCE(github_base_url, ''), COALESCE(jira_base_url, '') FROM org_settings WHERE org_id = ?`,
		runmode.LocalDefaultOrgID).Scan(&gh, &jira); err != nil {
		t.Fatalf("read base urls: %v", err)
	}
	if gh != "https://github.example.com" {
		t.Errorf("github_base_url = %q, want the trimmed, slash-stripped form", gh)
	}
	if jira != "https://jira.example.com/context" {
		t.Errorf("jira_base_url = %q, want the context path kept and the trailing slash dropped", jira)
	}
}

// TestSnooze_PastWakeTimeRejected covers the RFC3339 arm of `until`, which the
// UI never sends — it offers four presets, all future by construction. A past
// wake time parks the task in a state the sweeper wakes on its next pass: a
// snooze that answers 200 and does nothing.
func TestSnooze_PastWakeTimeRejected(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for _, tc := range []struct {
		verb string
		body map[string]any
	}{
		{"snooze", map[string]any{"until": "2020-01-02T03:04:05Z"}},
		{"swipe", map[string]any{"action": "snooze", "until": "2020-01-02T03:04:05Z"}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			taskID := seedAdvanceTask(t, s.db, "past-"+tc.verb, advanceTaskOpts{})
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/"+tc.verb, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s with a past wake time = %d, want 400; body=%s", tc.verb, rec.Code, rec.Body.String())
			}
			assertFirstError(t, rec, httpx.ReasonOutOfRange, "until")
			status, until := readSnoozeState(t, s.db, taskID)
			if status != "queued" || !until.IsZero() {
				t.Fatalf("task = (%q, %v) after a refused snooze; want it untouched at (queued, NULL)", status, until)
			}
		})
	}
}

// TestSwipe_NegativeHesitationRejected pins the other half of the same pair.
// hesitation_ms is the milliseconds the user spent deciding, computed by the
// UI as a positive elapsed time; a negative one lands in swipe_events and skews
// dwell-time aggregates that nothing downstream re-validates.
func TestSwipe_NegativeHesitationRejected(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for _, tc := range []struct {
		verb string
		body map[string]any
	}{
		{"swipe", map[string]any{"action": "dismiss", "hesitation_ms": -1}},
		{"snooze", map[string]any{"until": "2h", "hesitation_ms": -1}},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			taskID := seedAdvanceTask(t, s.db, "hesitation-"+tc.verb, advanceTaskOpts{})
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/"+tc.verb, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s with hesitation_ms=-1 = %d, want 400; body=%s", tc.verb, rec.Code, rec.Body.String())
			}
			assertFirstError(t, rec, httpx.ReasonOutOfRange, "hesitation_ms")
		})
	}
}
