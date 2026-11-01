package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// Blueprint duplication: a flat set of prompt ids deep-copies into new
// trigger-less blueprint(s), one per contiguous run, in one transaction, with
// the originals untouched.

func TestBlueprintDuplicate_FullBlueprintAtomic(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	src, p := createChainBlueprint(t, s, "Src", 3)
	attachTrigger(t, s, src, "github:pr:ci_check_failed")

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
	copyID, _ := copyBP.Blueprint["id"].(string)
	if copyID == src || copyID == "" {
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

	// Originals untouched: the source keeps its 3 steps with the original
	// prompt ids, and its trigger; the copy carries none.
	got := listBlueprintSteps(t, s, src)
	if len(got) != 3 || got[0]["step_prompt_id"] != p[0] || got[2]["step_prompt_id"] != p[2] {
		t.Fatalf("source mutated by duplicate: %+v", got)
	}
	if n := len(triggersForBlueprint(t, s, src)); n != 1 {
		t.Errorf("source blueprint triggers after duplication = %d, want 1", n)
	}
	if n := len(triggersForBlueprint(t, s, copyID)); n != 0 {
		t.Errorf("duplicated blueprint carried %d triggers; copies must always be trigger-less", n)
	}
}

func TestBlueprintDuplicate_NonContiguousYieldsSeparateBlueprints(t *testing.T) {
	s := newTestServer(t)
	_, p := createChainBlueprint(t, s, "Gapped", 3)

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

// An empty and an unknown prompt_ids are different fault classes, each with
// its own status and reason.
func TestBlueprintDuplicate_EmptyAndUnknownRejected(t *testing.T) {
	s := newTestServer(t)

	empty := doJSON(t, s, http.MethodPost, "/api/blueprints/duplicate", map[string]any{
		"prompt_ids": []string{},
	})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt_ids: expected 400, got %d: %s", empty.Code, empty.Body.String())
	}
	httpxtest.AssertFirstError(t, empty, httpx.ReasonMissingField, "prompt_ids")

	unknown := doJSON(t, s, http.MethodPost, "/api/blueprints/duplicate", map[string]any{
		"prompt_ids": []string{"00000000-0000-0000-0000-0000000000ff"},
	})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown prompt id: expected 404, got %d: %s", unknown.Code, unknown.Body.String())
	}
	httpxtest.AssertFirstError(t, unknown, httpx.ReasonNotFound, "prompt_ids")
}
