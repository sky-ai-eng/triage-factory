package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		rec := putBlueprintSteps(t, s, bp, []map[string]any{{"step_prompt_id": fixtureUUID("no-such-prompt")}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		httpxtest.AssertFirstError(t, rec, httpx.ReasonNotFound, "steps")
	})

	t.Run("prompt owned by another blueprint", func(t *testing.T) {
		owner, taken := createWrappedBlueprint(t, s, "Owner")
		thief := createBareBlueprint(t, s, "Thief")
		rec := putBlueprintSteps(t, s, thief, []map[string]any{{"step_prompt_id": taken}})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		// A conflict over who owns the prompt, not a team-boundary fault: both
		// blueprints are on the same team here.
		httpxtest.AssertFirstError(t, rec, httpx.ReasonConflict, "steps")

		// The owner re-saving its own step list is not a reuse.
		if ok := putBlueprintSteps(t, s, owner, []map[string]any{{"step_prompt_id": taken}}); ok.Code != http.StatusOK {
			t.Fatalf("re-saving own steps: expected 200, got %d: %s", ok.Code, ok.Body.String())
		}
	})

	t.Run("list that leaves out a step", func(t *testing.T) {
		bp, steps := createChainBlueprint(t, s, "Kept", 3)
		rec := putBlueprintSteps(t, s, bp, []map[string]any{{"step_prompt_id": steps[2]}, {"step_prompt_id": steps[0]}})
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
		if ok := putBlueprintSteps(t, s, bp, put); ok.Code != http.StatusOK {
			t.Fatalf("PUT naming every step = %d, want 200; body=%s", ok.Code, ok.Body.String())
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

	t.Run("version", func(t *testing.T) {
		bp, steps := createChainBlueprint(t, s, "Unversioned", 1)
		for _, c := range []struct {
			name    string
			body    map[string]any
			reasons map[string]string
		}{
			{"absent", map[string]any{"steps": []map[string]any{{"step_prompt_id": steps[0]}}},
				map[string]string{"version": httpx.ReasonMissingField}},
			{"zero", map[string]any{"steps": []map[string]any{{"step_prompt_id": steps[0]}}, "version": 0},
				map[string]string{"version": httpx.ReasonInvalidField}},
			// Both shape faults are reported, not the first.
			{"absent beside a step with no prompt", map[string]any{"steps": []map[string]any{{"brief": "no prompt"}}},
				map[string]string{"version": httpx.ReasonMissingField, "steps": httpx.ReasonMissingField}},
		} {
			rec := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", c.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400; body=%s", c.name, rec.Code, rec.Body.String())
				continue
			}
			got := map[string]string{}
			for _, item := range decodeErrorItems(t, rec) {
				got[item.Field] = item.Reason
			}
			if len(got) != len(c.reasons) {
				t.Errorf("%s: errors = %v, want %v", c.name, got, c.reasons)
			}
			for field, reason := range c.reasons {
				if got[field] != reason {
					t.Errorf("%s: %s error = %q, want %q", c.name, field, got[field], reason)
				}
			}
		}
	})

	t.Run("a PUT that names an older version", func(t *testing.T) {
		bp, steps := createChainBlueprint(t, s, "Raced", 2)
		read := listBlueprintSteps(t, s, bp)
		read0, _ := read[0]["blueprint_version"].(float64)
		if v := blueprintVersion(t, s, bp); int64(read0) != v {
			t.Fatalf("the step list's blueprint_version = %v, want the blueprint's version %d", read[0]["blueprint_version"], v)
		}
		send := func(brief string, order []string) *httptest.ResponseRecorder {
			put := make([]map[string]any, len(order))
			for i, p := range order {
				put[i] = map[string]any{"step_prompt_id": p, "brief": brief}
			}
			return doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{"steps": put, "version": read0})
		}

		// Two callers read the same steps. The first PUT lands, and answers
		// the version it wrote and the steps it wrote.
		first := send("first", []string{steps[1], steps[0]})
		if first.Code != http.StatusOK {
			t.Fatalf("first PUT = %d, want 200; body=%s", first.Code, first.Body.String())
		}
		var wrote struct {
			Blueprint struct {
				Version int64 `json:"version"`
			} `json:"blueprint"`
			Steps []struct {
				StepPromptID string `json:"step_prompt_id"`
				Brief        string `json:"brief"`
			} `json:"steps"`
		}
		if err := json.Unmarshal(first.Body.Bytes(), &wrote); err != nil {
			t.Fatalf("decode the first PUT's answer: %v", err)
		}
		if v := blueprintVersion(t, s, bp); wrote.Blueprint.Version != v || v <= int64(read0) {
			t.Errorf("first PUT answered version %d; the blueprint is at %d, from %v", wrote.Blueprint.Version, v, read0)
		}
		if len(wrote.Steps) != 2 || wrote.Steps[0].StepPromptID != steps[1] || wrote.Steps[0].Brief != "first" {
			t.Errorf("first PUT answered steps %+v, want the ones it wrote", wrote.Steps)
		}

		// The second names the same steps, so only the version tells it was
		// decided against steps the first has since changed.
		second := send("second", []string{steps[0], steps[1]})
		if second.Code != http.StatusConflict {
			t.Fatalf("second PUT = %d, want 409; body=%s", second.Code, second.Body.String())
		}
		httpxtest.AssertFirstError(t, second, httpx.ReasonVersionConflict, "version")
		if got := stepPromptIDs(t, s, bp); !slices.Equal(got, []string{steps[1], steps[0]}) {
			t.Errorf("steps after the refused PUT = %v, want the first PUT's", got)
		}
		for i, st := range listBlueprintSteps(t, s, bp) {
			if st["brief"] != "first" {
				t.Errorf("step %d brief = %v, want the first PUT's", i, st["brief"])
			}
		}
	})
}
