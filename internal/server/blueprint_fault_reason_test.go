package server

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
)

// The composition endpoints refuse for several distinct causes and used to
// stamp CROSS_TEAM_REF on all of them, because the reason was hardcoded at the
// single write site rather than named where the fault was raised. Status alone
// doesn't separate them — most are 422 — so a headless caller had no way to
// tell "that prompt doesn't exist" from "another blueprint already owns it".
// These tests pin the reason per cause, and the team-ownership arms pin that
// CROSS_TEAM_REF still means what its registry entry says.

// TestBlueprintStepsPut_FaultReasons covers the three refusals a steps-PUT can
// raise inside its transaction, all 422 and all previously CROSS_TEAM_REF.
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
		assertFirstError(t, rec, httpx.ReasonNotFound, "steps")
	})

	t.Run("prompt owned by another blueprint", func(t *testing.T) {
		_, taken := createWrappedBlueprint(t, s, "Owner")
		thief := createBareBlueprint(t, s, "Thief")
		rec := doJSON(t, s, http.MethodPut, "/api/blueprints/"+thief+"/steps", map[string]any{
			"steps": []map[string]any{{"step_prompt_id": taken}},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		// A conflict over who owns the prompt, not a team-boundary fault: both
		// blueprints are on the same team here.
		assertFirstError(t, rec, httpx.ReasonConflict, "steps")
	})
}

// TestBlueprintMerge_FaultReasons separates merge's three 422s.
func TestBlueprintMerge_FaultReasons(t *testing.T) {
	t.Run("triggered source", func(t *testing.T) {
		s := newTestServer(t)
		host, _ := createWrappedBlueprint(t, s, "Host")
		source, _ := createWrappedBlueprint(t, s, "TriggeredSource")
		tr := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
			"kind":                     "trigger",
			"event_type":               "github:pr:ci_check_failed",
			"blueprint_id":             source,
			"breaker_threshold":        3,
			"min_autonomy_suitability": 0.0,
		})
		if tr.Code != http.StatusCreated {
			t.Fatalf("seed trigger: %d: %s", tr.Code, tr.Body.String())
		}

		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
			"source_blueprint_id": source,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		// The source is mergeable in principle — it just has a trigger attached
		// right now. That is a state conflict the caller can clear.
		assertFirstError(t, rec, httpx.ReasonConflict, "source_blueprint_id")
	})

	t.Run("over the step cap", func(t *testing.T) {
		s := newTestServer(t)
		host := createBareBlueprint(t, s, "BigHost")
		steps := make([]map[string]any, 0, maxBlueprintSteps)
		for i := range maxBlueprintSteps {
			steps = append(steps, map[string]any{
				"step_prompt_id": createBarePrompt(t, s, fmt.Sprintf("reason-cap-%d", i)),
			})
		}
		if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+host+"/steps", map[string]any{"steps": steps}); put.Code != http.StatusNoContent {
			t.Fatalf("seed steps: %d: %s", put.Code, put.Body.String())
		}
		source, _ := createWrappedBlueprint(t, s, "OneMore")

		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
			"source_blueprint_id": source,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonOutOfRange, "source_blueprint_id")
	})
}

// TestBlueprintSplit_FaultReason: an index outside (0, N) is a bound, not a
// team fault.
func TestBlueprintSplit_FaultReason(t *testing.T) {
	s := newTestServer(t)
	bp := createBareBlueprint(t, s, "SplitReason")
	steps := []map[string]any{
		{"step_prompt_id": createBarePrompt(t, s, "sr0")},
		{"step_prompt_id": createBarePrompt(t, s, "sr1")},
	}
	if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+bp+"/steps", map[string]any{"steps": steps}); put.Code != http.StatusNoContent {
		t.Fatalf("seed steps: %d: %s", put.Code, put.Body.String())
	}

	for _, at := range []int{0, 2} {
		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/split", map[string]any{"at_step_index": at})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("split at %d = %d, want 422; body=%s", at, rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonOutOfRange, "at_step_index")
	}
}

// TestBlueprintReconnect_FaultReasons separates reconnect's self-reference and
// attached-trigger refusals — an invalid field and a state conflict.
func TestBlueprintReconnect_FaultReasons(t *testing.T) {
	t.Run("into itself", func(t *testing.T) {
		s := newTestServer(t)
		bp, _ := createWrappedBlueprint(t, s, "SelfReconnect")
		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/reconnect", map[string]any{
			"target_blueprint_id": bp,
			"at_step_index":       1,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonInvalidField, "target_blueprint_id")
	})

	t.Run("triggered target", func(t *testing.T) {
		s := newTestServer(t)
		host := createBareBlueprint(t, s, "ReconnectHost")
		steps := []map[string]any{
			{"step_prompt_id": createBarePrompt(t, s, "rh0")},
			{"step_prompt_id": createBarePrompt(t, s, "rh1")},
		}
		if put := doJSON(t, s, http.MethodPut, "/api/blueprints/"+host+"/steps", map[string]any{"steps": steps}); put.Code != http.StatusNoContent {
			t.Fatalf("seed steps: %d: %s", put.Code, put.Body.String())
		}
		target, _ := createWrappedBlueprint(t, s, "TriggeredTarget")
		tr := doJSON(t, s, http.MethodPost, "/api/event-handlers", map[string]any{
			"kind":                     "trigger",
			"event_type":               "github:pr:ci_check_failed",
			"blueprint_id":             target,
			"breaker_threshold":        3,
			"min_autonomy_suitability": 0.0,
		})
		if tr.Code != http.StatusCreated {
			t.Fatalf("seed trigger: %d: %s", tr.Code, tr.Body.String())
		}

		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/reconnect", map[string]any{
			"target_blueprint_id": target,
			"at_step_index":       1,
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body=%s", rec.Code, rec.Body.String())
		}
		assertFirstError(t, rec, httpx.ReasonConflict, "target_blueprint_id")
	})
}

// TestBlueprintDuplicate_FaultReasons: the three store sentinels map to three
// different fault classes across three statuses. All three used to answer
// CROSS_TEAM_REF, including the 400 and the 404, where it was doubly wrong.
func TestBlueprintDuplicate_FaultReasons(t *testing.T) {
	s := newTestServer(t)

	empty := doJSON(t, s, http.MethodPost, "/api/blueprints/duplicate", map[string]any{
		"prompt_ids": []string{},
	})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty prompt_ids = %d, want 400; body=%s", empty.Code, empty.Body.String())
	}
	assertFirstError(t, empty, httpx.ReasonMissingField, "prompt_ids")

	unknown := doJSON(t, s, http.MethodPost, "/api/blueprints/duplicate", map[string]any{
		"prompt_ids": []string{fixtureUUID("duplicate-unknown-prompt")},
	})
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown prompt_ids = %d, want 404; body=%s", unknown.Code, unknown.Body.String())
	}
	assertFirstError(t, unknown, httpx.ReasonNotFound, "prompt_ids")
}
