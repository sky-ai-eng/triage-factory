package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// listEventHandlers reads one page of the handler list. kind is "", "rule", or
// "trigger"; the page bound is the route's max, which every fixture fits in.
func listEventHandlers(t *testing.T, s *Server, kind string) []map[string]any {
	t.Helper()
	body := map[string]any{"page_size": 200}
	if kind != "" {
		body["kind"] = kind
	}
	return decodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/event-handlers/list", body)).Items
}

// seedBlueprintForTrigger inserts a user-source blueprint that trigger
// fixtures point at. event_handlers.blueprint_id has composite FKs to
// blueprints(id, org_id) AND the same-team blueprints(id, team_id); the
// handler also validates the id via Blueprints.Get before insert. Without a
// real blueprint row, creating a trigger fails the FK (or the handler-level
// lookup, whichever fires first). Existence-guarded so a test that seeds the
// same id twice is a no-op (Create is not idempotent on its own).
func seedBlueprintForTrigger(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx := t.Context()
	if existing, _ := s.blueprints.Get(ctx, runmode.LocalDefaultOrgID, id); existing != nil {
		return
	}
	if err := s.blueprints.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: id, Name: id, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint %s: %v", id, err)
	}
}

// --- POST /api/event-handlers --------------------------------------------

func TestHandleEventHandlerCreate_RuleHappyPath(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":             "rule",
		"event_type":       "github:pr:new_commits",
		"name":             "Heads-up on new commits",
		"default_priority": 0.6,
		"sort_order":       3,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["kind"] != "rule" {
		t.Errorf("kind=%v want rule", got["kind"])
	}
	if got["name"] != "Heads-up on new commits" {
		t.Errorf("name=%v", got["name"])
	}
	if got["source"] != "user" {
		t.Errorf("source=%v want user", got["source"])
	}
	// Rule rows have NULL trigger-only fields on the wire.
	if got["blueprint_id"] != "" {
		t.Errorf("blueprint_id=%v; rule rows must serialize empty", got["blueprint_id"])
	}
	if got["breaker_threshold"] != nil {
		t.Errorf("breaker_threshold=%v; rule rows must serialize null", got["breaker_threshold"])
	}
}

func TestHandleEventHandlerCreate_TriggerHappyPath(t *testing.T) {
	s := newTestServer(t)
	seedBlueprintForTrigger(t, s, "p-trigger-create")
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":                     "trigger",
		"event_type":               "github:pr:ci_check_failed",
		"blueprint_id":             "p-trigger-create",
		"breaker_threshold":        5,
		"min_autonomy_suitability": 0.4,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["kind"] != "trigger" {
		t.Errorf("kind=%v want trigger", got["kind"])
	}
	if got["blueprint_id"] != "p-trigger-create" {
		t.Errorf("blueprint_id=%v", got["blueprint_id"])
	}
	// Triggers default to disabled (project convention — users opt in).
	if got["enabled"] != false {
		t.Errorf("enabled=%v; new triggers must default to false", got["enabled"])
	}
}

func TestHandleEventHandlerCreate_TriggerAppliesDefaults(t *testing.T) {
	// breaker_threshold and min_autonomy_suitability are documented as
	// optional with defaults (4 and 0.0) — preserved from the
	// the original /api/triggers contract so drag-to-create paths can
	// supply only prompt_id + event_type.
	s := newTestServer(t)
	seedBlueprintForTrigger(t, s, "p-defaults")
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":         "trigger",
		"event_type":   "github:pr:ci_check_failed",
		"blueprint_id": "p-defaults",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["breaker_threshold"] != float64(4) {
		t.Errorf("breaker_threshold=%v; want default 4", got["breaker_threshold"])
	}
	if got["min_autonomy_suitability"] != float64(0) {
		t.Errorf("min_autonomy_suitability=%v; want default 0", got["min_autonomy_suitability"])
	}
}

