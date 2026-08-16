package server

import (
	"context"
	"net/http"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestProjectEntities_FiltersByProjectAndState — only entities
// assigned to this project AND active should appear. Closed entities
// in the project, and active entities in other projects, must be
// excluded.
func TestProjectEntities_FiltersByProjectAndState(t *testing.T) {
	s := newTestServer(t)
	seedConfiguredRepo(t, s, "owner", "repo")
	pid, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "P", PinnedRepos: []string{"owner/repo"}})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	other, err := s.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "Other", PinnedRepos: []string{"owner/repo"}})
	if err != nil {
		t.Fatal(err)
	}

	mine := mustEntity(t, s.db, "github", "owner/repo#1", "pr", "mine")
	if err := sqlitestore.New(s.db).Entities.AssignProject(context.Background(), runmode.LocalDefaultOrgID, mine.ID, &pid, "rationale-1"); err != nil {
		t.Fatal(err)
	}
	closedMine := mustEntity(t, s.db, "github", "owner/repo#2", "pr", "closed-mine")
	if err := sqlitestore.New(s.db).Entities.AssignProject(context.Background(), runmode.LocalDefaultOrgID, closedMine.ID, &pid, ""); err != nil {
		t.Fatal(err)
	}
	if err := sqlitestore.New(s.db).Entities.MarkClosed(context.Background(), runmode.LocalDefaultOrgID, closedMine.ID); err != nil {
		t.Fatal(err)
	}
	otherProj := mustEntity(t, s.db, "github", "owner/repo#3", "pr", "other-project")
	if err := sqlitestore.New(s.db).Entities.AssignProject(context.Background(), runmode.LocalDefaultOrgID, otherProj.ID, &other, ""); err != nil {
		t.Fatal(err)
	}
	unassigned := mustEntity(t, s.db, "github", "owner/repo#4", "pr", "unassigned")

	entities := decodeList[projectEntity](t, doJSON(t, s, http.MethodPost,
		"/api/projects/"+pid+"/entities/list", map[string]any{})).Items

	gotIDs := map[string]projectEntity{}
	for _, e := range entities {
		gotIDs[e.ID] = e
	}
	if _, ok := gotIDs[mine.ID]; !ok {
		t.Errorf("expected `mine` in response")
	}
	if _, ok := gotIDs[closedMine.ID]; ok {
		t.Errorf("closed entity should be filtered out")
	}
	if _, ok := gotIDs[otherProj.ID]; ok {
		t.Errorf("other-project entity should be filtered out")
	}
	if _, ok := gotIDs[unassigned.ID]; ok {
		t.Errorf("unassigned entity should be filtered out")
	}
	if got := gotIDs[mine.ID].ClassificationRationale; got != "rationale-1" {
		t.Errorf("classification_rationale = %q, want rationale-1", got)
	}
}

// TestProjectEntities_NotFoundProject — unknown project id returns 404
// rather than a 200 with empty list, so the frontend can distinguish
// "no entities here" from "this project is gone."
func TestProjectEntities_NotFoundProject(t *testing.T) {
	s := newTestServer(t)
	rec := doJSON(t, s, http.MethodPost, "/api/projects/missing-id/entities/list", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
