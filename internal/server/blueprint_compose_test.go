package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// Composition mutations (merge / split) + the ≤1-event-per-blueprint backstop.
// These exercise the transactional endpoints the canvas calls: merge
// absorbs a trigger-less source onto a host's tail; split partitions a
// blueprint at an index into the trigger-retaining upstream and a new
// trigger-less downstream. The 409 tests pin the clean conflict on a second
// trigger-create / promote at an already-triggered blueprint.

type composeSteps []map[string]any

type blueprintWithStepsView struct {
	Blueprint map[string]any `json:"blueprint"`
	Steps     composeSteps   `json:"steps"`
}

// listBlueprintSteps reads one blueprint's steps through the single step-list
// route (the per-blueprint GET it replaced was the same query with a filter).
func listBlueprintSteps(t *testing.T, s *Server, blueprintID string) composeSteps {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/blueprint-steps/list", map[string]any{
		"blueprint_ids": []string{blueprintID},
	})
	return decodeList[map[string]any](t, rec).Items
}

// listAllBlueprintSteps reads every in-scope blueprint's steps — the canvas's
// bulk read — and groups them by blueprint id.
func listAllBlueprintSteps(t *testing.T, s *Server) map[string]composeSteps {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/blueprint-steps/list", map[string]any{})
	out := map[string]composeSteps{}
	for _, st := range decodeList[map[string]any](t, rec).Items {
		id, _ := st["blueprint_id"].(string)
		out[id] = append(out[id], st)
	}
	return out
}

// blueprintExists reports whether the canonical single read resolves the
// blueprint. It replaces the per-blueprint steps GET's 404, which was the only
// way to ask "is this blueprint gone?" before that read existed.
func blueprintExists(t *testing.T, s *Server, blueprintID string) bool {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/blueprints/"+blueprintID, nil)
	switch rec.Code {
	case http.StatusOK:
		return true
	case http.StatusNotFound:
		return false
	default:
		t.Fatalf("GET /api/blueprints/%s = %d; body=%s", blueprintID, rec.Code, rec.Body.String())
		return false
	}
}

// --- B: atomic merge -----------------------------------------------------

func TestBlueprintMerge_AppendsAndRetiresSource(t *testing.T) {
	s := newTestServer(t)
	host, hp := createWrappedBlueprint(t, s, "Host")
	source, sp := createWrappedBlueprint(t, s, "Source")

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
		"source_blueprint_id": source,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("merge: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp blueprintWithStepsView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Blueprint["id"] != host {
		t.Errorf("merged blueprint id=%v, want host %s", resp.Blueprint["id"], host)
	}
	if len(resp.Steps) != 2 {
		t.Fatalf("merged steps=%d, want 2: %+v", len(resp.Steps), resp.Steps)
	}
	// Host's step first, source's appended after, densely indexed.
	if resp.Steps[0]["step_prompt_id"] != hp || resp.Steps[1]["step_prompt_id"] != sp {
		t.Errorf("merged prompts = [%v,%v], want [%s,%s]", resp.Steps[0]["step_prompt_id"], resp.Steps[1]["step_prompt_id"], hp, sp)
	}
	if resp.Steps[0]["step_index"].(float64) != 0 || resp.Steps[1]["step_index"].(float64) != 1 {
		t.Errorf("merged indices not dense: %+v", resp.Steps)
	}

	// Source is retired — gone from request-facing reads.
	list := doJSON(t, s, http.MethodGet, "/api/blueprints", nil)
	var bps []map[string]any
	_ = json.Unmarshal(list.Body.Bytes(), &bps)
	for _, b := range bps {
		if b["id"] == source {
			t.Fatalf("source %s still listed after merge", source)
		}
	}
	if blueprintExists(t, s, source) {
		t.Fatal("GET source steps after merge: the blueprint is still readable")
	}
}

func TestBlueprintMerge_TriggeredSourceRejected(t *testing.T) {
	s := newTestServer(t)
	host, _ := createWrappedBlueprint(t, s, "Host")
	source, _ := createWrappedBlueprint(t, s, "TriggeredSource")

	// Attach a trigger to the source — unreachable from the canvas, but a
	// third-party caller can do it, so the endpoint must refuse to corrupt state.
	tr := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":               "github:pr:ci_check_failed",
		"blueprint_id":             source,
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.0,
	})
	if tr.Code != http.StatusCreated {
		t.Fatalf("seed trigger on source: %d: %s", tr.Code, tr.Body.String())
	}

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
		"source_blueprint_id": source,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("merge triggered source: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	// The host is untouched (single transaction, nothing reparented).
	hostSteps := listBlueprintSteps(t, s, host)
	if len(hostSteps) != 1 {
		t.Fatalf("host steps after rejected merge = %d, want 1 (untouched)", len(hostSteps))
	}
}

