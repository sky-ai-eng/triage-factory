package runident

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	_ "modernc.org/sqlite"
)

// newStores spins up an in-memory SQLite with the full schema and
// returns the db.Stores bundle plus the raw connection for direct
// seed inserts.
func newStores(t *testing.T) (db.Stores, *sql.DB) {
	t.Helper()
	conn, err := sql.Open("sqlite", ":memory:?_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	t.Cleanup(func() { conn.Close() })
	if err := db.BootstrapSchemaForTest(conn); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return sqlitestore.New(conn), conn
}

// seedRun stamps a task + run with the requested trigger_type so the
// resolver has a row to find. Manual runs get LocalDefaultUserID per
// schema CHECK; event runs leave creator_user_id NULL.
func seedRun(t *testing.T, stores db.Stores, runID, triggerType string) {
	t.Helper()
	ctx := context.Background()
	entity, _, err := stores.Entities.FindOrCreate(ctx, runmode.LocalDefaultOrgID, "jira", "K-"+runID, "issue", "T", "https://x/"+runID)
	if err != nil {
		t.Fatalf("entity: %v", err)
	}
	evt, err := stores.Events.Record(ctx, runmode.LocalDefaultOrg, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	task, _, err := stores.Tasks.FindOrCreate(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, entity.ID, domain.EventJiraIssueAssigned, runID, evt, 0.5)
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if err := stores.Prompts.Create(ctx, runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Prompt{ID: "p-" + runID, Name: "T", Body: "x", Source: "user"}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if err := stores.AgentRuns.Create(ctx, runmode.LocalDefaultOrg, domain.AgentRun{
		ID: runID, TaskID: task.ID, PromptID: "p-" + runID,
		Status: "running", Model: "m",
		TriggerType: triggerType,
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestResolveRunIdentity_EmptyRunID(t *testing.T) {
	stores, _ := newStores(t)
	_, err := ResolveRunIdentity(context.Background(), stores, "")
	if !errors.Is(err, ErrRunIdentityMissing) {
		t.Errorf("err = %v, want ErrRunIdentityMissing", err)
	}
}

func TestResolveRunIdentity_RunNotFound(t *testing.T) {
	stores, _ := newStores(t)
	_, err := ResolveRunIdentity(context.Background(), stores, "ghost")
	if !errors.Is(err, ErrRunIdentityNotFound) {
		t.Errorf("err = %v, want ErrRunIdentityNotFound", err)
	}
}

func TestResolveRunIdentity_ManualRun(t *testing.T) {
	stores, _ := newStores(t)
	seedRun(t, stores, "m1", "manual")

	ident, err := ResolveRunIdentity(context.Background(), stores, "m1")
	if err != nil {
		t.Fatalf("ResolveRunIdentity: %v", err)
	}
	if ident.IsEventTriggered {
		t.Errorf("manual run resolved as event-triggered: %+v", ident)
	}
	if ident.UserID == "" {
		t.Errorf("manual run UserID empty; schema CHECK requires non-NULL creator_user_id for trigger_type='manual' (got %+v)", ident)
	}
	if ident.RunID != "m1" {
		t.Errorf("RunID = %q, want m1", ident.RunID)
	}
}

func TestResolveRunIdentity_EventTriggeredRun(t *testing.T) {
	stores, _ := newStores(t)
	seedRun(t, stores, "e1", "event")

	ident, err := ResolveRunIdentity(context.Background(), stores, "e1")
	if err != nil {
		t.Fatalf("ResolveRunIdentity: %v", err)
	}
	if !ident.IsEventTriggered {
		t.Errorf("event-triggered run resolved as manual: %+v", ident)
	}
	if ident.UserID != "" {
		t.Errorf("event-triggered UserID should be empty (schema NULL); got %q", ident.UserID)
	}
}
