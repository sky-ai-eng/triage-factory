package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Blueprint duplication: the atomic deep-copy endpoint. A flat set of prompt
// ids deep-copies into new trigger-less blueprint(s) following the induced-
// contiguous-runs rule, in one transaction, originals untouched.

// seedSourceBlueprint builds a multi-step source blueprint from bare prompts and
// returns the blueprint id + its ordered prompt ids.
func seedSourceBlueprint(t *testing.T, s *Server, name string, promptNames ...string) (string, []string) {
	t.Helper()
	ids := make([]string, len(promptNames))
	steps := make([]map[string]any, len(promptNames))
	for i, n := range promptNames {
		ids[i] = createBarePrompt(t, s, n)
		steps[i] = map[string]any{"step_prompt_id": ids[i]}
	}
	bp := createBareBlueprint(t, s, name)
	if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{"steps": steps}); put.Code != http.StatusNoContent {
		t.Fatalf("seed steps for %s: %d: %s", name, put.Code, put.Body.String())
	}
	return bp, ids
}

func TestBlueprintDuplicate_FullBlueprintAtomic(t *testing.T) {
	s := newTestServer(t)
	src, p := seedSourceBlueprint(t, s, "Src", "P0", "P1", "P2")

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/duplicate", map[string]any{
		"prompt_ids": []string{p[0], p[1], p[2]},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("duplicate full: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var out []blueprintWithStepsView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("full-blueprint duplicate returned %d blueprints, want 1", len(out))
	}
	copyBP := out[0]
	if copyBP.Blueprint["id"] == src || copyBP.Blueprint["id"] == "" {
		t.Errorf("copy must be a fresh blueprint, got id=%v", copyBP.Blueprint["id"])
	}
	if copyBP.Blueprint["name"] != "Src (copy)" {
		t.Errorf("full clone name=%v, want 'Src (copy)'", copyBP.Blueprint["name"])
	}
	if len(copyBP.Steps) != 3 {
		t.Fatalf("copy steps=%d, want 3: %+v", len(copyBP.Steps), copyBP.Steps)
	}
	for i, st := range copyBP.Steps {
		if st["step_index"].(float64) != float64(i) {
			t.Errorf("copy step %d index=%v, want %d", i, st["step_index"], i)
		}
		// Each step is a fresh prompt row, not the source's.
		for _, orig := range p {
			if st["step_prompt_id"] == orig {
				t.Errorf("copy step %d reuses source prompt %v; want a fresh copy", i, orig)
			}
		}
	}

	// Originals untouched: source blueprint still has its 3 steps with the
	// original prompt ids.
	got := listBlueprintSteps(t, s, src)
	if len(got) != 3 || got[0]["step_prompt_id"] != p[0] || got[2]["step_prompt_id"] != p[2] {
		t.Fatalf("source mutated by duplicate: %+v", got)
	}
}

func TestBlueprintDuplicate_NonContiguousYieldsSeparateBlueprints(t *testing.T) {
	s := newTestServer(t)
	_, p := seedSourceBlueprint(t, s, "Gapped", "G0", "G1", "G2")

	// Select 0 and 2 (gap at 1) → two separate 1-step blueprints, no edge.
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/duplicate", map[string]any{
		"prompt_ids": []string{p[0], p[2]},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("duplicate non-contiguous: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var out []blueprintWithStepsView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("non-contiguous duplicate returned %d blueprints, want 2", len(out))
	}
	for _, bp := range out {
		if len(bp.Steps) != 1 || bp.Steps[0]["step_index"].(float64) != 0 {
			t.Fatalf("each copy must be a single 0-indexed step, got %+v", bp.Steps)
		}
	}
}

func TestBlueprintDuplicate_CopyIsTriggerlessSourceUntouched(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	// A triggered source blueprint (1-step). Duplicating its prompt yields a
	// trigger-less copy; the source keeps its trigger.
	src, p := createWrappedBlueprint(t, s, "Triggered")
	if tr := doJSON(t, s, http.MethodPost, "/api/event-handlers/triggers", map[string]any{
		"event_type":               "github:pr:ci_check_failed",
		"blueprint_id":             src,
		"breaker_threshold":        3,
		"min_autonomy_suitability": 0.0,
	}); tr.Code != http.StatusCreated {
		t.Fatalf("seed trigger on source: %d: %s", tr.Code, tr.Body.String())
	}

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/duplicate", map[string]any{
		"prompt_ids": []string{p},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("duplicate triggered: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var out []blueprintWithStepsView
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("duplicate produced %d blueprints, want 1", len(out))
	}
	copyID, _ := out[0].Blueprint["id"].(string)

	// The source's trigger is intact; the copy has none.
	hs := listEventHandlers(t, s, "")
	var srcTriggered, copyTriggered bool
	for _, h := range hs {
		if h["kind"] != "trigger" {
			continue
		}
		switch h["blueprint_id"] {
		case src:
			srcTriggered = true
		case copyID:
			copyTriggered = true
		}
	}
	if !srcTriggered {
		t.Error("source blueprint lost its trigger after duplication")
	}
	if copyTriggered {
		t.Error("duplicated blueprint carried a trigger; copies must always be trigger-less")
	}
}

func TestBlueprintDuplicate_EmptyAndUnknownRejected(t *testing.T) {
	s := newTestServer(t)

	empty := doJSON(t, s, http.MethodPost, "/api/blueprints/duplicate", map[string]any{
		"prompt_ids": []string{},
	})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt_ids: expected 400, got %d: %s", empty.Code, empty.Body.String())
	}

	unknown := doJSON(t, s, http.MethodPost, "/api/blueprints/duplicate", map[string]any{
		"prompt_ids": []string{"00000000-0000-0000-0000-0000000000ff"},
	})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown prompt id: expected 404, got %d: %s", unknown.Code, unknown.Body.String())
	}
}
