package server

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// TestBlueprintStepsPut_FaultReasons pins the reason on each refusal a
// steps-PUT raises, since the status alone cannot tell a missing prompt from
// one another blueprint owns.
func TestBlueprintStepsPut_FaultReasons(t *testing.T) {
	s := newTestServer(t)

	t.Run("non-existent prompt", func(t *testing.T) {
		bp := createBareBlueprint(t, s, "MissingRef")
		rec := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{
			"steps": []map[string]any{{"step_prompt_id": fixtureUUID("no-such-prompt")}},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonNotFound, "steps")
	})

	t.Run("prompt owned by another blueprint", func(t *testing.T) {
		owner, taken := createWrappedBlueprint(t, s, "Owner")
		thief := createBareBlueprint(t, s, "Thief")
		rec := doJSON(t, s, http.MethodPut, "/api/blueprints/"+thief+"/steps", map[string]any{
			"steps": []map[string]any{{"step_prompt_id": taken}},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		// A conflict over who owns the prompt, not a team-boundary fault: both
		// blueprints are on the same team here.
		httpxtest.AssertFirstError(t, rec, httpx.ReasonConflict, "steps")

		// The owner re-saving its own step list is not a reuse.
		if ok := doJSON(t, s, http.MethodPut, "/api/blueprints/"+owner+"/steps", map[string]any{
			"steps": []map[string]any{{"step_prompt_id": taken}},
		}); ok.Code != http.StatusNoContent {
			t.Fatalf("re-saving own steps: expected 204, got %d: %s", ok.Code, ok.Body.String())
		}
	})

	t.Run("list that leaves out a step", func(t *testing.T) {
		bp, steps := createChainBlueprint(t, s, "Kept", 3)
		rec := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{
			"steps": []map[string]any{{"step_prompt_id": steps[2]}, {"step_prompt_id": steps[0]}},
		})
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonConflict, "steps")
		if got := stepPromptIDs(t, s, bp); !slices.Equal(got, steps) {
			t.Errorf("steps after the refused PUT = %v, want %v", got, steps)
		}

		// A list that names every step may reorder them, change their
		// briefs, and add a prompt.
		added := createBarePrompt(t, s, "Kept-added")
		want := []string{steps[2], steps[0], added, steps[1]}
		put := make([]map[string]any, len(want))
		for i, p := range want {
			put[i] = map[string]any{"step_prompt_id": p, "brief": "edited"}
		}
		if ok := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{"steps": put}); ok.Code != http.StatusNoContent {
			t.Fatalf("PUT naming every step = %d, want 204; body=%s", ok.Code, ok.Body.String())
		}
		if got := stepPromptIDs(t, s, bp); !slices.Equal(got, want) {
			t.Errorf("steps after the PUT naming every step = %v, want %v", got, want)
		}
		for i, st := range listBlueprintSteps(t, s, bp) {
			if st["brief"] != "edited" {
				t.Errorf("step %d brief = %v, want %q", i, st["brief"], "edited")
			}
		}
	})
}
