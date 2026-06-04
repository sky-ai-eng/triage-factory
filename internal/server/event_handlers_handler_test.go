package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

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
	if existing, _ := s.blueprints.Get(ctx, runmode.LocalDefaultOrg, id); existing != nil {
		return
	}
	if err := s.blueprints.Create(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Blueprint{
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
	// pre-SKY-259 /api/triggers contract so drag-to-create paths can
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
	listRec := doJSON(t, s, http.MethodGet, "/api/event-handlers?kind=rule", nil)
	var got []map[string]any
	_ = json.Unmarshal(listRec.Body.Bytes(), &got)
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

// TestHandleEventHandlerDelete_SystemRowHardDeletes pins the post-SKY-436
// behavior: a shipped (system) handler hard-deletes unconditionally — no
// soft-disable fallback, no resurrection. Nothing re-seeds at boot, so the
// deletion is durable.
func TestHandleEventHandlerDelete_SystemRowHardDeletes(t *testing.T) {
	s := newTestServer(t)
	// Insert a system-source handler directly (newTestServer seeds the
	// tenant but no handlers; provisioning would, but a single row keeps
	// the test focused). source='system' requires creator_user_id NULL.
	const id = "sys-handler-del"
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
		t.Errorf("status=%v want deleted (system rows now hard-delete)", got["status"])
	}
	// The row is gone, not merely disabled.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_handlers WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("system handler row count=%d after delete; want 0 (hard delete, not soft-disable)", n)
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

func TestHandleEventHandlerPromote_RequiresFields(t *testing.T) {
	s := newTestServer(t)
	id := createUserRule(t, s)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+id+"/promote", map[string]any{
		"blueprint_id": "p-promote",
		// missing breaker_threshold + min_autonomy_suitability
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
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

	listRec := doJSON(t, s, http.MethodGet, "/api/event-handlers?kind=rule", nil)
	var rules []map[string]any
	_ = json.Unmarshal(listRec.Body.Bytes(), &rules)
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

// --- GET /api/event-handlers --------------------------------------------

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
			rec := doJSON(t, s, http.MethodGet, "/api/event-handlers?kind="+tc.kind, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			var got []map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &got)
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
	rec := doJSON(t, s, http.MethodGet, "/api/event-handlers?kind=banana", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
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
