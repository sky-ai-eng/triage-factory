package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// postJSON is a thin helper that POSTs/PUTs/DELETEs JSON and returns the
// recorder; the copy-only tests below assert on status + decoded body.

// --- B: auto-wrap "New Prompt" -------------------------------------------

func TestBlueprintCreate_AutoWrapFirstPrompt(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints", map[string]any{
		"first_prompt": map[string]any{"name": "Reviewer", "body": "review the PR", "model": "sonnet"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		FirstPromptID string `json:"first_prompt_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == "" || created.FirstPromptID == "" {
		t.Fatalf("expected blueprint id + first_prompt_id, got %+v", created)
	}
	// Name defaults to the prompt's name.
	if created.Name != "Reviewer" {
		t.Errorf("blueprint name=%q want Reviewer (defaults to the prompt name)", created.Name)
	}

	// The blueprint has exactly one step pointing at the new prompt (the node
	// the canvas renders, wireable to an event).
	stepsRec := doJSON(t, s, http.MethodGet, "/api/blueprints/"+created.ID+"/steps", nil)
	if stepsRec.Code != http.StatusOK {
		t.Fatalf("steps GET: %d: %s", stepsRec.Code, stepsRec.Body.String())
	}
	var steps []map[string]any
	if err := json.Unmarshal(stepsRec.Body.Bytes(), &steps); err != nil {
		t.Fatalf("decode steps: %v", err)
	}
	if len(steps) != 1 || steps[0]["step_prompt_id"] != created.FirstPromptID {
		t.Fatalf("steps=%+v; want one step on %s", steps, created.FirstPromptID)
	}

	// And the prompt is visible in the prompts list.
	listRec := doJSON(t, s, http.MethodGet, "/api/prompts", nil)
	var prompts []map[string]any
	_ = json.Unmarshal(listRec.Body.Bytes(), &prompts)
	found := false
	for _, p := range prompts {
		if p["id"] == created.FirstPromptID {
			found = true
		}
	}
	if !found {
		t.Fatalf("auto-wrapped prompt %s not in /api/prompts", created.FirstPromptID)
	}
}

func TestBlueprintCreate_AutoWrapRequiresPromptBody(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints", map[string]any{
		"first_prompt": map[string]any{"name": "NoBody"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing first_prompt.body, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBlueprintCreate_AutoWrapRequiresPromptName(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints", map[string]any{
		"first_prompt": map[string]any{"body": "do something"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing first_prompt.name, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBlueprintCreate_BareStillValid(t *testing.T) {
	// first_prompt is optional — a bare blueprint (name only) still works.
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints", map[string]any{"name": "Bare"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID            string `json:"id"`
		FirstPromptID string `json:"first_prompt_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID == "" || created.FirstPromptID != "" {
		t.Fatalf("bare create: got %+v; want an id and no first_prompt_id", created)
	}
}

// --- A: copy-only 422 on cross-blueprint reuse ---------------------------

func TestBlueprintStepsPut_RejectsCrossBlueprintReuse(t *testing.T) {
	s := newTestServer(t)
	// Two auto-wrapped blueprints, each owning its own prompt.
	bp1, p1 := createWrappedBlueprint(t, s, "One")
	bp2, _ := createWrappedBlueprint(t, s, "Two")

	// Try to make bp2 step through bp1's prompt → 422, not a raw 500.
	rec := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp2+"/steps", map[string]any{
		"steps": []map[string]any{{"step_prompt_id": p1}},
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}

	// Re-saving bp1's own step list (owner == self) still works.
	ok := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp1+"/steps", map[string]any{
		"steps": []map[string]any{{"step_prompt_id": p1}},
	})
	if ok.Code != http.StatusNoContent {
		t.Fatalf("re-saving own steps: expected 204, got %d: %s", ok.Code, ok.Body.String())
	}
}

// --- C: prompt-delete pairing --------------------------------------------

func TestPromptDelete_SoleOwnerPairSoftDeletesBoth(t *testing.T) {
	s := newTestServer(t)
	bp, p := createWrappedBlueprint(t, s, "Solo")

	del := doJSON(t, s, http.MethodDelete, "/api/prompts/"+p, nil)
	if del.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d: %s", del.Code, del.Body.String())
	}
	// Prompt is gone from request-facing reads.
	if g := doJSON(t, s, http.MethodGet, "/api/prompts/"+p, nil); g.Code != http.StatusNotFound {
		t.Fatalf("GET deleted prompt: expected 404, got %d", g.Code)
	}
	// And its sole-owner blueprint vanished too.
	list := doJSON(t, s, http.MethodGet, "/api/blueprints", nil)
	var bps []map[string]any
	_ = json.Unmarshal(list.Body.Bytes(), &bps)
	for _, b := range bps {
		if b["id"] == bp {
			t.Fatalf("sole-owner blueprint %s still listed after pairing delete", bp)
		}
	}
}

func TestPromptDelete_MultiStepBlueprintConflicts(t *testing.T) {
	s := newTestServer(t)
	// Bare prompts (POST /api/prompts) aren't auto-wrapped, so a bare blueprint
	// can step through two of them — a real 2-step composition.
	p1 := createBarePrompt(t, s, "Step1")
	p2 := createBarePrompt(t, s, "Step2")
	bp := createBareBlueprint(t, s, "Composition")
	put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{
		"steps": []map[string]any{{"step_prompt_id": p1}, {"step_prompt_id": p2}},
	})
	if put.Code != http.StatusNoContent {
		t.Fatalf("steps PUT: expected 204, got %d: %s", put.Code, put.Body.String())
	}

	// Deleting a prompt that's part of a multi-step blueprint 409s.
	del := doJSON(t, s, http.MethodDelete, "/api/prompts/"+p1, nil)
	if del.Code != http.StatusConflict {
		t.Fatalf("delete step-of-composition: expected 409, got %d: %s", del.Code, del.Body.String())
	}
}

// --- helpers -------------------------------------------------------------

func createWrappedBlueprint(t *testing.T, s *Server, name string) (blueprintID, promptID string) {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints", map[string]any{
		"first_prompt": map[string]any{"name": name, "body": "do " + name},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("createWrappedBlueprint %s: %d: %s", name, rec.Code, rec.Body.String())
	}
	var created struct {
		ID            string `json:"id"`
		FirstPromptID string `json:"first_prompt_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	return created.ID, created.FirstPromptID
}

func createBarePrompt(t *testing.T, s *Server, name string) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/prompts", map[string]any{"name": name, "body": "b"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("createBarePrompt %s: %d: %s", name, rec.Code, rec.Body.String())
	}
	var p struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	return p.ID
}

func createBareBlueprint(t *testing.T, s *Server, name string) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints", map[string]any{"name": name})
	if rec.Code != http.StatusCreated {
		t.Fatalf("createBareBlueprint %s: %d: %s", name, rec.Code, rec.Body.String())
	}
	var b struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.ID
}
