package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// newTestServer spins up an in-memory SQLite with the full schema + events
// catalog seed, registers all HTTP routes, and returns the Server. Each test
// gets its own DB so there's no cross-contamination.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	database, err := sql.Open("sqlite3", ":memory:?_foreign_keys=on")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetMaxOpenConns(1)
	database.SetMaxIdleConns(1)
	t.Cleanup(func() { database.Close() })

	if err := db.Migrate(database); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SeedEventTypes(database); err != nil {
		t.Fatalf("seed events: %v", err)
	}
	return New(database)
}

// doJSON performs a JSON request against the server's mux and returns the
// response. Body may be nil.
func doJSON(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reqBody *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reqBody = bytes.NewReader(b)
	} else {
		reqBody = bytes.NewReader(nil)
	}

	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// TestTaskRuleCreate_NullPredicate_Accepted is a regression test for a
// suggested-but-wrong review finding. A client reading a seeded rule back
// from GET gets `"scope_predicate_json": null` for match-all rules, and
// naturally round-trips that shape on POST. Go's encoding/json silently
// decodes null into a non-pointer string as "", which ValidatePredicateJSON
// treats as match-all, which stores as nil and encodes back out as null.
// Lock this behavior in so a well-meaning future edit doesn't swap the
// POST field to *string (which would change the semantics — POST doesn't
// need the absent/explicit distinction the way PATCH does).
func TestTaskRuleCreate_NullPredicate_Accepted(t *testing.T) {
	s := newTestServer(t)

	body := map[string]any{
		"event_type":           "github:pr:new_commits",
		"scope_predicate_json": nil, // JSON null, match-all
		"name":                 "Null predicate round-trip",
		"default_priority":     0.5,
	}
	rec := doJSON(t, s, "POST", "/api/task-rules", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}

	var created domain.TaskRule
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.ScopePredicateJSON != nil {
		t.Errorf("expected nil predicate (match-all), got %q", *created.ScopePredicateJSON)
	}
	if created.EventType != "github:pr:new_commits" {
		t.Errorf("event_type mismatch: %s", created.EventType)
	}
	if created.Source != "user" {
		t.Errorf("expected source=user, got %s", created.Source)
	}
}

// TestTaskRuleCreate_AbsentPredicate_SameAsNull verifies that omitting the
// predicate field entirely behaves identically to explicit null — both
// yield a match-all rule. POST semantics don't need the pointer-field
// "absent vs null" distinction (a new rule has no prior predicate to
// leave unchanged); PATCH does use *string for exactly that distinction.
func TestTaskRuleCreate_AbsentPredicate_SameAsNull(t *testing.T) {
	s := newTestServer(t)

	body := map[string]any{
		"event_type":       "github:pr:new_commits",
		"name":             "No predicate field at all",
		"default_priority": 0.5,
	}
	rec := doJSON(t, s, "POST", "/api/task-rules", body)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created domain.TaskRule
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if created.ScopePredicateJSON != nil {
		t.Errorf("expected nil predicate, got %q", *created.ScopePredicateJSON)
	}
}

// --- PATCH predicate semantics -------------------------------------------
// PATCH uses json.RawMessage for scope_predicate_json to distinguish three
// cases a plain *string would collapse:
//
//   absent          → leave the predicate unchanged
//   explicit null   → clear the predicate to match-all
//   actual value    → validate + canonicalise + store
//
// These tests lock each semantic in place so a future "simplification" to
// *string doesn't silently re-introduce the no-op-on-null bug.

// helper: create a user rule with a specific predicate so PATCH has
// something to modify.
func seedUserRuleWithPredicate(t *testing.T, s *Server, predicate string) string {
	t.Helper()
	body := map[string]any{
		"event_type":           "github:pr:new_commits",
		"scope_predicate_json": predicate,
		"name":                 "Test rule",
		"default_priority":     0.5,
	}
	rec := doJSON(t, s, "POST", "/api/task-rules", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed rule failed: %d: %s", rec.Code, rec.Body.String())
	}
	var created domain.TaskRule
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return created.ID
}