func TestHandleEventHandlerCreate_RejectsUnknownKind(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":       "automation", // not in the enum
		"event_type": "github:pr:opened",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleEventHandlerCreate_RuleRequiresName(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":       "rule",
		"event_type": "github:pr:opened",
		// no name
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleEventHandlerCreate_TriggerRequiresPromptID(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":       "trigger",
		"event_type": "github:pr:ci_check_failed",
		// no prompt_id
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleEventHandlerCreate_RejectsUnknownEventType(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":             "rule",
		"event_type":       "not:a:real:event",
		"name":             "x",
		"default_priority": 0.5,
		"sort_order":       0,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleEventHandlerCreate_CanonicalizesPredicate(t *testing.T) {
	// The predicate canonicalizer normalizes empty / {} / null to "" so
	// the wire-out form is identical for match-all regardless of the
	// shape the client sent. Pin the round-trip.
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":                 "rule",
		"event_type":           "github:pr:new_commits",
		"name":                 "match-all",
		"default_priority":     0.5,
		"sort_order":           0,
		"scope_predicate_json": "{}",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["scope_predicate_json"] != nil {
		t.Errorf("empty predicate must round-trip as null, got %v", got["scope_predicate_json"])
	}
}

func TestHandleEventHandlerCreate_RejectsInvalidPredicate(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":                 "rule",
		"event_type":           "github:pr:new_commits",
		"name":                 "bad",
		"default_priority":     0.5,
		"sort_order":           0,
		"scope_predicate_json": `{"not_a_real_field": true}`,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- PATCH /api/event-handlers/{id} --------------------------------------

func TestHandleEventHandlerUpdate_PatchesRule(t *testing.T) {
	s := newTestServer(t)
	id := createUserRule(t, s)

	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"name":             "renamed",
		"default_priority": 0.9,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["name"] != "renamed" {
		t.Errorf("name=%v", got["name"])
	}
	if got["default_priority"] != 0.9 {
		t.Errorf("default_priority=%v", got["default_priority"])
	}
}

func TestHandleEventHandlerUpdate_NotFound(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/does-not-exist", map[string]any{
		"name": "x",
	})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// --- POST /api/event-handlers/{id}/toggle --------------------------------

func TestHandleEventHandlerToggle_FlipsEnabled(t *testing.T) {
	s := newTestServer(t)
	id := createUserRule(t, s)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+id+"/toggle", map[string]any{"enabled": false})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// Re-read to confirm.
	got := listEventHandlers(t, s, "rule")
	var found bool
	for _, h := range got {
		if h["id"] == id {
			found = true
			if h["enabled"] != false {
				t.Errorf("after toggle off, enabled=%v want false", h["enabled"])
			}
		}
	}
	if !found {
		t.Error("created rule missing from list")
	}
}

// TestHandleEventHandlerToggle_RequiresEnabled pins that an absent `enabled`
// is rejected rather than read as false. A non-pointer bool made `{}` a
// silent disable with a 200 — a body a client sends by mistake turning a
// user's rule off. The explicit `{"enabled": false}` disable is covered by
// TestHandleEventHandlerToggle_FlipsEnabled above and must keep working.
func TestHandleEventHandlerToggle_RequiresEnabled(t *testing.T) {
	s := newTestServer(t)
	id := createUserRule(t, s)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+id+"/toggle", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("toggle with empty body = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	// The rule is untouched — a rule created through the API starts enabled.
	// The list call is this assertion's instrument, so its own failures are
	// fatal and named (decodeList fatals on a non-200): an error body decoded
	// as an empty list would otherwise read as "the rule vanished" and send a
	// reader after the wrong bug.
	got := listEventHandlers(t, s, "rule")
	var found bool
	for _, h := range got {
		if h["id"] == id {
			found = true
			if h["enabled"] != true {
				t.Errorf("after a refused toggle, enabled=%v want true (unchanged)", h["enabled"])
			}
		}
	}
	if !found {
		t.Error("created rule missing from list")
	}
}

// --- DELETE /api/event-handlers/{id} -------------------------------------

func TestHandleEventHandlerDelete_UserRowHardDeletes(t *testing.T) {
	s := newTestServer(t)
	id := createUserRule(t, s)

	rec := doJSON(t, s, http.MethodDelete, "/api/event-handlers/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "deleted" {
		t.Errorf("status=%v want deleted", got["status"])
	}
	// GET returns 404 now.
	follow := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"name": "x"})
	if follow.Code != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", follow.Code)
	}
}

// TestHandleEventHandlerDelete_SystemRowSoftDeletes pins the current
// behavior: a shipped (system_slug) handler soft-deletes — deleted_at is
// stamped, but the row survives so its (org_id, team_id, system_slug)
// identity stays occupied and the boot-time shipped-content sync never
// resurrects it.
func TestHandleEventHandlerDelete_SystemRowSoftDeletes(t *testing.T) {
	s := newTestServer(t)
	// Insert a system-source handler directly (newTestServer seeds the
	// tenant but no handlers; provisioning would, but a single row keeps
	// the test focused). source='system' requires creator_user_id NULL.
	id := fixtureUUID("sys-handler-del")
	if _, err := s.db.Exec(`
		INSERT INTO event_handlers
			(id, org_id, team_id, kind, event_type, source, system_slug, name, default_priority, sort_order, enabled, creator_user_id)
		VALUES (?, ?, ?, 'rule', 'github:pr:opened', 'system', 'system-test-del', 'Shipped Rule', 0.5, 0, 1, NULL)
	`, id, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID); err != nil {
		t.Fatalf("seed system handler: %v", err)
	}

	rec := doJSON(t, s, http.MethodDelete, "/api/event-handlers/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "deleted" {
		t.Errorf("status=%v want deleted", got["status"])
	}
	// GET returns 404 (deleted_at IS NULL filter), same observable shape as a
	// hard delete to every caller except the sync.
	follow := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{"name": "x"})
	if follow.Code != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", follow.Code)
	}
	// The row survives (deleted_at stamped, not removed) so the slug slot
	// stays occupied — not a soft-disable (enabled untouched, still hidden
	// from every read path).
	var deletedAt sql.NullString
	if err := s.db.QueryRow(`SELECT deleted_at FROM event_handlers WHERE id = ?`, id).Scan(&deletedAt); err != nil {
		t.Fatalf("query row: %v", err)
	}
	if !deletedAt.Valid {
		t.Errorf("deleted_at not stamped on the shipped row; want it soft-deleted, not gone")
	}
}