func TestBlueprintMerge_ExceedsStepCapRejected(t *testing.T) {
	s := newTestServer(t)
	// A host already at the cap; merging even a 1-step source would overflow it
	// into a blueprint the normal steps-PUT (capped at maxBlueprintSteps) could
	// no longer edit.
	host := createBareBlueprint(t, s, "BigHost")
	steps := make([]map[string]any, 0, maxBlueprintSteps)
	for i := 0; i < maxBlueprintSteps; i++ {
		pid := createBarePrompt(t, s, fmt.Sprintf("cap-step-%d", i))
		steps = append(steps, map[string]any{"step_prompt_id": pid})
	}
	if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+host+"/steps", map[string]any{"steps": steps}); put.Code != http.StatusNoContent {
		t.Fatalf("seed %d steps: %d: %s", maxBlueprintSteps, put.Code, put.Body.String())
	}
	source, _ := createWrappedBlueprint(t, s, "OneMore")

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
		"source_blueprint_id": source,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("merge over cap: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	// Both blueprints untouched — host still at the cap, source not retired.
	hs := listBlueprintSteps(t, s, host)
	if len(hs) != maxBlueprintSteps {
		t.Fatalf("host steps after rejected merge = %d, want %d (untouched)", len(hs), maxBlueprintSteps)
	}
	if !blueprintExists(t, s, source) {
		t.Fatal("source blueprint was retired by a rejected merge")
	}
}

func TestBlueprintSplit_RequiresIndex(t *testing.T) {
	s := newTestServer(t)
	p0 := createBarePrompt(t, s, "R0")
	p1 := createBarePrompt(t, s, "R1")
	bp := createBareBlueprint(t, s, "NeedsIndex")
	doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{
		"steps": []map[string]any{{"step_prompt_id": p0}, {"step_prompt_id": p1}},
	})
	// A missing at_step_index is a clean 400 "required", distinct from the 422
	// the ends return — a present 0 is a real index, not absence.
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/split", map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("split without at_step_index: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBlueprintMerge_SelfAndMissingRejected(t *testing.T) {
	s := newTestServer(t)
	host, _ := createWrappedBlueprint(t, s, "Solo")

	self := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
		"source_blueprint_id": host,
	})
	if self.Code != http.StatusUnprocessableEntity {
		t.Fatalf("merge into self: expected 422, got %d: %s", self.Code, self.Body.String())
	}

	missing := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
		"source_blueprint_id": "00000000-0000-0000-0000-0000000000ff",
	})
	if missing.Code != http.StatusNotFound {
		t.Fatalf("merge missing source: expected 404, got %d: %s", missing.Code, missing.Body.String())
	}
}

// --- C: atomic split -----------------------------------------------------

