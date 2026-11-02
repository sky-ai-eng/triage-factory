package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

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
	prompts := httpxtest.DecodeList[map[string]any](t, doJSON(t, s, http.MethodPost, "/api/prompts/list", map[string]any{}))
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
	if blueprintListed(t, s, bp) {
		t.Fatalf("sole-owner blueprint %s still listed after pairing delete", bp)
	}
}

// Deleting a step prompt of a triggered multi-step blueprint splits it: the
// steps before it keep the blueprint's id and trigger (with none before it,
// both are gone), and the steps after it become a new trigger-less blueprint.
func TestPromptDelete_SplitsMultiStepBlueprint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		n, at int
	}{
		{"tail keeps the id and trigger", 3, 2},
		{"head retires the blueprint and detaches its trigger", 3, 0},
		{"mid splits off a trigger-less downstream", 4, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			configureEventSources(t, s)
			bp, ps := createChainBlueprint(t, s, "Chain", tc.n)
			attachTrigger(t, s, bp, "github:pr:ci_check_failed")

			code, orphaned := deletePrompt(t, s, ps[tc.at])
			if code != http.StatusOK {
				t.Fatalf("delete step %d: expected 200, got %d", tc.at, code)
			}
			if want := tc.at < tc.n-1; orphaned != want {
				t.Errorf("orphaned_blueprint = %v, want %v", orphaned, want)
			}
			if g := doJSON(t, s, http.MethodGet, "/api/prompts/"+ps[tc.at], nil); g.Code != http.StatusNotFound {
				t.Fatalf("deleted prompt GET: expected 404, got %d", g.Code)
			}

			// A head delete retires the blueprint with its entry prompt and
			// deletes its trigger rather than leave it on a deleted row.
			wantTriggers := 1
			if tc.at == 0 {
				wantTriggers = 0
				if blueprintExists(t, s, bp) {
					t.Error("head delete left the original blueprint readable")
				}
			} else if got := stepPromptIDs(t, s, bp); !equalStrings(got, ps[:tc.at]) {
				t.Errorf("upstream steps = %v, want %v", got, ps[:tc.at])
			}
			if n := len(triggersForBlueprint(t, s, bp)); n != wantTriggers {
				t.Errorf("original blueprint trigger count = %d, want %d", n, wantTriggers)
			}

			if tc.at == tc.n-1 {
				return
			}
			down := ownerBlueprintOf(t, s, ps[tc.at+1])
			if down == "" || down == bp {
				t.Fatalf("owner of the step after the deleted one = %q, want a fresh blueprint (original %s)", down, bp)
			}
			if got := stepPromptIDs(t, s, down); !equalStrings(got, ps[tc.at+1:]) {
				t.Errorf("downstream steps = %v, want %v", got, ps[tc.at+1:])
			}
			if n := len(triggersForBlueprint(t, s, down)); n != 0 {
				t.Errorf("downstream trigger count = %d, want 0 (trigger-less)", n)
			}
		})
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

// createBareBlueprint creates a blueprint with no first_prompt, which wraps
// nothing.
func createBareBlueprint(t *testing.T, s *Server, name string) string {
	t.Helper()
	rec := doJSON(t, s, http.MethodPost, "/api/blueprints", map[string]any{"name": name})
	if rec.Code != http.StatusCreated {
		t.Fatalf("createBareBlueprint %s: %d: %s", name, rec.Code, rec.Body.String())
	}
	var b struct {
		ID            string `json:"id"`
		FirstPromptID string `json:"first_prompt_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	if b.ID == "" || b.FirstPromptID != "" {
		t.Fatalf("createBareBlueprint %s: got %+v, want an id and no first_prompt_id", name, b)
	}
	return b.ID
}

// createChainBlueprint builds a bare blueprint stepping through n fresh bare
// prompts named name-0 … name-(n-1) and returns the blueprint id + the ordered
// step prompt ids.
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

// seedTriggerRun records a run of triggerID: a conversation and its blueprint
// run, both carrying the trigger_id that references the trigger. Returns the
// conversation id.
func seedTriggerRun(t *testing.T, s *Server, triggerID, suffix string) string {
	t.Helper()
	conversationID := seedSteerConversation(t, s.db, suffix, dbtest.SeedConcluded)
	execSQL(t, s.db, `UPDATE conversations SET trigger_id = ? WHERE id = ?`, triggerID, conversationID)
	execSQL(t, s.db, `UPDATE blueprint_runs SET trigger_id = ? WHERE id = (SELECT blueprint_run_id FROM conversations WHERE id = ?)`, triggerID, conversationID)
	return conversationID
}

// runTriggerIDs returns the trigger_id a conversation and its blueprint run
// carry.
func runTriggerIDs(t *testing.T, s *Server, conversationID string) (conversation, blueprintRun string) {
	t.Helper()
	if err := s.db.QueryRow(`
		SELECT COALESCE(c.trigger_id, ''), COALESCE(br.trigger_id, '')
		FROM conversations c JOIN blueprint_runs br ON br.id = c.blueprint_run_id
		WHERE c.id = ?`, conversationID,
	).Scan(&conversation, &blueprintRun); err != nil {
		t.Fatalf("read trigger_id of conversation %s: %v", conversationID, err)
	}
	return conversation, blueprintRun
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