// --- POST /api/event-handlers/{id}/promote -------------------------------

func TestHandleEventHandlerPromote_RuleToTrigger(t *testing.T) {
	s := newTestServer(t)
	seedBlueprintForTrigger(t, s, "p-promote")
	id := createUserRule(t, s)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+id+"/promote", map[string]any{
		"blueprint_id":             "p-promote",
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.2,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["kind"] != "trigger" {
		t.Errorf("kind=%v want trigger", got["kind"])
	}
	if got["blueprint_id"] != "p-promote" {
		t.Errorf("blueprint_id=%v", got["blueprint_id"])
	}
	// Rule-only fields must be cleared on the promoted row.
	if got["name"] != "" {
		t.Errorf("name=%v; promoted trigger must have no name", got["name"])
	}
	if got["default_priority"] != nil {
		t.Errorf("default_priority=%v; promoted trigger must serialize null", got["default_priority"])
	}
}

func TestHandleEventHandlerPromote_RejectsAlreadyTrigger(t *testing.T) {
	s := newTestServer(t)
	seedBlueprintForTrigger(t, s, "p-already-trigger")
	// Create a trigger then try to promote it.
	createRec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":                     "trigger",
		"event_type":               "github:pr:ci_check_failed",
		"blueprint_id":             "p-already-trigger",
		"breaker_threshold":        4,
		"min_autonomy_suitability": 0.0,
	})
	var created map[string]any
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+id+"/promote", map[string]any{
		"blueprint_id":             "p-already-trigger",
		"breaker_threshold":        4,
		"min_autonomy_suitability": 0.0,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleEventHandlerPromote_ValidatesTriggerFields: promote requires the
// two trigger fields and enforces their ranges — the checks create and PATCH
// already applied, and whose absence here let promote persist a negative
// breaker threshold or an out-of-band autonomy floor. Missing and out-of-range
// are both 422: the body is well-formed, the values can't be applied.
func TestHandleEventHandlerPromote_ValidatesTriggerFields(t *testing.T) {
	s := newTestServer(t)
	id := createUserRule(t, s)

	for name, body := range map[string]map[string]any{
		"missing both": {"blueprint_id": "p-promote"},
		"negative breaker": {
			"blueprint_id": "p-promote", "breaker_threshold": -1, "min_autonomy_suitability": 0.5,
		},
		"autonomy above one": {
			"blueprint_id": "p-promote", "breaker_threshold": 3, "min_autonomy_suitability": 7,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+id+"/promote", body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// --- PUT /api/event-handlers/reorder -------------------------------------

func TestHandleEventHandlerReorder_AppliesOrder(t *testing.T) {
	s := newTestServer(t)
	idA := createUserRuleWithSort(t, s, "rule-a", 0)
	idB := createUserRuleWithSort(t, s, "rule-b", 1)

	rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", []string{idB, idA})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	rules := listEventHandlers(t, s, "rule")
	// First rule in the list should be the one we put at index 0 (idB).
	if len(rules) == 0 || rules[0]["id"] != idB {
		t.Errorf("reorder didn't take: rules[0]=%v, want id=%s", rules, idB)
	}
}

func TestHandleEventHandlerReorder_RejectsEmpty(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", []string{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

// --- POST /api/event-handlers/list ---------------------------------------

func TestHandleEventHandlersList_KindFilter(t *testing.T) {
	s := newTestServer(t)
	_ = createUserRule(t, s)
	seedBlueprintForTrigger(t, s, "p-list")
	doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":                     "trigger",
		"event_type":               "github:pr:ci_check_failed",
		"blueprint_id":             "p-list",
		"breaker_threshold":        4,
		"min_autonomy_suitability": 0.0,
	})

	for _, tc := range []struct {
		kind     string
		wantKind string
	}{
		{"rule", "rule"},
		{"trigger", "trigger"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			got := listEventHandlers(t, s, tc.kind)
			if len(got) == 0 {
				t.Fatalf("kind=%s returned 0 rows", tc.kind)
			}
			for _, h := range got {
				if h["kind"] != tc.wantKind {
					t.Errorf("kind filter %q leaked a %q row", tc.kind, h["kind"])
				}
			}
		})
	}
}

func TestHandleEventHandlersList_RejectsBadKind(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/list", map[string]any{"kind": "banana"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

// --- helpers --------------------------------------------------------------

func createUserRule(t *testing.T, s *Server) string {
	return createUserRuleWithSort(t, s, "test-rule", 0)
}

func createUserRuleWithSort(t *testing.T, s *Server, name string, sortOrder int) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":             "rule",
		"event_type":       "github:pr:opened",
		"name":             name,
		"default_priority": 0.5,
		"sort_order":       sortOrder,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed user rule: %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	id, ok := got["id"].(string)
	if !ok || id == "" {
		t.Fatalf("seed rule: missing id in response: %s", rec.Body.String())
	}
	return id
}

// --- GET /api/event-handlers/{id} ----------------------------------------

// TestHandleEventHandlerGet_ShapeParityAndNotFound pins the single read the
// five mutation routes needed and nobody had: it answers with the LIST's row,
// field for field, and a well-formed-but-unknown id and a malformed one are
// both 404 (a path id that names nothing must not disclose which it was, nor
// reach the store as a bad cast).
func TestHandleEventHandlerGet_ShapeParityAndNotFound(t *testing.T) {
	s := newTestServer(t)
	created := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":             "rule",
		"event_type":       "github:pr:new_commits",
		"name":             "Heads-up on new commits",
		"default_priority": 0.6,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("seed create = %d; body=%s", created.Code, created.Body.String())
	}
	var row map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &row); err != nil {
		t.Fatalf("decode create: %v", err)
	}
	id, _ := row["id"].(string)
	if id == "" {
		t.Fatalf("created handler has no id: %v", row)
	}

	rec := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode single read: %v", err)
	}

	// Shape parity with the list row — same keys, same values. A bespoke
	// single-read projection is exactly what this asserts against.
	var listed map[string]any
	for _, h := range listEventHandlers(t, s, "") {
		if h["id"] == id {
			listed = h
		}
	}
	if listed == nil {
		t.Fatalf("seeded handler %s absent from the list", id)
	}
	if len(got) != len(listed) {
		t.Errorf("single read has %d keys, list row has %d — the projections have diverged\nsingle=%v\nlist=%v",
			len(got), len(listed), got, listed)
	}
	for k, want := range listed {
		if fmt.Sprint(got[k]) != fmt.Sprint(want) {
			t.Errorf("field %q: single read = %v, list row = %v", k, got[k], want)
		}
	}

	for _, badID := range []string{"11111111-1111-1111-1111-111111111111", "not-a-uuid"} {
		if rec := doJSON(t, s, http.MethodGet, "/api/event-handlers/"+badID, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET /api/event-handlers/%s = %d, want 404; body=%s", badID, rec.Code, rec.Body.String())
		}
	}
}
