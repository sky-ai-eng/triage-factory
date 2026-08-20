package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// assertFirstError decodes the error envelope off rec and asserts the first
// item's reason and field.
func assertFirstError(t *testing.T, rec *httptest.ResponseRecorder, wantReason, wantField string) {
	t.Helper()
	var body struct {
		Errors []struct {
			Reason string `json:"reason"`
			Field  string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("empty errors list; body=%s", rec.Body.String())
	}
	if body.Errors[0].Reason != wantReason || body.Errors[0].Field != wantField {
		t.Errorf("first error = %+v, want {%s, field %q}", body.Errors[0], wantReason, wantField)
	}
}

// errorFields decodes the error envelope off rec and returns every item's
// field — for the accumulating validators, where the contract is that a body
// with three bad fields reports three rather than only the first one hit.
func errorFields(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var body struct {
		Errors []struct {
			Field string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("empty errors list; body=%s", rec.Body.String())
	}
	out := make([]string, 0, len(body.Errors))
	for _, e := range body.Errors {
		out = append(out, e.Field)
	}
	return out
}

// containsString is the membership check errorFields' callers want; the
// accumulator's ORDER is not part of the contract, only its contents.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// firstErrorMessage decodes the error envelope off rec and returns the first
// item's message — the prose half of the contract, for tests that assert on
// what the response says rather than on its reason code.
func firstErrorMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("empty errors list; body=%s", rec.Body.String())
	}
	return body.Errors[0].Message
}

// decodeErrorItems returns every item in the error envelope. Its reason for
// existing beside firstErrorMessage is the accumulating validators: a body
// with several bad fields answers with several items, and asserting only the
// first would pass a handler that stopped at it.
func decodeErrorItems(t *testing.T, rec *httptest.ResponseRecorder) []httpx.ErrorItem {
	t.Helper()
	var body struct {
		Errors []httpx.ErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v (body=%s)", err, rec.Body.String())
	}
	if len(body.Errors) == 0 {
		t.Fatalf("empty errors list; body=%s", rec.Body.String())
	}
	return body.Errors
}

// firstErrorReason is firstErrorMessage's sibling for the machine code.
func firstErrorReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return decodeErrorItems(t, rec)[0].Reason
}

// TestTaskRoutes_404OnMalformedID pins the family-wide contract: every
// /api/tasks/{id} route validates the path id as a UUID before any store
// call and answers 404 for a malformed one. Production ids are UUIDs in both
// dialects; on Postgres tasks.id is the uuid column type, so without the
// guard a malformed id surfaces as SQLSTATE 22P02 → 500 (the store-level
// reality is pinned by TestTaskStore_Postgres_MalformedIDErrors). Treating
// it as not-found keeps the answer identical across dialects and discloses
// nothing.
func TestTaskRoutes_404OnMalformedID(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
		body   map[string]any
	}{
		{"get", http.MethodGet, "/api/tasks/not-a-uuid", nil},
		{"patch", http.MethodPatch, "/api/tasks/not-a-uuid", map[string]any{"status": "dismissed"}},
		{"claim", http.MethodPost, "/api/tasks/not-a-uuid/claim", map[string]any{}},
		{"delegate", http.MethodPost, "/api/tasks/not-a-uuid/delegate", map[string]any{}},
		{"undo", http.MethodPost, "/api/tasks/not-a-uuid/undo", nil},
		{"requeue", http.MethodPost, "/api/tasks/not-a-uuid/requeue", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := doJSON(t, s, tc.method, tc.path, tc.body)
			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestEntityDeckReads_StrictQueryParams is the negative space of the
// strict-params contract on the query-param-carrying reads next to the tasks
// family: a filter that fails to parse is rejected, never resolved to a
// default — a corrupt team filter must not widen a result set back to the
// union, and must not silently retarget a single-team deck to the caller's
// default team. The tasks list's own filters travel in a body now; their
// negative space is TestTaskList_StrictBody.
func TestEntityDeckReads_StrictQueryParams(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"malformed team filter on factory", "/api/factory/snapshot?team_id=junk"},
		{"malformed team filter on stock", "/api/jira/stock?team_id=junk"},
		// Present-but-empty and repeated values on the single-team deck are
		// rejected too: treating either as absent (or taking the first) would
		// silently retarget the read to the caller's default team.
		{"empty team filter on stock", "/api/jira/stock?team_id="},
		{"repeated team filter on stock", "/api/jira/stock?team_id=00000000-0000-4000-8000-000000000001&team_id=00000000-0000-4000-8000-000000000002"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := doJSON(t, s, http.MethodGet, tc.path, nil)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestTaskWrites_StrictBodies pins strict decoding on the family's write
// bodies: an unknown field is a 400 naming the field, on every route that
// takes one. The former `action` and `to` discriminators are among the
// unknown fields now, so a stale client's body is rejected rather than
// half-understood.
func TestTaskWrites_StrictBodies(t *testing.T) {
	const taskID = "/api/tasks/00000000-0000-4000-8000-0000000000aa"
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   map[string]any
		field  string
	}{
		{"patch", http.MethodPatch, taskID, map[string]any{"status": "done", "bogus": 1}, "bogus"},
		{"patch_stale_advance_body", http.MethodPatch, taskID, map[string]any{"to": "in_progress"}, "to"},
		{"claim", http.MethodPost, taskID + "/claim", map[string]any{"bogus": 1}, "bogus"},
		{"claim_stale_swipe_body", http.MethodPost, taskID + "/claim", map[string]any{"action": "claim"}, "action"},
		{"delegate", http.MethodPost, taskID + "/delegate", map[string]any{"bogus": 1}, "bogus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := doJSON(t, s, tc.method, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			assertFirstError(t, rec, "UNKNOWN_FIELD", tc.field)
		})
	}
}
