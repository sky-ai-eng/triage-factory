package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// Reconnection, the canvas gesture of dragging a sequence arrow's head onto a
// new target: an atomic split + merge, leaving an orphaned downstream.

type reconnectView struct {
	Host              blueprintWithStepsView `json:"host"`
	OrphanBlueprintID string                 `json:"orphan_blueprint_id"`
}

// --- sequence-edge re-target (POST /api/blueprints/{id}/reconnect) --------

func TestBlueprintReconnect_RepointsUpstreamAndOrphansDownstream(t *testing.T) {
	s := newTestServer(t)
	b, p := createChainBlueprint(t, s, "Chain", 3) // [p0,p1,p2]
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
	os := listBlueprintSteps(t, s, resp.OrphanBlueprintID)
	if len(os) != 2 || os[0]["step_prompt_id"] != p[1] || os[1]["step_prompt_id"] != p[2] {
		t.Fatalf("orphan steps = %+v, want [%s,%s]", os, p[1], p[2])
	}
	// Target is absorbed + retired.
	if blueprintExists(t, s, target) {
		t.Fatal("GET target steps after reconnect: the blueprint is still readable")
	}
}

func TestBlueprintReconnect_SelfRejected(t *testing.T) {
	s := newTestServer(t)
	b, _ := createChainBlueprint(t, s, "Self", 3)
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+b+"/reconnect", map[string]any{
		"at_step_index":       1,
		"target_blueprint_id": b,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reconnect into self: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonInvalidField, "target_blueprint_id")
}

func TestBlueprintReconnect_TriggeredTargetRejected(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	b, p := createChainBlueprint(t, s, "Chain", 3)
	target, _ := createWrappedBlueprint(t, s, "TriggeredTarget")
	attachHandler(t, s, target, "github:pr:ci_check_failed")
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+b+"/reconnect", map[string]any{
		"at_step_index":       1,
		"target_blueprint_id": target,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("reconnect to triggered target: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	// A state conflict the caller can clear, unlike the self-reference above.
	httpxtest.AssertFirstError(t, rec, httpx.ReasonConflict, "target_blueprint_id")
	// Host untouched — still its original 3 steps (single transaction).
	hs := listBlueprintSteps(t, s, b)
	if len(hs) != 3 {
		t.Fatalf("host steps after rejected reconnect = %d, want 3 (untouched). p0=%s", len(hs), p[0])
	}
	// The reconnect retires the target before it finds the handler.
	assertLiveWithSteps(t, s, target, 1)
}

func TestBlueprintReconnect_IndexOutOfRangeRejected(t *testing.T) {
	s := newTestServer(t)
	b, _ := createChainBlueprint(t, s, "Chain", 3)
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
	b, _ := createChainBlueprint(t, s, "Chain", 3)
	target, _ := createWrappedBlueprint(t, s, "Target")
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+b+"/reconnect", map[string]any{
		"target_blueprint_id": target,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("reconnect without at_step_index: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}
