package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestFactorySnapshot_ServesTheRepositoryID pins the id the belt groups a pull
// request's chip by: the snapshot carries the entity's repository registry id,
// so the client never has to group by the repository's name.
func TestFactorySnapshot_ServesTheRepositoryID(t *testing.T) {
	s := newTestServer(t)

	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "https://github.com", "owner/repo#7", "", "pr", "test PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if entity.RepositoryID == "" {
		t.Fatal("seeded entity has no repository id")
	}

	rec := doJSON(t, s, http.MethodGet, "/api/factory/snapshot", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d", rec.Code)
	}
	var snap struct {
		Entities []map[string]any `json:"entities"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, e := range snap.Entities {
		if e["id"] != entity.ID {
			continue
		}
		if e["repository_id"] != entity.RepositoryID {
			t.Errorf("repository_id = %v, want %s", e["repository_id"], entity.RepositoryID)
		}
		for _, absent := range []string{"jira_project_id", "linear_team_id"} {
			if v, ok := e[absent]; ok {
				t.Errorf("a pull request carried %s = %v", absent, v)
			}
		}
		return
	}
	t.Fatalf("entity %s missing from the snapshot", entity.ID)
}
