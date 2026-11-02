package server

import (
	"net/http"
	"testing"
)

// TestBlueprintDelete_CascadesPromptsAndTrigger deletes a multi-step blueprint
// whose trigger has run: the header, every step prompt and the bound trigger
// soft-delete (request reads 404, but the rows survive for the ...System reads
// of in-flight run timelines and for the runs that reference the trigger).
func TestBlueprintDelete_CascadesPromptsAndTrigger(t *testing.T) {
	s := newTestServer(t)
	configureEventSources(t, s)
	bp, ps := createChainBlueprint(t, s, "DeleteMe", 3)
	trigger := attachTrigger(t, s, bp, "github:pr:ci_check_failed")
	run := seedTriggerRun(t, s, trigger, "bpdel")

	del := doJSON(t, s, http.MethodDelete, "/api/blueprints/"+bp, nil)
	if del.Code != http.StatusOK {
		t.Fatalf("delete blueprint: expected 200, got %d: %s", del.Code, del.Body.String())
	}

	// Header gone from request-facing reads (GET + list).
	if blueprintExists(t, s, bp) {
		t.Fatal("deleted blueprint steps GET: the blueprint is still readable")
	}
	if blueprintListed(t, s, bp) {
		t.Fatalf("deleted blueprint %s still listed", bp)
	}

	// Every step prompt is gone from request-facing reads.
	for i, p := range ps {
		if g := doJSON(t, s, http.MethodGet, "/api/prompts/"+p, nil); g.Code != http.StatusNotFound {
			t.Fatalf("step %d prompt GET after blueprint delete: expected 404, got %d", i, g.Code)
		}
	}

	// Trigger detached at the API surface.
	if n := len(triggersForBlueprint(t, s, bp)); n != 0 {
		t.Errorf("trigger count after blueprint delete = %d, want 0 (detached)", n)
	}

	// At the row level, the header, every step prompt and the trigger survive
	// with deleted_at stamped, and the run still names its trigger.
	if n := countRows(t, s, `SELECT COUNT(*) FROM blueprints WHERE id = ? AND deleted_at IS NOT NULL`, bp); n != 1 {
		t.Errorf("blueprint header rows with deleted_at = %d, want 1 (soft-delete, row survives)", n)
	}
	for _, p := range ps {
		if n := countRows(t, s, `SELECT COUNT(*) FROM prompts WHERE id = ? AND deleted_at IS NOT NULL`, p); n != 1 {
			t.Errorf("step prompt %s rows with deleted_at = %d, want 1 (soft-delete)", p, n)
		}
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM event_handlers WHERE id = ? AND deleted_at IS NOT NULL`, trigger); n != 1 {
		t.Errorf("trigger rows with deleted_at = %d, want 1 (soft-delete)", n)
	}
	if c, br := runTriggerIDs(t, s, run); c != trigger || br != trigger {
		t.Errorf("run trigger_id after blueprint delete = (conversation %q, blueprint run %q), want %q", c, br, trigger)
	}
}

// TestBlueprintDelete_NotFound covers the missing + already-deleted cases — both
// 404 by re-read (Get filters soft-deleted rows). A second delete of the same id
// must not 500 on the now-soft-deleted row.
func TestBlueprintDelete_NotFound(t *testing.T) {
	s := newTestServer(t)

	if g := doJSON(t, s, http.MethodDelete, "/api/blueprints/does-not-exist", nil); g.Code != http.StatusNotFound {
		t.Fatalf("delete missing blueprint: expected 404, got %d", g.Code)
	}

	bp, p := createWrappedBlueprint(t, s, "Once")
	if g := doJSON(t, s, http.MethodDelete, "/api/blueprints/"+bp, nil); g.Code != http.StatusOK {
		t.Fatalf("first delete: expected 200, got %d: %s", g.Code, g.Body.String())
	}
	// The auto-wrapped sole step prompt soft-deletes alongside the header.
	if g := doJSON(t, s, http.MethodGet, "/api/prompts/"+p, nil); g.Code != http.StatusNotFound {
		t.Fatalf("wrapped step prompt GET after delete: expected 404, got %d", g.Code)
	}
	if g := doJSON(t, s, http.MethodDelete, "/api/blueprints/"+bp, nil); g.Code != http.StatusNotFound {
		t.Fatalf("second delete (already soft-deleted): expected 404, got %d", g.Code)
	}
}

// TestBlueprintDelete_SoftDeletesStepPromptsBySource pins the cascade's split by
// step-prompt source: a user prompt gets deleted_at stamped, a system prompt is
// hidden with deleted_at NULL so the ...System reads still resolve it.
func TestBlueprintDelete_SoftDeletesStepPromptsBySource(t *testing.T) {
	s := newTestServer(t)
	bp, ps := createChainBlueprint(t, s, "MixedSources", 2)
	userPrompt, systemPrompt := ps[0], ps[1]

	// Flip the second step prompt to system source so the cascade routes it
	// through Hide instead of Delete. System prompts carry no creator (the
	// prompts_system_has_no_creator CHECK), so null it in the same UPDATE.
	if _, err := s.db.Exec(`UPDATE prompts SET source = 'system', creator_user_id = NULL WHERE id = ?`, systemPrompt); err != nil {
		t.Fatalf("set step prompt source=system: %v", err)
	}

	del := doJSON(t, s, http.MethodDelete, "/api/blueprints/"+bp, nil)
	if del.Code != http.StatusOK {
		t.Fatalf("delete blueprint: expected 200, got %d: %s", del.Code, del.Body.String())
	}

	// User step prompt → Delete: deleted_at stamped, not hidden.
	if n := countRows(t, s, `SELECT COUNT(*) FROM prompts WHERE id = ? AND deleted_at IS NOT NULL AND hidden = 0`, userPrompt); n != 1 {
		t.Errorf("user step prompt (deleted_at stamped, not hidden) rows = %d, want 1", n)
	}
	// System step prompt → Hide: hidden, deleted_at NULL. The row survives so the
	// ...System reads still resolve it (hidden filters List, not the resolve path).
	if n := countRows(t, s, `SELECT COUNT(*) FROM prompts WHERE id = ? AND hidden = 1 AND deleted_at IS NULL`, systemPrompt); n != 1 {
		t.Errorf("system step prompt (hidden, deleted_at NULL) rows = %d, want 1", n)
	}
	// Header soft-deleted regardless of step-prompt sources.
	if n := countRows(t, s, `SELECT COUNT(*) FROM blueprints WHERE id = ? AND deleted_at IS NOT NULL`, bp); n != 1 {
		t.Errorf("blueprint header rows with deleted_at = %d, want 1", n)
	}

	// Both step prompts lose their canvas presence: the bulk steps read excludes a
	// soft-deleted blueprint's steps, so neither resolves to an owning blueprint —
	// even the hidden system prompt, which (unlike the deleted_at-stamped user
	// prompt) stays individually GET-able since Get filters deleted_at, not hidden.
	if owner := ownerBlueprintOf(t, s, userPrompt); owner != "" {
		t.Errorf("user step prompt still on canvas after delete, owner = %q", owner)
	}
	if owner := ownerBlueprintOf(t, s, systemPrompt); owner != "" {
		t.Errorf("system step prompt still on canvas after delete, owner = %q", owner)
	}
}

// countRows runs a COUNT(*) query against the server's raw DB handle, because
// the request-facing API sees a soft-deleted row and a hard-deleted one the
// same way.
func countRows(t *testing.T, s *Server, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("countRows %q: %v", query, err)
	}
	return n
}
