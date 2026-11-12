package server

import (
	"database/sql"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
	"github.com/zalando/go-keyring"
)

// This file covers rules the SPA enforces before it calls, pinned on the
// handler so a headless caller — an API token, an agent, a script — meets the
// same gate. Each test names the frontend check it mirrors.

// TestOrgSettingsPost_BaseURLValidated mirrors the wizard's isHttpUrl and
// normalizeBaseUrl steps (frontend/src/lib/reachability.ts): a value that is not
// an http(s) base URL is refused, and an accepted one is stored trimmed of
// whitespace and its trailing slash, since the column is read verbatim.
func TestOrgSettingsPost_BaseURLValidated(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	keyring.MockInit()

	for _, tc := range []struct{ field, kind, good, stored string }{
		{"github_base_url", "github", "  https://github.example.com/  ", "https://github.example.com"},
		// A Jira context path is kept; only its trailing slash is dropped.
		{"jira_base_url", "jira", "https://jira.example.com/context/", "https://jira.example.com/context"},
	} {
		field := tc.field
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
				rec := patchOrgSettings(t, s, map[string]any{field: bad})
				if rec.Code != http.StatusBadRequest {
					t.Errorf("POST %s=%q = %d, want 400; body=%s", field, bad, rec.Code, rec.Body.String())
					continue
				}
				httpxtest.AssertFirstError(t, rec, httpx.ReasonInvalidField, field)
			}

			patchOrgSettingsOK(t, s, map[string]any{field: tc.good})
			var stored string
			if err := s.db.QueryRowContext(t.Context(),
				`SELECT COALESCE((SELECT base_url FROM org_event_sources WHERE org_id = ? AND kind = ?), '')`,
				runmode.LocalDefaultOrgID, tc.kind).Scan(&stored); err != nil {
				t.Fatalf("read base url: %v", err)
			}
			if stored != tc.stored {
				t.Errorf("%s stored as %q, want %q", field, stored, tc.stored)
			}
		})
	}
}

// TestSnooze_PastWakeTimeRejected: a past wake time, which the UI's picker
// never sends, is a 422 OUT_OF_RANGE on snooze_until and leaves the task
// untouched, rather than a snooze the sweeper undoes on its next pass.
func TestSnooze_PastWakeTimeRejected(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	taskID := seedLifecycleTask(t, s.db, "past-wake", lifecycleTaskOpts{})
	rec := doJSON(t, s, http.MethodPatch, "/api/tasks/"+taskID,
		map[string]any{"snooze_until": "2020-01-02T03:04:05Z"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a past wake time = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonOutOfRange, "snooze_until")
	status, until := readSnoozeState(t, s.db, taskID)
	if status != "queued" || !until.IsZero() {
		t.Fatalf("task = (%q, %v) after a refused snooze; want it untouched at (queued, NULL)", status, until)
	}
}

// TestTaskGestures_RefuseHesitation: hesitation_ms is not a field of the
// PATCH, claim, or delegate body, so strict decoding refuses it as unknown on
// every arm, whatever its value, and nothing is written.
func TestTaskGestures_RefuseHesitation(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{"patch_dismiss", http.MethodPatch, "", map[string]any{"status": "dismissed", "hesitation_ms": 0}},
		{"patch_snooze", http.MethodPatch, "", map[string]any{"snooze_until": "2999-01-01T00:00:00Z", "hesitation_ms": 250}},
		{"claim", http.MethodPost, "/claim", map[string]any{"hesitation_ms": 250}},
		{"delegate", http.MethodPost, "/delegate", map[string]any{"hesitation_ms": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			taskID := seedLifecycleTask(t, s.db, "hesitation-"+tc.name, lifecycleTaskOpts{})
			rec := doJSON(t, s, tc.method, "/api/tasks/"+taskID+tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s with hesitation_ms = %d, want 400; body=%s", tc.name, rec.Code, rec.Body.String())
			}
			httpxtest.AssertFirstError(t, rec, httpx.ReasonUnknownField, "hesitation_ms")
			var status string
			var claimedBy sql.NullString
			if err := s.db.QueryRow(`SELECT status, COALESCE(claimed_by_user_id, claimed_by_agent_id) FROM tasks WHERE id = ?`, taskID).Scan(&status, &claimedBy); err != nil {
				t.Fatalf("read task: %v", err)
			}
			if status != "queued" || claimedBy.Valid {
				t.Errorf("task = (%s, claimed by %v) after a refused body, want it untouched (queued, unclaimed)", status, claimedBy)
			}
		})
	}
}
