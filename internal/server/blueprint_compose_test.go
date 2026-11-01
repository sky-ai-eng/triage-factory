package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// Composition mutations: merge absorbs a trigger-less source onto a host's
// tail, split partitions a blueprint at an index into the trigger-retaining
// upstream and a new trigger-less downstream, and a second trigger on one
// blueprint is a clean 409.

type composeSteps []map[string]any

type blueprintWithStepsView struct {
	Blueprint map[string]any `json:"blueprint"`
	Steps     composeSteps   `json:"steps"`
}

// listBlueprintSteps reads one blueprint's steps through the step-list route.
func listBlueprintSteps(t *testing.T, s *Server, blueprintID string) composeSteps {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/blueprint-steps/list", map[string]any{
		"blueprint_ids": []string{blueprintID},
	})
	return httpxtest.DecodeList[map[string]any](t, rec).Items
}

// listAllBlueprintSteps reads every in-scope blueprint's steps — the canvas's
// bulk read — and groups them by blueprint id.
func listAllBlueprintSteps(t *testing.T, s *Server) map[string]composeSteps {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/blueprint-steps/list", map[string]any{})
	out := map[string]composeSteps{}
	for _, st := range httpxtest.DecodeList[map[string]any](t, rec).Items {
		id, _ := st["blueprint_id"].(string)
		out[id] = append(out[id], st)
	}
	return out
}

// blueprintExists reports whether GET /api/blueprints/{id} resolves the
// blueprint.
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

// blueprintListed reports whether the blueprint list serves blueprintID.
func blueprintListed(t *testing.T, s *Server, blueprintID string) bool {
	t.Helper()
	page := httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/blueprints/list", map[string]any{}))
	for _, b := range page.Items {
		if b["id"] == blueprintID {
			return true
		}
	}
	return false
}

// --- B: atomic merge -----------------------------------------------------

func TestBlueprintMerge_TriggeredSourceRejected(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	host, _ := createWrappedBlueprint(t, s, "Host")
	source, _ := createWrappedBlueprint(t, s, "TriggeredSource")
	// The canvas cannot attach a trigger to a merge source, but a headless
	// caller can, so the endpoint must refuse rather than corrupt state.
	attachTrigger(t, s, source, "github:pr:ci_check_failed")

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
		"source_blueprint_id": source,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("merge triggered source: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	// A state conflict the caller can clear, not a team-boundary fault.
	httpxtest.AssertFirstError(t, rec, httpx.ReasonConflict, "source_blueprint_id")
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
	host, _ := createChainBlueprint(t, s, "BigHost", maxBlueprintSteps)
	source, _ := createWrappedBlueprint(t, s, "OneMore")

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
		"source_blueprint_id": source,
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("merge over cap: expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonOutOfRange, "source_blueprint_id")
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
	bp, _ := createChainBlueprint(t, s, "NeedsIndex", 2)
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
	bp, p := createChainBlueprint(t, s, "Chain", 3)

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
	if len(resp.Upstream.Steps) != 1 || resp.Upstream.Steps[0]["step_prompt_id"] != p[0] {
		t.Fatalf("upstream steps=%+v, want [p0]", resp.Upstream.Steps)
	}
	// Downstream is a new blueprint with [1,N) re-densified 0-based.
	if resp.Downstream.Blueprint["id"] == bp || resp.Downstream.Blueprint["id"] == "" {
		t.Errorf("downstream must be a fresh blueprint, got id=%v", resp.Downstream.Blueprint["id"])
	}
	if len(resp.Downstream.Steps) != 2 || resp.Downstream.Steps[0]["step_prompt_id"] != p[1] || resp.Downstream.Steps[1]["step_prompt_id"] != p[2] {
		t.Fatalf("downstream steps=%+v, want [p1,p2]", resp.Downstream.Steps)
	}
	if resp.Downstream.Steps[0]["step_index"].(float64) != 0 || resp.Downstream.Steps[1]["step_index"].(float64) != 1 {
		t.Errorf("downstream indices not re-densified 0-based: %+v", resp.Downstream.Steps)
	}
	// Downstream name defaults to its new step-0 prompt's name.
	if resp.Downstream.Blueprint["name"] != "Chain-1" {
		t.Errorf("downstream name=%v, want Chain-1 (defaults to its step-0 prompt)", resp.Downstream.Blueprint["name"])
	}
}

func TestBlueprintSplit_AtEndsRejected(t *testing.T) {
	s := newTestServer(t)
	bp, _ := createChainBlueprint(t, s, "Pair", 2)
	for _, k := range []int{0, 2} {
		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/split", map[string]any{"at_step_index": k})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("split at %d: expected 422, got %d: %s", k, rec.Code, rec.Body.String())
		}
		// An index outside (0, N) is a bound, not a team fault.
		httpxtest.AssertFirstError(t, rec, httpx.ReasonOutOfRange, "at_step_index")
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
	configureEventSources(t, s)
	seedBlueprintForTrigger(t, s, "bp-one-trigger")
	attachTrigger(t, s, "bp-one-trigger", "github:pr:ci_check_failed")
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
	configureEventSources(t, s)
	seedBlueprintForTrigger(t, s, "bp-promote-conflict")
	attachTrigger(t, s, "bp-promote-conflict", "github:pr:ci_check_failed")
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

// A merge appends the source's steps to the host's tail and retires the
// source: the merge response, the bulk step read (no blueprint_ids, grouped by
// blueprint_id) and the blueprint reads all show it.
func TestBlueprintStepsAll_BulkGroupedAndExcludesRetired(t *testing.T) {
	s := newTestServer(t)
	host, hp := createWrappedBlueprint(t, s, "Alpha")
	source, sp := createWrappedBlueprint(t, s, "Beta")

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+host+"/merge", map[string]any{
		"source_blueprint_id": source,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("merge: %d: %s", rec.Code, rec.Body.String())
	}
	var resp blueprintWithStepsView
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Blueprint["id"] != host || len(resp.Steps) != 2 ||
		resp.Steps[0]["step_prompt_id"] != hp || resp.Steps[1]["step_prompt_id"] != sp ||
		resp.Steps[0]["step_index"] != float64(0) || resp.Steps[1]["step_index"] != float64(1) {
		t.Errorf("merge response = %+v, want host %s stepping [%s %s] at indices 0, 1", resp, host, hp, sp)
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
	if blueprintListed(t, s, source) || blueprintExists(t, s, source) {
		t.Errorf("retired source %s is still listed or readable after the merge", source)
	}
}