func TestBlueprintSplit_PartitionsIntoTwo(t *testing.T) {
	s := newTestServer(t)
	p0 := createBarePrompt(t, s, "P0")
	p1 := createBarePrompt(t, s, "P1")
	p2 := createBarePrompt(t, s, "P2")
	bp := createBareBlueprint(t, s, "Chain")
	if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{
		"steps": []map[string]any{{"step_prompt_id": p0}, {"step_prompt_id": p1}, {"step_prompt_id": p2}},
	}); put.Code != http.StatusNoContent {
		t.Fatalf("steps PUT: %d: %s", put.Code, put.Body.String())
	}

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/split", map[string]any{
		"at_step_index": 1,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("split: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Upstream   blueprintWithStepsView `json:"upstream"`
		Downstream blueprintWithStepsView `json:"downstream"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Upstream keeps [0,1) and its identity.
	if resp.Upstream.Blueprint["id"] != bp {
		t.Errorf("upstream id=%v, want %s", resp.Upstream.Blueprint["id"], bp)
	}
	if len(resp.Upstream.Steps) != 1 || resp.Upstream.Steps[0]["step_prompt_id"] != p0 {
		t.Fatalf("upstream steps=%+v, want [p0]", resp.Upstream.Steps)
	}
	// Downstream is a new blueprint with [1,N) re-densified 0-based.
	if resp.Downstream.Blueprint["id"] == bp || resp.Downstream.Blueprint["id"] == "" {
		t.Errorf("downstream must be a fresh blueprint, got id=%v", resp.Downstream.Blueprint["id"])
	}
	if len(resp.Downstream.Steps) != 2 || resp.Downstream.Steps[0]["step_prompt_id"] != p1 || resp.Downstream.Steps[1]["step_prompt_id"] != p2 {
		t.Fatalf("downstream steps=%+v, want [p1,p2]", resp.Downstream.Steps)
	}
	if resp.Downstream.Steps[0]["step_index"].(float64) != 0 || resp.Downstream.Steps[1]["step_index"].(float64) != 1 {
		t.Errorf("downstream indices not re-densified 0-based: %+v", resp.Downstream.Steps)
	}
	// Downstream name defaults to its new step-0 prompt (P1).
	if resp.Downstream.Blueprint["name"] != "P1" {
		t.Errorf("downstream name=%v, want P1 (defaults to its step-0 prompt)", resp.Downstream.Blueprint["name"])
	}
}

func TestBlueprintSplit_AtEndsRejected(t *testing.T) {
	s := newTestServer(t)
	p0 := createBarePrompt(t, s, "Q0")
	p1 := createBarePrompt(t, s, "Q1")
	bp := createBareBlueprint(t, s, "Pair")
	doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{
		"steps": []map[string]any{{"step_prompt_id": p0}, {"step_prompt_id": p1}},
	})
	for _, k := range []int{0, 2} {
		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/split", map[string]any{"at_step_index": k})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("split at %d: expected 422, got %d: %s", k, rec.Code, rec.Body.String())
		}
	}
	// The blueprint is untouched after the rejected splits.
	got := listBlueprintSteps(t, s, bp)
	if len(got) != 2 {
		t.Fatalf("steps after rejected splits = %d, want 2 (untouched)", len(got))
	}
}

// --- A: ≤1 event per blueprint surfaces as a clean 409 -------------------

func TestEventHandlerCreate_SecondTriggerConflicts(t *testing.T) {
	s := newTestServer(t)
	seedBlueprintForTrigger(t, s, "bp-one-trigger")
	first := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":               "github:pr:ci_check_failed",
		"blueprint_id":             "bp-one-trigger",
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.0,
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("first trigger: %d: %s", first.Code, first.Body.String())
	}
	// A second trigger on the same blueprint (even on a different event) is a
	// clean 409, not a raw 500 from the partial-unique index.
	second := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":               "github:pr:review_changes_requested",
		"blueprint_id":             "bp-one-trigger",
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.0,
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("second trigger: expected 409, got %d: %s", second.Code, second.Body.String())
	}
}

func TestEventHandlerPromote_OntoTriggeredBlueprintConflicts(t *testing.T) {
	s := newTestServer(t)
	seedBlueprintForTrigger(t, s, "bp-promote-conflict")
	if tr := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":               "github:pr:ci_check_failed",
		"blueprint_id":             "bp-promote-conflict",
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.0,
	}); tr.Code != http.StatusCreated {
		t.Fatalf("seed trigger: %d: %s", tr.Code, tr.Body.String())
	}
	rule := doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":       "github:pr:review_changes_requested",
		"name":             "to-promote",
		"default_priority": 0.5,
		"sort_order":       0,
	})
	if rule.Code != http.StatusCreated {
		t.Fatalf("create rule: %d: %s", rule.Code, rule.Body.String())
	}
	var r struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rule.Body.Bytes(), &r)

	prom := doJSON(t, s, http.MethodPost, "/api/event-handlers/"+r.ID+"/promote", map[string]any{
		"blueprint_id":             "bp-promote-conflict",
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.0,
	})
	if prom.Code != http.StatusConflict {
		t.Fatalf("promote onto triggered blueprint: expected 409, got %d: %s", prom.Code, prom.Body.String())
	}
}

// The step list with no blueprint_ids returns every blueprint's steps in one
// read, grouped by blueprint_id and excluding retired (merged-away)
// blueprints — the canvas's bulk fetch that replaced the per-blueprint N+1.
func TestBlueprintStepsAll_BulkGroupedAndExcludesRetired(t *testing.T) {
	s := newTestServer(t)
	host, hp := createWrappedBlueprint(t, s, "Alpha")
	source, sp := createWrappedBlueprint(t, s, "Beta")

	// Merge Beta onto Alpha's tail → Alpha = [hp, sp], Beta retired.
	if rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
		"source_blueprint_id": source,
	}); rec.Code != http.StatusOK {
		t.Fatalf("merge: %d: %s", rec.Code, rec.Body.String())
	}

	byBp := map[string][]string{}
	for bp, steps := range listAllBlueprintSteps(t, s) {
		for _, st := range steps {
			byBp[bp] = append(byBp[bp], st["step_prompt_id"].(string))
		}
	}
	if len(byBp[host]) != 2 || byBp[host][0] != hp || byBp[host][1] != sp {
		t.Errorf("Alpha steps = %v, want [%s %s]", byBp[host], hp, sp)
	}
	if _, present := byBp[source]; present {
		t.Errorf("retired blueprint %s leaked steps into the bulk read: %v", source, byBp[source])
	}
}