// helper: fetch a single rule via the list endpoint.
func getRule(t *testing.T, s *Server, id string) domain.TaskRule {
	t.Helper()
	rec := doJSON(t, s, "GET", "/api/task-rules", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list failed: %d", rec.Code)
	}
	var rules []domain.TaskRule
	if err := json.Unmarshal(rec.Body.Bytes(), &rules); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, r := range rules {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("rule %s not found", id)
	return domain.TaskRule{}
}

func TestTaskRulePatch_PredicateAbsent_LeavesUnchanged(t *testing.T) {
	s := newTestServer(t)
	id := seedUserRuleWithPredicate(t, s, `{"author_is_self":true}`)

	// PATCH without the predicate field at all — should not touch it.
	rec := doJSON(t, s, "PATCH", "/api/task-rules/"+id, map[string]any{
		"default_priority": 0.9,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch failed: %d: %s", rec.Code, rec.Body.String())
	}

	got := getRule(t, s, id)
	if got.ScopePredicateJSON == nil || *got.ScopePredicateJSON != `{"author_is_self":true}` {
		t.Errorf("predicate changed unexpectedly: got %v", got.ScopePredicateJSON)
	}
	if got.DefaultPriority != 0.9 {
		t.Errorf("priority didn't update: %v", got.DefaultPriority)
	}
}

func TestTaskRulePatch_PredicateNull_ClearsToMatchAll(t *testing.T) {
	// This is the bug the review bot caught: *string would make null and
	// absent indistinguishable, so sending null would silently no-op.
	s := newTestServer(t)
	id := seedUserRuleWithPredicate(t, s, `{"author_is_self":true}`)

	// PATCH with explicit null — should clear the predicate.
	rec := doJSON(t, s, "PATCH", "/api/task-rules/"+id, map[string]any{
		"scope_predicate_json": nil,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch failed: %d: %s", rec.Code, rec.Body.String())
	}

	got := getRule(t, s, id)
	if got.ScopePredicateJSON != nil {
		t.Errorf("expected predicate to be cleared (nil), got %q", *got.ScopePredicateJSON)
	}
}

func TestTaskRulePatch_PredicateEmptyString_ClearsToMatchAll(t *testing.T) {
	// Sending explicit "" should also clear (ValidatePredicateJSON treats
	// "" as match-all). Alternative clear spelling for clients that prefer
	// strings.
	s := newTestServer(t)
	id := seedUserRuleWithPredicate(t, s, `{"author_is_self":true}`)

	rec := doJSON(t, s, "PATCH", "/api/task-rules/"+id, map[string]any{
		"scope_predicate_json": "",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch failed: %d: %s", rec.Code, rec.Body.String())
	}
	got := getRule(t, s, id)
	if got.ScopePredicateJSON != nil {
		t.Errorf("expected predicate cleared via empty string, got %q", *got.ScopePredicateJSON)
	}
}

func TestTaskRulePatch_PredicateNewValue_Replaces(t *testing.T) {
	s := newTestServer(t)
	id := seedUserRuleWithPredicate(t, s, `{"author_is_self":true}`)

	rec := doJSON(t, s, "PATCH", "/api/task-rules/"+id, map[string]any{
		"scope_predicate_json": `{"author_is_self":true,"is_draft":true}`,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch failed: %d: %s", rec.Code, rec.Body.String())
	}
	got := getRule(t, s, id)
	if got.ScopePredicateJSON == nil {
		t.Fatal("predicate unexpectedly cleared")
	}
	if *got.ScopePredicateJSON != `{"author_is_self":true,"is_draft":true}` {
		t.Errorf("unexpected predicate: %q", *got.ScopePredicateJSON)
	}
}

func TestTaskRulePatch_PredicateInvalidField_Rejects(t *testing.T) {
	s := newTestServer(t)
	id := seedUserRuleWithPredicate(t, s, `{"author_is_self":true}`)

	rec := doJSON(t, s, "PATCH", "/api/task-rules/"+id, map[string]any{
		"scope_predicate_json": `{"bogus_field":true}`,
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 on unknown predicate field, got %d", rec.Code)
	}
	// Verify the rule is unchanged.
	got := getRule(t, s, id)
	if got.ScopePredicateJSON == nil || *got.ScopePredicateJSON != `{"author_is_self":true}` {
		t.Errorf("rule changed despite validation failure: %v", got.ScopePredicateJSON)
	}
}

// TestTaskRuleRoundTrip_GetThenPostUnchanged is the scenario the review bot
// was worried about: client GETs a rule, wants to duplicate it, POSTs the
// same shape. The null predicate on the response must be accepted on the
// request.
func TestTaskRuleRoundTrip_GetThenPostUnchanged(t *testing.T) {
	s := newTestServer(t)
	if err := db.SeedTaskRules(s.db); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// GET the list, find a rule with null predicate (system-rule-review-requested).
	rec := doJSON(t, s, "GET", "/api/task-rules", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list failed: %d", rec.Code)
	}
	var rules []domain.TaskRule
	if err := json.Unmarshal(rec.Body.Bytes(), &rules); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var template *domain.TaskRule
	for i := range rules {
		if rules[i].ID == "system-rule-review-requested" {
			template = &rules[i]
			break
		}
	}
	if template == nil {
		t.Fatal("seeded review-requested rule missing")
	}
	if template.ScopePredicateJSON != nil {
		t.Fatalf("expected seeded rule to have null predicate, got %q", *template.ScopePredicateJSON)
	}

	// Now POST a new rule using that exact shape (changing just the event_type
	// so it doesn't collide with the seeded rule).
	body := map[string]any{
		"event_type":           "github:pr:new_commits",
		"scope_predicate_json": template.ScopePredicateJSON, // nil → JSON null
		"enabled":              template.Enabled,
		"name":                 "Duplicated from template",
		"default_priority":     template.DefaultPriority,
		"sort_order":           template.SortOrder,
	}
	rec = doJSON(t, s, "POST", "/api/task-rules", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST round-trip failed: %d: %s", rec.Code, rec.Body.String())
	}
}
