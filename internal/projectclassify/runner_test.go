package projectclassify

import (
	"context"
	"errors"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunner_AllErroredLeavesEntityForRetry guards against the bug
// where a fully-failed classification cycle (e.g., claude CLI
// unavailable) would still stamp classified_at, permanently freezing
// the entity at unassigned even after the failure clears. The runner
// should leave classified_at NULL when every per-project vote
// errored, so the entity resurfaces on the next trigger.
func TestRunner_AllErroredLeavesEntityForRetry(t *testing.T) {
	isolateHome(t)
	database := newTestDB(t)

	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p1", Name: "P1"}); err != nil {
		t.Fatal(err)
	}
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#1", "pr", "T", "https://x/1")
	if err != nil {
		t.Fatal(err)
	}

	// Force every Stage 1 vote to error (simulates claude CLI down).
	origS1 := runStage1Haiku
	t.Cleanup(func() { runStage1Haiku = origS1 })
	runStage1Haiku = func(_ context.Context, _ string, prompt string) (int, string, error) {
		return 0, "", errors.New("simulated CLI down")
	}

	r := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, sqlitestore.New(database).Orgs)
	r.run(context.Background()) // synchronous one cycle

	post, err := sqlitestore.New(database).Entities.ListUnclassified(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("ListUnclassifiedEntities: %v", err)
	}
	found := false
	for _, e := range post {
		if e.ID == entity.ID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("entity should still be unclassified after fully-failed cycle; classified_at must remain NULL for retry")
	}
}

// TestRunner_PartialErrorStillStamps verifies that a cycle where some
// votes succeed and some error DOES stamp classified_at — partial
// information is a real classification result, just with a known
// gap, and we don't want to keep retrying for the broken project.
func TestRunner_PartialErrorStillStamps(t *testing.T) {
	isolateHome(t)
	database := newTestDB(t)

	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p-good", Name: "Good"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlitestore.New(database).Projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{ID: "p-flaky", Name: "Flaky"}); err != nil {
		t.Fatal(err)
	}
	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "owner/repo#2", "pr", "T", "https://x/2")
	if err != nil {
		t.Fatal(err)
	}

	origS1 := runStage1Haiku
	t.Cleanup(func() { runStage1Haiku = origS1 })
	runStage1Haiku = func(_ context.Context, _ string, prompt string) (int, string, error) {
		if strings.Contains(prompt, "<project_name>\nFlaky\n</project_name>") {
			return 0, "", errors.New("simulated CLI failure for Flaky")
		}
		return 30, "stub for Good", nil
	}

	r := NewRunner(sqlitestore.New(database).Entities, sqlitestore.New(database).Projects, sqlitestore.New(database).Orgs)
	r.run(context.Background())

	post, err := sqlitestore.New(database).Entities.ListUnclassified(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range post {
		if e.ID == entity.ID {
			t.Errorf("entity should be classified (partial-error path stamps classified_at)")
		}
	}
}
