package delegate

import (
	"errors"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// An event firing of a handler left on a blueprint its team deleted is refused
// as blueprint_unusable and mints no run. The blueprint's steps and their
// prompts survive the delete, so without the refusal the run would execute
// what the team deleted.
func TestDelegate_EventFiringOfADeletedBlueprintIsRefused(t *testing.T) {
	database := newDelegateTestDB(t)
	org := runmode.LocalDefaultOrgID
	task, bpID := delegatableFixture(t, database, "deletedbp")
	stores := sqlitestore.New(database)

	handlerID := "deletedbp-handler"
	if _, err := stores.EventHandlers.Create(t.Context(), org, runmode.LocalDefaultTeamID, domain.EventHandler{
		ID: handlerID, Mode: domain.EventHandlerModeAutomate, EventType: domain.EventGitHubPRCICheckFailed,
		Name: "CI fix", DefaultPriority: 0.5, BlueprintID: bpID, BreakerThreshold: 3,
	}); err != nil {
		t.Fatalf("EventHandlers.Create: %v", err)
	}
	eventID, err := stores.Events.Record(t.Context(), org, domain.Event{
		EventType: domain.EventGitHubPRCICheckFailed, MetadataJSON: `{}`,
	})
	if err != nil {
		t.Fatalf("record event: %v", err)
	}
	// Deleted without detaching the handler: the row the delete races left.
	if err := stores.Blueprints.Delete(t.Context(), org, bpID); err != nil {
		t.Fatalf("delete blueprint: %v", err)
	}

	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	_, err = s.Delegate(task, DelegateOpts{
		OrgID: org, ExplicitBlueprintID: bpID,
		TriggerType: "event", TriggerID: handlerID, TriggeringEventID: eventID,
	})
	if !errors.Is(err, ErrBlueprintNotFound) {
		t.Fatalf("event Delegate on a deleted blueprint = %v; want ErrBlueprintNotFound", err)
	}
	if cause, ok := RefusalCause(err); !ok || cause != domain.DelegationRefusedBlueprintUnusable {
		t.Errorf("RefusalCause = (%q, %v); want (%q, true)", cause, ok, domain.DelegationRefusedBlueprintUnusable)
	}
	if n := countBlueprintRuns(t, database, task.ID); n != 0 {
		t.Errorf("blueprint runs after the refused firing = %d; want 0", n)
	}
}
