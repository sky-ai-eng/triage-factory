package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestBlueprintDelete_CascadesPromptsAndTrigger deletes a whole multi-step
// blueprint and asserts the cascade: the header soft-deletes (request reads 404
// but the row survives so the ...System reads still resolve it for in-flight
// run timelines), every copy-only step prompt soft-deletes, and the bound
// trigger is hard-deleted (detached).
func TestBlueprintDelete_CascadesPromptsAndTrigger(t *testing.T) {
	s := newTestServer(t)
	bp, ps := createChainBlueprint(t, s, "DeleteMe", 3)
	attachTrigger(t, s, bp, "github:pr:ci_check_failed")

	del := doJSON(t, s, http.MethodDelete, "/api/blueprints/"+bp, nil)
	if del.Code != http.StatusOK {
		t.Fatalf("delete blueprint: expected 200, got %d: %s", del.Code, del.Body.String())
	}

	// Header gone from request-facing reads (steps GET + List).
	if g := doJSON(t, s, http.MethodGet, "/api/blueprints/"+bp+"/steps", nil); g.Code != http.StatusNotFound {
		t.Fatalf("deleted blueprint steps GET: expected 404, got %d", g.Code)
	}
	list := doJSON(t, s, http.MethodGet, "/api/blueprints", nil)
	var bps []map[string]any
	_ = json.Unmarshal(list.Body.Bytes(), &bps)
	for _, b := range bps {
		if b["id"] == bp {
			t.Fatalf("deleted blueprint %s still listed", bp)
		}
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

	// Soft vs hard, at the row level: the header + every step prompt survive with
	// deleted_at stamped (so the ...System reads resolve them), but the trigger
	// row is physically gone (hard delete, system rows included).
	if n := countRows(t, s, `SELECT COUNT(*) FROM blueprints WHERE id = ? AND deleted_at IS NOT NULL`, bp); n != 1 {
		t.Errorf("blueprint header rows with deleted_at = %d, want 1 (soft-delete, row survives)", n)
	}
	for _, p := range ps {
		if n := countRows(t, s, `SELECT COUNT(*) FROM prompts WHERE id = ? AND deleted_at IS NOT NULL`, p); n != 1 {
			t.Errorf("step prompt %s rows with deleted_at = %d, want 1 (soft-delete)", p, n)
		}
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM event_handlers WHERE blueprint_id = ?`, bp); n != 0 {
		t.Errorf("trigger rows after blueprint delete = %d, want 0 (hard-deleted)", n)
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

// countRows runs a COUNT(*) query against the server's raw DB handle. The
// blueprint-delete cascade mixes soft-deletes (header, step prompts) with a hard
// delete (trigger), so the tests assert at the row level — the request-facing
// API can only see the soft-deleted rows as absent, not tell soft from hard.
func countRows(t *testing.T, s *Server, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("countRows %q: %v", query, err)
	}
	return n
}
