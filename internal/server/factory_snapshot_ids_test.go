package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/zalando/go-keyring"

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
	for _, absent := range []string{"jira_project_id", "linear_team_id"} {
		if v, ok := e[absent]; ok {
			t.Errorf("a pull request carried %s = %v", absent, v)
		}
	}
}

// A Jira ticket on the belt is the viewer's when it is assigned to the account
// id their identity on the org's site carries. A colleague who shares the
// viewer's display name is not the viewer, and an unassigned ticket is
// nobody's.
func TestFactorySnapshot_JiraMineIsTheViewersAccount(t *testing.T) {
	keyring.MockInit()
	s := newTestServer(t)
	seedStockConfig(t, s) // the viewer is acc-1, display name "Tester"
	seedStockTicketIn(t, s, "SKY-1", "To Do", "Tester", "acc-1")
	seedStockTicketIn(t, s, "SKY-2", "To Do", "Tester", "acc-2")
	seedStockTicketIn(t, s, "SKY-3", "To Do", "", "")

	belt := factoryBelt(t, s)
	for id, mine := range map[string]bool{"e_stock_SKY-1": true, "e_stock_SKY-2": false, "e_stock_SKY-3": false} {
		if e, ok := belt[id]; !ok || e["mine"] != mine {
			t.Errorf("%s on the belt = %v, want it there with mine %v", id, e, mine)
		}
	}
}
