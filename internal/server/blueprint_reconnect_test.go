package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Reconnection mutations (TFAC-312's canvas "drag an arrow's head onto a new
// target" gesture): the sequence-edge re-target (atomic split + merge, leaving
// an orphaned downstream) and the trigger-edge re-target (in-place blueprint_id
// move). These are the re-target half of rule 4; the detach half reuses the
// existing split / remove-trigger endpoints.

type reconnectView struct {
	Host              blueprintWithStepsView `json:"host"`
	OrphanBlueprintID string                 `json:"orphan_blueprint_id"`
}

// build3StepBlueprint makes a bare blueprint with three bare-prompt steps and
// returns (blueprintID, [p0,p1,p2]).
func build3StepBlueprint(t *testing.T, s *Server, name string) (string, [3]string) {
	t.Helper()
	var pids [3]string
	steps := make([]map[string]any, 0, 3)
	for i := range pids {
		pids[i] = createBarePrompt(t, s, name+"-p")
		steps = append(steps, map[string]any{"step_prompt_id": pids[i]})
	}
	bp := createBareBlueprint(t, s, name)
	if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{"steps": steps}); put.Code != http.StatusNoContent {
		t.Fatalf("seed 3 steps: %d: %s", put.Code, put.Body.String())
	}
	return bp, pids
}

// --- sequence-edge re-target (POST /api/blueprints/{id}/reconnect) --------

