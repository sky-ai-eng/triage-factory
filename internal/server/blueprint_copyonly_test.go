package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// postJSON is a thin helper that POSTs/PUTs/DELETEs JSON and returns the
// recorder; the copy-only tests below assert on status + decoded body.

// --- B: auto-wrap "New Prompt" -------------------------------------------

func TestBlueprintCreate_AutoWrapFirstPrompt(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints", map[string]any{
		"first_prompt": map[string]any{"name": "Reviewer", "body": "review the PR", "model": domain.ModelAliasSonnet},
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
	steps := listBlueprintSteps(t, s, created.ID)
	if len(steps) != 1 || steps[0]["step_prompt_id"] != created.FirstPromptID {
		t.Fatalf("steps=%+v; want one step on %s", steps, created.FirstPromptID)
	}

	// And the prompt is visible in the prompts list.
	prompts := decodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/prompts/list", map[string]any{}))
	found := false
	for _, p := range prompts.Items {
		if p["id"] == created.FirstPromptID {
			found = true
		}
	}
	if !found {
		t.Fatalf("auto-wrapped prompt %s not in the prompts list", created.FirstPromptID)
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

	// Deleting a prompt that's part of a multi-step blueprint no longer 409s —
	// the chain fragments per the split rule. Deleting the entry (p1, head) drops
	// it and leaves the remaining step as a new, trigger-less blueprint.
	code, orphaned := deletePrompt(t, s, p1)
	if code != http.StatusOK {
		t.Fatalf("delete step-of-composition: expected 200 (split), got %d", code)
	}
	if !orphaned {
		t.Errorf("head delete should report an orphaned downstream blueprint")
	}
	// p1 is gone; p2 survives in a fresh blueprint (not the retired original).
	if g := doJSON(t, s, http.MethodGet, "/api/prompts/"+p1, nil); g.Code != http.StatusNotFound {
		t.Fatalf("deleted prompt GET: expected 404, got %d", g.Code)
	}
	if down := ownerBlueprintOf(t, s, p2); down == "" || down == bp {
		t.Fatalf("p2 should live in a new blueprint after the head split, got owner %q (original %s)", down, bp)
	}
	// The original blueprint is retired (request-facing steps 404).
	if blueprintExists(t, s, bp) {
		t.Fatal("retired blueprint steps GET: the blueprint is still readable")
	}
}

// --- C2: multi-step prompt-delete split semantics ------------------------

func TestPromptDelete_TailKeepsTriggerAndId(t *testing.T) {
	s := newTestServer(t)
	bp, ps := createChainBlueprint(t, s, "TailChain", 3)
	attachTrigger(t, s, bp, "github:pr:ci_check_failed")

	code, orphaned := deletePrompt(t, s, ps[2]) // tail
	if code != http.StatusOK {
		t.Fatalf("tail delete: expected 200, got %d", code)
	}
	if orphaned {
		t.Errorf("tail delete should not orphan a downstream blueprint")
	}
	// The blueprint keeps its id, [p0,p1] densely, and its trigger.
	if got := stepPromptIDs(t, s, bp); !equalStrings(got, ps[:2]) {
		t.Errorf("tail-deleted blueprint steps = %v, want %v", got, ps[:2])
	}
	if n := len(triggersForBlueprint(t, s, bp)); n != 1 {
		t.Errorf("blueprint trigger count after tail delete = %d, want 1 (retained)", n)
	}
	if g := doJSON(t, s, http.MethodGet, "/api/prompts/"+ps[2], nil); g.Code != http.StatusNotFound {
		t.Fatalf("deleted tail prompt GET: expected 404, got %d", g.Code)
	}
}

func TestPromptDelete_HeadDetachesTrigger(t *testing.T) {
	s := newTestServer(t)
	bp, ps := createChainBlueprint(t, s, "HeadChain", 3)
	attachTrigger(t, s, bp, "github:pr:ci_check_failed")

	code, orphaned := deletePrompt(t, s, ps[0]) // head
	if code != http.StatusOK {
		t.Fatalf("head delete: expected 200, got %d", code)
	}
	if !orphaned {
		t.Errorf("head delete should report an orphaned downstream blueprint")
	}
	// The original blueprint_id retires with the entry prompt; its (user) trigger
	// is hard-deleted, not left dangling on a soft-deleted blueprint.
	if blueprintExists(t, s, bp) {
		t.Fatal("retired blueprint steps GET: the blueprint is still readable")
	}
	if n := len(triggersForBlueprint(t, s, bp)); n != 0 {
		t.Errorf("original blueprint trigger count after head delete = %d, want 0 (detached)", n)
	}
	// The remaining steps are a new, trigger-less blueprint, re-densified.
	down := ownerBlueprintOf(t, s, ps[1])
	if down == "" || down == bp {
		t.Fatalf("p1 owner after head split = %q, want a fresh blueprint (original %s)", down, bp)
	}
	if got := stepPromptIDs(t, s, down); !equalStrings(got, ps[1:]) {
		t.Errorf("downstream steps = %v, want %v", got, ps[1:])
	}
	if n := len(triggersForBlueprint(t, s, down)); n != 0 {
		t.Errorf("downstream blueprint trigger count = %d, want 0 (trigger-less)", n)
	}
	if g := doJSON(t, s, http.MethodGet, "/api/prompts/"+ps[0], nil); g.Code != http.StatusNotFound {
		t.Fatalf("deleted head prompt GET: expected 404, got %d", g.Code)
	}
}

func TestPromptDelete_MidSplitsDownstreamTriggerless(t *testing.T) {
	s := newTestServer(t)
	bp, ps := createChainBlueprint(t, s, "MidChain", 4)
	attachTrigger(t, s, bp, "github:pr:ci_check_failed")

	code, orphaned := deletePrompt(t, s, ps[1]) // mid (index 1)
	if code != http.StatusOK {
		t.Fatalf("mid delete: expected 200, got %d", code)
	}
	if !orphaned {
		t.Errorf("mid delete should report an orphaned downstream blueprint")
	}
	// Upstream keeps its id, [p0], and its trigger.
	if got := stepPromptIDs(t, s, bp); !equalStrings(got, ps[:1]) {
		t.Errorf("upstream steps after mid delete = %v, want %v", got, ps[:1])
	}
	if n := len(triggersForBlueprint(t, s, bp)); n != 1 {
		t.Errorf("upstream trigger count after mid delete = %d, want 1 (retained)", n)
	}
	// Downstream is a new trigger-less blueprint holding [p2,p3], re-densified.
	down := ownerBlueprintOf(t, s, ps[2])
	if down == "" || down == bp {
		t.Fatalf("p2 owner after mid split = %q, want a fresh blueprint (original %s)", down, bp)
	}
	if got := stepPromptIDs(t, s, down); !equalStrings(got, ps[2:]) {
		t.Errorf("downstream steps = %v, want %v", got, ps[2:])
	}
	if n := len(triggersForBlueprint(t, s, down)); n != 0 {
		t.Errorf("downstream trigger count = %d, want 0 (trigger-less)", n)
	}
	if g := doJSON(t, s, http.MethodGet, "/api/prompts/"+ps[1], nil); g.Code != http.StatusNotFound {
		t.Fatalf("deleted mid prompt GET: expected 404, got %d", g.Code)
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

// createChainBlueprint builds a bare blueprint stepping through n fresh bare
// prompts and returns the blueprint id + the ordered step prompt ids.
func createChainBlueprint(t *testing.T, s *Server, name string, n int) (blueprintID string, promptIDs []string) {
	t.Helper()
	bp := createBareBlueprint(t, s, name)
	steps := make([]map[string]any, 0, n)
	promptIDs = make([]string, 0, n)
	for i := 0; i < n; i++ {
		p := createBarePrompt(t, s, fmt.Sprintf("%s-%d", name, i))
		promptIDs = append(promptIDs, p)
		steps = append(steps, map[string]any{"step_prompt_id": p})
	}
	if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{"steps": steps}); put.Code != http.StatusNoContent {
		t.Fatalf("createChainBlueprint %s steps PUT: %d: %s", name, put.Code, put.Body.String())
	}
	return bp, promptIDs
}

// attachTrigger wires a user trigger to blueprintID for eventType and returns
// the trigger id.
func attachTrigger(t *testing.T, s *Server, blueprintID, eventType string) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":               eventType,
		"blueprint_id":             blueprintID,
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.0,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("attachTrigger to %s: %d: %s", blueprintID, rec.Code, rec.Body.String())
	}
	var tr struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tr)
	return tr.ID
}

