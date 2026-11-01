package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// seedBlueprintForTrigger inserts the user-source blueprint a trigger fixture
// points at, which the handler and the same-team FKs both require. Seeding an
// id twice is a no-op.
func seedBlueprintForTrigger(t *testing.T, s *Server, id string) {
	t.Helper()
	ctx := t.Context()
	if existing, _ := s.blueprints.Get(ctx, runmode.LocalDefaultOrgID, id); existing != nil {
		return
	}
	if _, err := s.blueprints.Create(ctx, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Blueprint{
		ID: id, Name: id, Source: "user", TeamID: runmode.LocalDefaultTeamID,
	}); err != nil {
		t.Fatalf("seed blueprint %s: %v", id, err)
	}
}

// --- POST /api/event-handlers/rules + /triggers ---------------------------

// TestHandleEventHandlerCreate_RejectsCrossKindFields: the path names the kind,
// so each create body is decoded against one schema, and the other kind's fields
// or a `kind` field are UNKNOWN_FIELD rather than accepted and dropped.
func TestHandleEventHandlerCreate_RejectsCrossKindFields(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	for name, tc := range map[string]struct {
		path string
		body map[string]any
	}{
		"kind on rule create": {"/api/event-handlers/rules", map[string]any{
			"kind": "rule", "event_type": "github:pr:opened", "name": "x",
		}},
		"kind on trigger create": {"/api/event-handlers/triggers", map[string]any{
			"kind": "trigger", "event_type": "github:pr:ci_check_failed", "blueprint_id": "p",
		}},
		"trigger field on rule create": {"/api/event-handlers/rules", map[string]any{
			"event_type": "github:pr:opened", "name": "x", "breaker_threshold": 4,
		}},
		"rule field on trigger create": {"/api/event-handlers/triggers", map[string]any{
			"event_type": "github:pr:ci_check_failed", "blueprint_id": "p", "default_priority": 0.5,
		}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if reason := firstErrorReason(t, rec); reason != "UNKNOWN_FIELD" {
				t.Errorf("reason = %q, want UNKNOWN_FIELD", reason)
			}
		})
	}
}

func TestHandleEventHandlerCreate_RuleRequiresName(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type": "github:pr:opened",
		// no name
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleEventHandlerCreate_TriggerRequiresBlueprintID(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type": "github:pr:ci_check_failed",
		// no blueprint_id
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleEventHandlerCreate_RejectsSystemEventType pins that a system
// sentinel — including the poll's close obligation — is never a handler's
// subject: the router matches no handler on one, so the create refuses it as
// a field fault rather than storing a handler that would never fire.
func TestHandleEventHandlerCreate_RejectsSystemEventType(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	for _, eventType := range []string{domain.EventSystemEntityCloseOwed, domain.EventSystemPollCompleted, domain.EventSystemRoutingDisposition} {
		rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
			"event_type":       eventType,
			"name":             "x",
			"default_priority": 0.5,
			"sort_order":       0,
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: expected 400, got %d: %s", eventType, rec.Code, rec.Body.String())
			continue
		}
		items := decodeErrorItems(t, rec)
		if len(items) != 1 || items[0].Reason != "INVALID_FIELD" || items[0].Field != "event_type" {
			t.Errorf("%s: want one INVALID_FIELD item on event_type, got %+v", eventType, items)
		}
	}
}

func TestHandleEventHandlerCreate_RejectsUnknownEventType(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
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
	configureEventSources(t, s)
	for name, predicate := range map[string]any{
		"empty object":  map[string]any{},
		"explicit null": nil,
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
				"event_type":       "github:pr:new_commits",
				"name":             "match-all " + name,
				"default_priority": 0.5,
				"sort_order":       0,
				"scope_predicate":  predicate,
			})
			if rec.Code != http.StatusCreated {
				t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
			}
			var got map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
			if got["scope_predicate_json"] != nil {
				t.Errorf("empty predicate must round-trip as null, got %v", got["scope_predicate_json"])
			}
		})
	}
}

