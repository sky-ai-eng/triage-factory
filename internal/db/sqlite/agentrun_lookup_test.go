package sqlite_test

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestAgentRunStore_SQLite_LookupOrgForRunSystem_ReturnsSentinelOrgID
// pins the local-mode behavior of the cold-start identity probe used
// by cmd/exec/runident — every locally-seeded run resolves back to
// the LocalDefaultOrgID sentinel.
func TestAgentRunStore_SQLite_LookupOrgForRunSystem_ReturnsSentinelOrgID(t *testing.T) {
	conn := newSQLiteForAgentRunTest(t)
	stores := sqlitestore.New(conn)
	seeder := newSQLiteAgentRunSeeder(conn)

	entityID := seeder.Entity(t, "lookup")
	eventID := seeder.Event(t, entityID, "github:pr:opened")
	taskID := seeder.Task(t, entityID, "github:pr:opened", eventID)

	runID := "run-lookup-1"
	if err := stores.AgentRuns.Create(context.Background(), runmode.LocalDefaultOrgID, domain.AgentRun{
		ID: runID, TaskID: taskID, PromptID: "p_agentrun_test", Status: "running", Model: "m",
		BlueprintRunID: seeder.BlueprintRun(t, taskID),
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	got, err := stores.AgentRuns.LookupOrgForRunSystem(context.Background(), runID)
	if err != nil {
		t.Fatalf("LookupOrgForRunSystem: %v", err)
	}
	if got != runmode.LocalDefaultOrgID {
		t.Errorf("LookupOrgForRunSystem = %q; want %q", got, runmode.LocalDefaultOrgID)
	}
}

// TestAgentRunStore_SQLite_LookupOrgForRunSystem_UnknownReturnsEmpty
// — an unknown runID returns ("", nil). Callers (runident) map that
// to ErrRunIdentityNotFound rather than a SQL error.
func TestAgentRunStore_SQLite_LookupOrgForRunSystem_UnknownReturnsEmpty(t *testing.T) {
	conn := newSQLiteForAgentRunTest(t)
	stores := sqlitestore.New(conn)

	got, err := stores.AgentRuns.LookupOrgForRunSystem(context.Background(), "ghost-run")
	if err != nil {
		t.Fatalf("LookupOrgForRunSystem: %v", err)
	}
	if got != "" {
		t.Errorf("LookupOrgForRunSystem on unknown run = %q; want empty string", got)
	}
}
