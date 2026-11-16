package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// factoryBelt reads the factory snapshot's entities, keyed by id.
func factoryBelt(t *testing.T, s *Server) map[string]map[string]any {
	t.Helper()
	rec := doJSON(t, s, http.MethodGet, "/api/factory/snapshot", nil)
	var snap struct {
		Entities []map[string]any `json:"entities"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("snapshot = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	belt := make(map[string]map[string]any, len(snap.Entities))
	for _, e := range snap.Entities {
		id, _ := e["id"].(string)
		belt[id] = e
	}
	return belt
}

// The belt groups a pull request's chip by the repository registry id the
// snapshot serves, so the client never groups by the repository's name.
func TestFactorySnapshot_ServesTheRepositoryID(t *testing.T) {
	s := newTestServer(t)
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "https://github.com", "owner/repo#7", "", "pr", "test PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}

	e, ok := factoryBelt(t, s)[entity.ID]
	if !ok {
		t.Fatalf("entity %s missing from the snapshot", entity.ID)
	}
	if e["repository_id"] != entity.RepositoryID {
		t.Errorf("repository_id = %v, want %s", e["repository_id"], entity.RepositoryID)
	}
}
