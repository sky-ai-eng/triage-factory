package routing

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// These tests cover the narrow race the reviewer flagged: a ticket that was
// atomic (task already exists from jira:issue:assigned) gains subtasks, loses
// them, and fires jira:issue:became_atomic. Dedup on (entity, event_type,
// dedup_key) doesn't catch this across different event_types, so without a
// guard the router would create a second concurrent task for the same entity.

func TestHandleEvent_BecameAtomic_ExistingTask_NoDuplicate(t *testing.T) {
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-500", "issue",
		"Epic that went atomic-subtasks-atomic", "https://jira.example.com/browse/SKY-500")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	// Simulate: the ticket was atomic on first discovery, task already
	// exists from jira:issue:assigned. This is the pre-existing state
	// when became_atomic is about to fire.
	assignedMeta := events.JiraIssueAssignedMetadata{
		Assignee:          "aidan",
		AssigneeAccountID: "557058:abc-aidan",
		IssueKey:          "SKY-500",
		Project:           "SKY",
		Summary:           "Epic",
	}
	assignedJSON, _ := json.Marshal(assignedMeta)
	assignedEventID, err := sqlitestore.New(database).Events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EventType:    domain.EventJiraIssueAssigned,
		EntityID:     &entity.ID,
		MetadataJSON: string(assignedJSON),
		CreatedAt:    time.Now(),
	})
	if err != nil {
		t.Fatalf("record assigned event: %v", err)
	}
	existingTask, _, err := testTaskStore(database).FindOrCreate(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, entity.ID,
		domain.EventJiraIssueAssigned, "", assignedEventID, 0.5)
	if err != nil {
		t.Fatalf("create existing task: %v", err)
	}

	// Now fire became_atomic (simulating: subtasks were added, then all
	// closed, triggering the belated-discovery path). HandleEvent
	// records the event itself, so we pass an unpersisted one.
	atomicMeta := events.JiraIssueBecameAtomicMetadata{
		Assignee:          "aidan",
		AssigneeAccountID: "557058:abc-aidan",
		IssueKey:          "SKY-500",
		Project:           "SKY",
		Summary:           "Epic",
	}
	atomicJSON, _ := json.Marshal(atomicMeta)

	ws := websocket.NewHub()
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, nil, testTaskStore(database), sqlitestore.New(database).Conversations, sqlitestore.New(database).Entities, sqlitestore.New(database).PendingFirings, sqlitestore.New(database).Events, sqlitestore.New(database).Orgs, sqlitestore.New(database).Teams, nil, nil, nil, nil, noopScorer{}, ws)

	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventJiraIssueBecameAtomic,
		EntityID:     &entity.ID,
		MetadataJSON: string(atomicJSON),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	// Verify: still exactly one active task on the entity.
	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("expected 1 active task (existing one preserved, duplicate skipped), got %d", len(active))
		for _, a := range active {
			t.Logf("  task %s: event_type=%s status=%s", a.ID, a.EventType, a.Status)
		}
	}
	if len(active) >= 1 && active[0].ID != existingTask.ID {
		t.Errorf("expected original task %s, got %s", existingTask.ID, active[0].ID)
	}
}

func TestHandleEvent_BecameAtomic_NoExistingTask_CreatesTask(t *testing.T) {
	// Negative control: without an existing task, became_atomic DOES
	// create one via the seeded rule. Confirms the guard only suppresses
	// the duplicate case — it doesn't break the normal belated-discovery
	// flow.
	database := newTestDB(t)
	seedHandlerFKTargets(t, database)
	if err := testEventHandlerStore(database).Seed(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, seedHandlerFKTargets(t, database)); err != nil {
		t.Fatalf("seed event handlers: %v", err)
	}
	// became_atomic now routes by assignee (the owning-team ladder), so the
	// assignee must resolve to a TF team for the rule to mint a task.
	setJiraHost(t, database)
	seedJiraUserOnTeam(t, database, runmode.LocalDefaultTeamID, "557058:abc-aidan", "aidan")

	entity, _, err := sqlitestore.New(database).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "jira", "SKY-501", "issue",
		"Epic decomposed then atomic", "https://jira.example.com/browse/SKY-501")
	if err != nil {
		t.Fatalf("create entity: %v", err)
	}

	meta := events.JiraIssueBecameAtomicMetadata{
		Assignee:          "aidan",
		AssigneeAccountID: "557058:abc-aidan",
		IssueKey:          "SKY-501",
		Project:           "SKY",
	}
	metaJSON, _ := json.Marshal(meta)

	ws := websocket.NewHub()
	st := sqlitestore.New(database)
	router := NewRouter(testPromptStore(database), testBlueprintStore(database), testEventHandlerStore(database), nil, nil, st.Users, testTaskStore(database), st.Conversations, st.Entities, st.PendingFirings, st.Events, st.Orgs, st.Teams, nil, nil, nil, nil, noopScorer{}, ws)

	router.HandleEvent(context.Background(), domain.Event{
		EventType:    domain.EventJiraIssueBecameAtomic,
		EntityID:     &entity.ID,
		MetadataJSON: string(metaJSON),
		CreatedAt:    time.Now(),
		OrgID:        runmode.LocalDefaultOrgID,
	})

	active, err := testTaskStore(database).FindActiveByEntity(t.Context(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatalf("list active tasks: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active task from became_atomic rule, got %d", len(active))
	}
	if active[0].EventType != domain.EventJiraIssueBecameAtomic {
		t.Errorf("expected event_type=became_atomic, got %s", active[0].EventType)
	}
}
