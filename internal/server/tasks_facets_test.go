package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// facetsBody is the shape POST /api/tasks/facets answers with: named cuts,
// no paging keys, no total_count.
type facetsBody struct {
	EventTypes []struct {
		Value string `json:"value"`
		Count int    `json:"count"`
	} `json:"event_types"`
}

// postTaskFacets calls the route, asserts a 200, and decodes the response.
func postTaskFacets(t *testing.T, s *Server, body map[string]any) facetsBody {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/facets", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out facetsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode facets response: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

// facetCounts renders the response as value → count, asserting the values came
// back ascending — the chips are alphabetical, and a client that has to
// re-sort was told the wrong thing.
func facetCounts(t *testing.T, body facetsBody) map[string]int {
	t.Helper()
	out := map[string]int{}
	prev := ""
	for _, f := range body.EventTypes {
		if f.Value <= prev {
			t.Errorf("event_types are not ascending: %q follows %q", f.Value, prev)
		}
		prev = f.Value
		out[f.Value] = f.Count
	}
	return out
}

// TestTaskFacets_CountsTheLane is the route's happy path: one entry per event
// type present in the lane, counting the rows the lane's own list returns.
//
// The fixture set spans the two ways a row can be absent from a lane — a
// different status, and a snooze window the lane doesn't ask for — because
// those are what a chip count silently overreports if the facet renders its
// own predicate instead of the list's.
func TestTaskFacets_CountsTheLane(t *testing.T) {
	s := newTestServer(t)
	future := time.Now().UTC().Add(2 * time.Hour)
	past := time.Now().UTC().Add(-2 * time.Hour)

	for _, f := range []taskFixture{
		{name: "ci-one", status: "queued"},
		{name: "ci-two", status: "queued"},
		{name: "review", status: "queued", eventType: domain.EventGitHubPRReviewRequested},
		// Deferred into the future: out of the pickable lane, in when the
		// board's toggle asks for it.
		{name: "sleeping", status: "queued", snoozeUntil: &future},
		// Other lanes entirely.
		{name: "claimed", status: "queued", claimedUser: true},
		{name: "closed", status: "done", closedAt: &past},
	} {
		seedTaskFixture(t, s.db, f)
	}

	ci, review := domain.EventGitHubPRCICheckFailed, domain.EventGitHubPRReviewRequested
	got := facetCounts(t, postTaskFacets(t, s, queueProjectionBody()))
	want := map[string]int{ci: 2, review: 1}
	if len(got) != len(want) || got[ci] != want[ci] || got[review] != want[review] {
		t.Errorf("queue lane facet = %v, want %v", got, want)
	}

	// The same lane with the snooze window admitted: the sleeping row joins
	// its own type's count and nothing else moves.
	widened := queueProjectionBody()
	widened["include_snoozed"] = true
	if got := facetCounts(t, postTaskFacets(t, s, widened)); got[ci] != 3 || got[review] != 1 {
		t.Errorf("include_snoozed facet = %v, want %s:3 %s:1", got, ci, review)
	}

	// A lane the facet is not about contributes nothing to it: the closed row
	// is the only member of the done lane, and it is the whole of that lane's
	// answer.
	done := facetCounts(t, postTaskFacets(t, s, map[string]any{"statuses": []string{"done"}}))
	if len(done) != 1 || done[ci] != 1 {
		t.Errorf("done lane facet = %v, want just %s:1", done, ci)
	}

	// An empty lane is an empty cut, not a null one.
	empty := postTaskFacets(t, s, map[string]any{"statuses": []string{"in_review"}})
	if empty.EventTypes == nil || len(empty.EventTypes) != 0 {
		t.Errorf("empty lane event_types = %v, want []", empty.EventTypes)
	}
}

// TestTaskFacets_AgreesWithTheList is the invariant the shared WHERE renderer
// exists for: the counts above a column and the rows in it are one query.
func TestTaskFacets_AgreesWithTheList(t *testing.T) {
	s := newTestServer(t)
	future := time.Now().UTC().Add(2 * time.Hour)
	for _, f := range []taskFixture{
		{name: "agree-ci", status: "queued"},
		{name: "agree-review", status: "queued", eventType: domain.EventGitHubPRReviewRequested},
		{name: "agree-sleeping", status: "queued", snoozeUntil: &future},
		{name: "agree-claimed", status: "queued", claimedUser: true},
	} {
		seedTaskFixture(t, s.db, f)
	}

	lane := queueProjectionBody()
	sum := 0
	for _, f := range postTaskFacets(t, s, lane).EventTypes {
		sum += f.Count
	}
	// page_size 0 is the list's count-only read: the same filters, the total
	// under them, no rows.
	counting := queueProjectionBody()
	counting["page_size"] = 0
	if total := postTaskList(t, s, counting).TotalCount; sum != total {
		t.Errorf("facet counts sum to %d, want the lane's own total_count %d", sum, total)
	}
}

// TestTaskFacets_RefusesTheFieldsItDoesNotAnswerFor covers the route's one
// real asymmetry with the list. The facet is ABOUT the lane, so a caller
// narrowing it by the values it exists to offer — or asking it for a page —
// has misread what it answers, and a field ignored in silence is how that
// misreading ships as a chip row that quietly hides half the lane.
//
// Each is INVALID_FIELD naming itself rather than UNKNOWN_FIELD: these are
// real fields of the sibling list, so the fault is the meaning, not the spelling.
func TestTaskFacets_RefusesTheFieldsItDoesNotAnswerFor(t *testing.T) {
	for _, tc := range []struct {
		field string
		value any
	}{
		{"search", "ci"},
		{"event_types", []string{domain.EventGitHubPRCICheckFailed}},
		{"sort_key", "title"},
		{"sort_dir", "asc"},
		{"created_before", time.Now().UTC().Format(time.RFC3339)},
		{"page_size", 25},
		{"page_token", "whatever"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			s := newTestServer(t)
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/facets", map[string]any{tc.field: tc.value})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			assertFirstError(t, rec, "INVALID_FIELD", tc.field)
		})
	}

	// Emptiness is not an escape hatch: presence is the fault, so a field
	// sent with a value that would have narrowed nothing is still refused.
	t.Run("empty search is still a narrowing", func(t *testing.T) {
		s := newTestServer(t)
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/facets", map[string]any{"search": ""})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, "INVALID_FIELD", "search")
	})

	// A field of neither route is still a spelling fault.
	t.Run("unknown field", func(t *testing.T) {
		s := newTestServer(t)
		rec := doJSON(t, s, http.MethodPost, "/api/tasks/facets", map[string]any{"bogus": 1})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, "UNKNOWN_FIELD", "bogus")
	})
}

// TestTaskFacets_ValidatesTheLaneLikeTheList pins the shared half: the lane
// filters are one struct and one validator, so a value the list refuses the
// facet refuses identically, and every failing field is reported at once.
func TestTaskFacets_ValidatesTheLaneLikeTheList(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/tasks/facets", map[string]any{
		"statuses":     []string{"bogus"},
		"team_ids":     []string{"junk"},
		"sources":      []string{"carrier-pigeon"},
		"closed_since": "yesterday",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Errors []struct {
			Reason string `json:"reason"`
			Field  string `json:"field"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	seen := map[string]string{}
	for _, e := range body.Errors {
		seen[e.Field] = e.Reason
	}
	for _, field := range []string{"statuses", "team_ids", "sources", "closed_since"} {
		if seen[field] != "INVALID_FIELD" {
			t.Errorf("field %q reported as %q, want INVALID_FIELD (every failing field, not the first)", field, seen[field])
		}
	}
}
