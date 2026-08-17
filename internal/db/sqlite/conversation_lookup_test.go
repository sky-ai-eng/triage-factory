package sqlite_test

import (
	"context"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestConversationStore_SQLite_LookupOrgForRunSystem_ReturnsSentinelOrgID
// pins the local-mode behavior of the cold-start identity probe used
// by cmd/exec/runident — every locally-seeded run resolves back to
// the LocalDefaultOrgID sentinel.
func TestConversationStore_SQLite_LookupOrgForRunSystem_ReturnsSentinelOrgID(t *testing.T) {
	conn := newSQLiteForConversationTest(t)
	stores := sqlitestore.New(conn)
	seeder := newSQLiteConversationSeeder(conn)

	entityID := seeder.Entity(t, "lookup")
	eventID := seeder.Event(t, entityID, "github:pr:opened")
	taskID := seeder.Task(t, entityID, "github:pr:opened", eventID)

	conversationID := seeder.Run(t, domain.Conversation{
		ID: "run-lookup-1", TaskID: taskID, PromptID: "p_agentrun_test", Status: "running", Model: "m",
		BlueprintRunID: seeder.BlueprintRun(t, taskID),
	})

	got, err := stores.Conversations.LookupOrgForConversationSystem(context.Background(), conversationID)
	if err != nil {
		t.Fatalf("LookupOrgForConversationSystem: %v", err)
	}
	if got != runmode.LocalDefaultOrgID {
		t.Errorf("LookupOrgForConversationSystem = %q; want %q", got, runmode.LocalDefaultOrgID)
	}
}

// TestConversationStore_SQLite_LookupOrgForRunSystem_UnknownReturnsEmpty
// — an unknown conversationID returns ("", nil). Callers (runident) map that
// to ErrRunIdentityNotFound rather than a SQL error.
func TestConversationStore_SQLite_LookupOrgForRunSystem_UnknownReturnsEmpty(t *testing.T) {
	conn := newSQLiteForConversationTest(t)
	stores := sqlitestore.New(conn)

	got, err := stores.Conversations.LookupOrgForConversationSystem(context.Background(), "ghost-run")
	if err != nil {
		t.Fatalf("LookupOrgForConversationSystem: %v", err)
	}
	if got != "" {
		t.Errorf("LookupOrgForConversationSystem on unknown run = %q; want empty string", got)
	}
}
