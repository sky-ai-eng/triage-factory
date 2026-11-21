package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// Composition mutations: merge absorbs a trigger-less source onto a host's
// tail, split partitions a blueprint at an index into the trigger-retaining
// upstream and a new trigger-less downstream, and a blueprint takes a trigger
// whatever triggers it already has.

// maxBlueprintSteps is the step cap the blueprints API enforces on the steps
// PUT, merge and reconnect. TestBlueprintMerge_ExceedsStepCapRejected fails if
// it drifts from that cap: a lower API cap refuses the steps PUT that builds
// the host, and a higher one lets the over-cap merge through.
const maxBlueprintSteps = 50

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
	attachHandler(t, s, source, "github:pr:ci_check_failed")

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
	// The source is left live with its steps.
	assertLiveWithSteps(t, s, source, 1)
}

// assertLiveWithSteps fails the test unless the blueprint reads as live and
// holds want steps.
func assertLiveWithSteps(t *testing.T, s *Server, blueprintID string, want int) {
	t.Helper()
	if rec := doJSON(t, s, http.MethodGet, "/api/blueprints/"+blueprintID, nil); rec.Code != http.StatusOK {
		t.Fatalf("GET blueprint %s = %d %s, want it live", blueprintID, rec.Code, rec.Body.String())
	}
	if got := len(listBlueprintSteps(t, s, blueprintID)); got != want {
		t.Fatalf("blueprint %s steps = %d, want %d", blueprintID, got, want)
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

func TestBlueprintSplit_Shape(t *testing.T) {
	s := newTestServer(t)
	bp, _ := createChainBlueprint(t, s, "NeedsIndex", 2)
	v := blueprintVersion(t, s, bp)
	for _, c := range []struct {
		name    string
		body    map[string]any
		reasons map[string]string
	}{
		// A missing at_step_index is a clean 400 "required", distinct from the
		// 422 the ends return — a present 0 is a real index, not absence.
		{"no index", map[string]any{"version": v},
			map[string]string{"at_step_index": httpx.ReasonMissingField}},
		{"no version", map[string]any{"at_step_index": 1},
			map[string]string{"version": httpx.ReasonMissingField}},
		{"zero version", map[string]any{"at_step_index": 1, "version": 0},
			map[string]string{"version": httpx.ReasonInvalidField}},
		// Both are reported, not the first.
		{"neither", map[string]any{},
			map[string]string{"at_step_index": httpx.ReasonMissingField, "version": httpx.ReasonMissingField}},
	} {
		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/split", c.body)
		wantShapeFaults(t, c.name, rec, c.reasons)
	}
	if got := listBlueprintSteps(t, s, bp); len(got) != 2 {
		t.Fatalf("steps after the refused splits = %d, want 2 (untouched)", len(got))
	}
}

// TestBlueprintSplit_StaleVersionRefused sends a split with the version read
// before a steps PUT reordered the blueprint. Its index was a position in the
// steps the caller read, so it is refused, and the PUT's order stays.
func TestBlueprintSplit_StaleVersionRefused(t *testing.T) {
	s := newTestServer(t)
	bp, p := createChainBlueprint(t, s, "Moved", 3)
	read := blueprintVersion(t, s, bp)
	reordered := []string{p[2], p[0], p[1]}
	if put := putBlueprintSteps(t, s, bp, stepsNaming(reordered)); put.Code != http.StatusOK {
		t.Fatalf("steps PUT: %d: %s", put.Code, put.Body.String())
	}

	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/split", map[string]any{"at_step_index": 1, "version": read})
	if rec.Code != http.StatusConflict {
		t.Fatalf("split naming the version before the PUT = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonVersionConflict, "version")
	if got := stepPromptIDs(t, s, bp); !slices.Equal(got, reordered) {
		t.Errorf("steps after the refused split = %v, want the PUT's %v", got, reordered)
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

	read := blueprintVersion(t, s, bp)
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/split", map[string]any{
		"at_step_index": 1,
		"version":       read,
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
	// The upstream answers the version the split raised it to, on itself and
	// on its steps, so the caller can send its next cut from this answer.
	if v := blueprintVersion(t, s, bp); v <= read || resp.Upstream.Blueprint["version"] != float64(v) || resp.Upstream.Steps[0]["blueprint_version"] != float64(v) {
		t.Errorf("upstream answered version %v and step version %v; the blueprint is at %d, from %d",
			resp.Upstream.Blueprint["version"], resp.Upstream.Steps[0]["blueprint_version"], v, read)
	}
}

func TestBlueprintSplit_AtEndsRejected(t *testing.T) {
	s := newTestServer(t)
	bp, _ := createChainBlueprint(t, s, "Pair", 2)
	for _, k := range []int{0, 2} {
		rec := doJSON(t, s, http.MethodPost, "/api/blueprints/"+bp+"/split", map[string]any{"at_step_index": k, "version": blueprintVersion(t, s, bp)})
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

// --- A: many events per blueprint ------------------------------------------

func TestEventHandlerCreate_SecondHandlerOnBlueprint(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	seedHandlerBlueprint(t, s, "bp-two-handlers")
	first := attachHandler(t, s, "bp-two-handlers", "github:pr:ci_check_failed")
	second := attachHandler(t, s, "bp-two-handlers", "github:pr:review_changes_requested")
	if got := handlerIDsForBlueprint(t, s, "bp-two-handlers"); len(got) != 2 || !got[first] || !got[second] {
		t.Errorf("handlers on the blueprint = %v, want the first and the second", got)
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