// TestHandleEventHandlerCreate_PredicateIsAnObject: the predicate's one wire
// type is a JSON object, stored canonically; a string-wrapped object is refused
// rather than unwrapped, so the field has one spelling.
func TestHandleEventHandlerCreate_PredicateIsAnObject(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	body := func(predicate any) map[string]any {
		return map[string]any{
			"event_type": "github:pr:new_commits", "name": "predicate", "scope_predicate": predicate,
		}
	}

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", body(map[string]any{"author_id_in": []string{"583231"}}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("object predicate: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["scope_predicate_json"] != `{"author_id_in":["583231"]}` {
		t.Errorf("stored predicate = %v, want the canonical object text", got["scope_predicate_json"])
	}

	if rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", body(`{"author_id_in":["583231"]}`)); rec.Code != http.StatusBadRequest {
		t.Errorf("string-wrapped predicate: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- PATCH /api/event-handlers/{id} --------------------------------------

// TestHandleEventHandlerUpdate_PatchesRule: PATCH writes the named fields,
// enabled among them, and the list reads the write back.
func TestHandleEventHandlerUpdate_PatchesRule(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	id := createUserRule(t, s)

	rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+id, map[string]any{
		"name":             "renamed",
		"default_priority": 0.9,
		"enabled":          false,
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
	var listed map[string]any
	for _, h := range listEventHandlers(t, s, "rule") {
		if h["id"] == id {
			listed = h
		}
	}
	if listed == nil || listed["enabled"] != false {
		t.Errorf("listed rule = %v, want it present with enabled=false", listed)
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

// TestHandleEventHandlerUpdate_RejectsOtherKindsFields: the row's kind picks the
// PATCH schema, so the other kind's fields, `kind`, event_type and blueprint_id
// are UNKNOWN_FIELD rather than dropped behind a 200.
func TestHandleEventHandlerUpdate_RejectsOtherKindsFields(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedBlueprintForTrigger(t, s, "p-patch-kinds")
	ruleID := createUserRule(t, s)
	triggerID := createUserTrigger(t, s, "p-patch-kinds")

	for name, tc := range map[string]struct {
		id   string
		body map[string]any
	}{
		"trigger field on a rule":   {ruleID, map[string]any{"breaker_threshold": 9}},
		"rule field on a trigger":   {triggerID, map[string]any{"default_priority": 0.9}},
		"kind on a rule":            {ruleID, map[string]any{"kind": "trigger"}},
		"kind on a trigger":         {triggerID, map[string]any{"kind": "rule"}},
		"event_type on a rule":      {ruleID, map[string]any{"event_type": "github:pr:opened"}},
		"blueprint_id on a trigger": {triggerID, map[string]any{"blueprint_id": "p-patch-kinds"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPatch, "/api/event-handlers/"+tc.id, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			if reason := firstErrorReason(t, rec); reason != "UNKNOWN_FIELD" {
				t.Errorf("reason = %q, want UNKNOWN_FIELD", reason)
			}
		})
	}
}

// TestHandleEventHandlerRemovedRoutes: PATCH is the one update and the path
// names the kind on create, so PUT, a /toggle verb and an untyped create answer
// 404/405 and leave the rule as it was.
func TestHandleEventHandlerRemovedRoutes(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	id := createUserRule(t, s)

	for name, tc := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"PUT alias":          {http.MethodPut, "/api/event-handlers/" + id, map[string]any{"name": "x"}},
		"toggle verb":        {http.MethodPost, "/api/event-handlers/" + id + "/toggle", map[string]any{"enabled": false}},
		"polymorphic create": {http.MethodPost, "/api/event-handlers", map[string]any{"kind": "rule", "event_type": "github:pr:opened", "name": "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, tc.method, tc.path, tc.body)
			if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s = %d, want 404/405; body=%s", tc.method, tc.path, rec.Code, rec.Body.String())
			}
		})
	}

	// And the rule it was aimed at is untouched — a rule created through the
	// API starts enabled.
	for _, h := range listEventHandlers(t, s, "rule") {
		if h["id"] == id && h["enabled"] != true {
			t.Errorf("enabled=%v after the refused calls, want true (unchanged)", h["enabled"])
		}
	}
}

// --- DELETE /api/event-handlers/{id} -------------------------------------

func TestHandleEventHandlerDelete_UserRowHardDeletes(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
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

// TestHandleEventHandlerDelete_SystemRowSoftDeletes: a shipped (system_slug)
// handler is soft-deleted, so its slug stays occupied and the boot-time sync
// never resurrects it, while every read answers 404 as for a hard delete.
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

func TestHandleEventHandlerPromote_RejectsAlreadyTrigger(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedBlueprintForTrigger(t, s, "p-already-trigger")
	// Create a trigger then try to promote it.
	id := createUserTrigger(t, s, "p-already-trigger")

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+id+"/promote", map[string]any{
		"blueprint_id":             "p-already-trigger",
		"breaker_threshold":        4,
		"min_autonomy_suitability": 0.0,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleEventHandlerPromote_ValidatesTriggerFields: promote enforces the
// ranges create and PATCH apply, with a 422 for a well-formed value out of range.
func TestHandleEventHandlerPromote_ValidatesTriggerFields(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	// The blueprint exists, because a missing one is a 404 answered before
	// the body's 422s.
	seedBlueprintForTrigger(t, s, "p-promote")
	id := createUserRule(t, s)

	for name, body := range map[string]map[string]any{
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

// TestEventHandlerTriggerParity_CreateAndPromote: create and promote mint the
// same trigger for the same body, with the tuning fields defaulted (breaker 4,
// autonomy 0), new triggers disabled, and the rule-only fields cleared.
func TestEventHandlerTriggerParity_CreateAndPromote(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedBlueprintForTrigger(t, s, "p-parity-created")
	seedBlueprintForTrigger(t, s, "p-parity-promoted")

	created := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":   "github:pr:ci_check_failed",
		"blueprint_id": "p-parity-created",
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create trigger: %d: %s", created.Code, created.Body.String())
	}

	ruleID := createUserRuleFor(t, s, "github:pr:ci_check_failed")
	promoted := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+ruleID+"/promote", map[string]any{
		"blueprint_id": "p-parity-promoted",
	})
	if promoted.Code != http.StatusOK {
		t.Fatalf("promote with the same omissions: %d: %s", promoted.Code, promoted.Body.String())
	}

	var a, b map[string]any
	_ = json.Unmarshal(created.Body.Bytes(), &a)
	_ = json.Unmarshal(promoted.Body.Bytes(), &b)
	if a["kind"] != "trigger" || a["blueprint_id"] != "p-parity-created" || a["enabled"] != false ||
		a["breaker_threshold"] != float64(4) || a["min_autonomy_suitability"] != float64(0) {
		t.Errorf("created trigger = %v, want kind trigger on its blueprint, disabled, breaker 4, autonomy 0", a)
	}
	if b["blueprint_id"] != "p-parity-promoted" || b["name"] != "" || b["default_priority"] != nil {
		t.Errorf("promoted trigger = %v, want it on its blueprint with no name or default_priority", b)
	}
	// Everything the body didn't name must agree. id / blueprint_id / the
	// timestamps and the sync's user_modified flag are properties of which row
	// it is and how it got there, not of the trigger the caller described.
	//
	// enabled is excluded and stays excluded: promote carries the rule's own
	// activation across rather than defaulting it, so promoting a live rule
	// does not silently switch it off. That is a property of the row promote
	// starts from, not a disagreement about what an unspecified trigger is.
	for _, field := range []string{
		"kind", "event_type", "breaker_threshold", "min_autonomy_suitability",
		"scope_predicate_json", "name", "default_priority", "sort_order",
		"applies_to_unowned", "trigger_type",
	} {
		if fmt.Sprint(a[field]) != fmt.Sprint(b[field]) {
			t.Errorf("field %q: created = %v, promoted = %v", field, a[field], b[field])
		}
	}
}

// --- PUT /api/event-handlers/reorder -------------------------------------

func TestHandleEventHandlerReorder_AppliesOrder(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	idA := createUserRuleWithSort(t, s, "rule-a", 0)
	idB := createUserRuleWithSort(t, s, "rule-b", 1)

	rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", map[string]any{"ids": []string{idB, idA}})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rules := listEventHandlers(t, s, "rule")
	// First rule in the list should be the one we put at index 0 (idB).
	if len(rules) == 0 || rules[0]["id"] != idB {
		t.Errorf("reorder didn't take: rules[0]=%v, want id=%s", rules, idB)
	}
}

func TestHandleEventHandlerReorder_RejectsEmpty(t *testing.T) {
	s := newTestServer(t)
	for name, body := range map[string]any{
		"empty list": map[string]any{"ids": []string{}},
		"no ids key": map[string]any{},
		"bare array": []string{},
	} {
		t.Run(name, func(t *testing.T) {
			rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestHandleEventHandlerReorder_RejectsInexactSet: the body must name every
// visible rule exactly once, so an unknown id, a trigger id, a duplicate or an
// omitted rule is a 422, each fault naming its id and all reported at once.
func TestHandleEventHandlerReorder_RejectsInexactSet(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedBlueprintForTrigger(t, s, "p-reorder")
	idA := createUserRuleWithSort(t, s, "rule-a", 0)
	idB := createUserRuleWithSort(t, s, "rule-b", 1)
	triggerID := createUserTrigger(t, s, "p-reorder")
	unknownID := "11111111-1111-1111-1111-111111111111"

	t.Run("a trigger and an unknown id are both named", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder",
			map[string]any{"ids": []string{idA, idB, triggerID, unknownID}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{triggerID, unknownID} {
			if !strings.Contains(body, want) {
				t.Errorf("error body does not name %s: %s", want, body)
			}
		}
		if items := decodeErrorItems(t, rec); len(items) != 2 {
			t.Errorf("got %d error items, want one per bad id: %v", len(items), items)
		}
	})

	t.Run("a partial list is refused", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", map[string]any{"ids": []string{idA}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), idB) {
			t.Errorf("error body does not name the omitted rule %s: %s", idB, rec.Body.String())
		}
	})

	t.Run("duplicates are refused", func(t *testing.T) {
		rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder", map[string]any{"ids": []string{idA, idA}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("a deleted rule leaves the list", func(t *testing.T) {
		idC := createUserRuleWithSort(t, s, "rule-c", 2)
		if rec := doJSON(t, s, http.MethodDelete, "/api/event-handlers/"+idC, nil); rec.Code != http.StatusOK {
			t.Fatalf("delete rule-c: %d: %s", rec.Code, rec.Body.String())
		}
		// Naming it is now a fault...
		if rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder",
			map[string]any{"ids": []string{idB, idA, idC}}); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("deleted id: expected 422, got %d: %s", rec.Code, rec.Body.String())
		}
		// ...and omitting it is the whole visible set again.
		if rec := doJSON(t, s, http.MethodPut, "/api/event-handlers/reorder",
			map[string]any{"ids": []string{idB, idA}}); rec.Code != http.StatusOK {
			t.Errorf("exact set after delete: expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// --- POST /api/event-handlers/list ---------------------------------------

func TestHandleEventHandlersList_KindFilter(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	_ = createUserRule(t, s)
	seedBlueprintForTrigger(t, s, "p-list")
	doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
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
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
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

// createUserTrigger creates a user-source trigger on an already-seeded
// blueprint and returns its id.
func createUserTrigger(t *testing.T, s *Server, blueprintID string) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":   "github:pr:ci_check_failed",
		"blueprint_id": blueprintID,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed user trigger on %s: %d: %s", blueprintID, rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	id, ok := got["id"].(string)
	if !ok || id == "" {
		t.Fatalf("seed trigger: missing id in response: %s", rec.Body.String())
	}
	return id
}

// --- GET /api/event-handlers/{id} ----------------------------------------

// TestHandleEventHandlerGet_ShapeParityAndNotFound: the single read answers the
// list's row field for field, the created row is a user rule with null trigger
// fields, and an unknown or malformed id is a 404 either way.
func TestHandleEventHandlerGet_ShapeParityAndNotFound(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	created := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
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
	// A rule row carries the trigger-only fields empty on the wire.
	if row["kind"] != "rule" || row["name"] != "Heads-up on new commits" || row["source"] != "user" ||
		row["blueprint_id"] != "" || row["breaker_threshold"] != nil {
		t.Errorf("created rule = %v, want a user rule with empty blueprint_id and null breaker_threshold", row)
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