func TestBlueprintReconnect_RepointsUpstreamAndOrphansDownstream(t *testing.T) {
	s := newTestServer(t)
	b, p := build3StepBlueprint(t, s, "Chain") // [p0,p1,p2]
	target, q0 := createWrappedBlueprint(t, s, "Target")

	// Re-point the boundary before step 1: [p0] stays and continues into the
	// target's chain; [p1,p2] peel off as a new orphan.
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+b+"/reconnect", map[string]any{
		"at_step_index":       1,
		"target_blueprint_id": target,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reconnect: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp reconnectView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Host keeps its id and is now [p0, q0], densely indexed.
	if resp.Host.Blueprint["id"] != b {
		t.Errorf("host id=%v, want %s", resp.Host.Blueprint["id"], b)
	}
	if len(resp.Host.Steps) != 2 {
		t.Fatalf("host steps=%d, want 2: %+v", len(resp.Host.Steps), resp.Host.Steps)
	}
	if resp.Host.Steps[0]["step_prompt_id"] != p[0] || resp.Host.Steps[1]["step_prompt_id"] != q0 {
		t.Errorf("host prompts = [%v,%v], want [%s,%s]",
			resp.Host.Steps[0]["step_prompt_id"], resp.Host.Steps[1]["step_prompt_id"], p[0], q0)
	}
	if resp.Host.Steps[0]["step_index"].(float64) != 0 || resp.Host.Steps[1]["step_index"].(float64) != 1 {
		t.Errorf("host indices not dense: %+v", resp.Host.Steps)
	}
	// Orphan holds the peeled tail [p1,p2], re-densified from 0.
	if resp.OrphanBlueprintID == "" {
		t.Fatal("expected an orphan_blueprint_id")
	}
	orphanSteps := doJSON(t, s, http.MethodGet, "/api/blueprints/"+resp.OrphanBlueprintID+"/steps", nil)
	var os composeSteps
	_ = json.Unmarshal(orphanSteps.Body.Bytes(), &os)
	if len(os) != 2 || os[0]["step_prompt_id"] != p[1] || os[1]["step_prompt_id"] != p[2] {
		t.Fatalf("orphan steps = %+v, want [%s,%s]", os, p[1], p[2])
	}
	// Target is absorbed + retired.
	if g := doJSON(t, s, http.MethodGet, "/api/blueprints/"+target+"/steps", nil); g.Code != http.StatusNotFound {
		t.Fatalf("GET target steps after reconnect: expected 404, got %d", g.Code)
	}
}

func TestBlueprintReconnect_SelfRejected(t *testing.T) {
	s := newTestServer(t)
	b, _ := build3StepBlueprint(t, s, "Self")
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+b+"/reconnect", map[string]any{
		"at_step_index":       1,
		"target_blueprint_id": b,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reconnect into self: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBlueprintReconnect_TriggeredTargetRejected(t *testing.T) {
	s := newTestServer(t)
	b, p := build3StepBlueprint(t, s, "Chain")
	target, _ := createWrappedBlueprint(t, s, "TriggeredTarget")
	if tr := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":                     "trigger",
		"event_type":               "github:pr:ci_check_failed",
		"blueprint_id":             target,
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.0,
	}); tr.Code != http.StatusCreated {
		t.Fatalf("seed trigger on target: %d: %s", tr.Code, tr.Body.String())
	}
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+b+"/reconnect", map[string]any{
		"at_step_index":       1,
		"target_blueprint_id": target,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reconnect to triggered target: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	// Host untouched — still its original 3 steps (single transaction).
	steps := doJSON(t, s, http.MethodGet, "/api/blueprints/"+b+"/steps", nil)
	var hs composeSteps
	_ = json.Unmarshal(steps.Body.Bytes(), &hs)
	if len(hs) != 3 {
		t.Fatalf("host steps after rejected reconnect = %d, want 3 (untouched). p0=%s", len(hs), p[0])
	}
}

func TestBlueprintReconnect_IndexOutOfRangeRejected(t *testing.T) {
	s := newTestServer(t)
	b, _ := build3StepBlueprint(t, s, "Chain")
	target, _ := createWrappedBlueprint(t, s, "Target")
	for _, k := range []int{0, 3, 9} { // 0 keeps the upstream empty; ≥N keeps the downstream empty.
		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+b+"/reconnect", map[string]any{
			"at_step_index":       k,
			"target_blueprint_id": target,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("reconnect at index %d: expected 422, got %d: %s", k, rec.Code, rec.Body.String())
		}
	}
}

func TestBlueprintReconnect_MissingIndexRejected(t *testing.T) {
	s := newTestServer(t)
	b, _ := build3StepBlueprint(t, s, "Chain")
	target, _ := createWrappedBlueprint(t, s, "Target")
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+b+"/reconnect", map[string]any{
		"target_blueprint_id": target,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reconnect without at_step_index: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- trigger-edge re-target (POST /api/event-handlers/{id}/retarget) ------

func createTrigger(t *testing.T, s *Server, blueprintID string) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":                     "trigger",
		"event_type":               "github:pr:ci_check_failed",
		"blueprint_id":             blueprintID,
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.0,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create trigger: %d: %s", rec.Code, rec.Body.String())
	}
	var h map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode trigger: %v", err)
	}
	return h["id"].(string)
}

func TestEventHandlerRetarget_MovesTriggerPreservingID(t *testing.T) {
	s := newTestServer(t)
	from, _ := createWrappedBlueprint(t, s, "From")
	to, _ := createWrappedBlueprint(t, s, "To")
	trigID := createTrigger(t, s, from)

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+trigID+"/retarget", map[string]any{
		"blueprint_id": to,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("retarget: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var h map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &h); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h["id"] != trigID {
		t.Errorf("trigger id changed: got %v, want %s (move must preserve the row)", h["id"], trigID)
	}
	if h["blueprint_id"] != to {
		t.Errorf("blueprint_id=%v, want %s", h["blueprint_id"], to)
	}
	// The source blueprint is now trigger-less, the target carries the trigger.
	if got := len(triggersForBlueprint(t, s, from)); got != 0 {
		t.Errorf("source has %d triggers after move, want 0", got)
	}
	if got := len(triggersForBlueprint(t, s, to)); got != 1 {
		t.Errorf("target has %d triggers after move, want 1", got)
	}
}

func TestEventHandlerRetarget_AlreadyTriggeredRejected(t *testing.T) {
	s := newTestServer(t)
	from, _ := createWrappedBlueprint(t, s, "From")
	to, _ := createWrappedBlueprint(t, s, "To")
	trigID := createTrigger(t, s, from)
	_ = createTrigger(t, s, to) // `to` already has its own trigger

	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+trigID+"/retarget", map[string]any{
		"blueprint_id": to,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("retarget onto triggered blueprint: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestEventHandlerRetarget_RuleRejected(t *testing.T) {
	s := newTestServer(t)
	to, _ := createWrappedBlueprint(t, s, "To")
	rule := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
		"kind":       "rule",
		"event_type": "github:pr:ci_check_failed",
		"name":       "a rule",
	})
	if rule.Code != http.StatusCreated {
		t.Fatalf("create rule: %d: %s", rule.Code, rule.Body.String())
	}
	var rh map[string]any
	_ = json.Unmarshal(rule.Body.Bytes(), &rh)
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+rh["id"].(string)+"/retarget", map[string]any{
		"blueprint_id": to,
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("retarget a rule: expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}