// triggersForBlueprint returns the trigger handlers bound to blueprintID.
func triggersForBlueprint(t *testing.T, s *Server, blueprintID string) []map[string]any {
	t.Helper()
	all := listEventHandlers(t, s, "trigger")
	var out []map[string]any
	for _, h := range all {
		if h["blueprint_id"] == blueprintID {
			out = append(out, h)
		}
	}
	return out
}

// stepPromptIDs returns the ordered step prompt ids of a request-visible
// blueprint.
func stepPromptIDs(t *testing.T, s *Server, blueprintID string) []string {
	t.Helper()
	steps := listBlueprintSteps(t, s, blueprintID)
	out := make([]string, len(steps))
	for i, st := range steps {
		out[i], _ = st["step_prompt_id"].(string)
	}
	return out
}

// ownerBlueprintOf resolves which request-visible blueprint holds promptID as a
// step via the bulk steps read (so a freshly-minted downstream blueprint is
// locatable without knowing its id).
func ownerBlueprintOf(t *testing.T, s *Server, promptID string) string {
	t.Helper()
	for bp, steps := range listAllBlueprintSteps(t, s) {
		for _, st := range steps {
			if st["step_prompt_id"] == promptID {
				return bp
			}
		}
	}
	return ""
}

// deletePrompt issues DELETE /api/prompts/{id} and decodes the status code +
// the orphaned_blueprint signal.
func deletePrompt(t *testing.T, s *Server, promptID string) (code int, orphaned bool) {
	t.Helper()
	rec := doJSON(t, s, http.MethodDelete, "/api/prompts/"+promptID, nil)
	var body struct {
		Status            string `json:"status"`
		OrphanedBlueprint bool   `json:"orphaned_blueprint"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body.OrphanedBlueprint
}

// equalStrings reports whether two string slices are element-wise equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
