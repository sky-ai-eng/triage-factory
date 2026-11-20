package delegate

import (
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// An event firing of a handler on a blueprint its team deleted, or on a live
// blueprint whose step prompt its team deleted, is refused as
// blueprint_unusable and mints no run. Both survive their delete as rows, so
// without the refusal the run would execute what the team deleted. A hidden
// step prompt is not deleted: the skill importer hides duplicate prompts a
// live blueprint can still run, so its firing goes ahead.
func TestDelegate_EventFiringOfDeletedContent(t *testing.T) {
	for _, tc := range []struct {
		name string
		// remove takes away part of the fixture blueprint bpID, whose one
		// step runs promptID.
		remove  func(t *testing.T, stores db.Stores, org, bpID, promptID string)
		refused error
	}{
		{"blueprint_deleted", func(t *testing.T, stores db.Stores, org, bpID, _ string) {
			if err := stores.Blueprints.Delete(t.Context(), org, bpID); err != nil {
				t.Fatalf("delete blueprint: %v", err)
			}
		}, ErrBlueprintNotFound},
		{"step_prompt_deleted", func(t *testing.T, stores db.Stores, org, _, promptID string) {
			if err := stores.Prompts.Delete(t.Context(), org, promptID); err != nil {
				t.Fatalf("delete prompt: %v", err)
			}
		}, ErrPromptNotFound},
		{"step_prompt_hidden", func(t *testing.T, stores db.Stores, org, _, promptID string) {
			if _, err := stores.Prompts.Hide(t.Context(), org, promptID); err != nil {
				t.Fatalf("hide prompt: %v", err)
			}
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newDelegateTestDB(t)
			org := runmode.LocalDefaultOrgID
			task, bpID := delegatableFixture(t, database, "deleted-"+tc.name)
			stores := sqlitestore.New(database)

			handlerID := "deleted-" + tc.name + "-handler"
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
			steps, err := stores.Blueprints.ListStepsSystem(t.Context(), org, bpID)
			if err != nil || len(steps) != 1 {
				t.Fatalf("fixture steps = %v, %v; want one", steps, err)
			}
			// Taken away without detaching or fragmenting anything: the row a
			// delete race leaves.
			tc.remove(t, stores, org, bpID, steps[0].StepPromptID)

			s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
			_, err = s.Delegate(task, DelegateOpts{
				OrgID: org, ExplicitBlueprintID: bpID,
				TriggerType: "event", TriggerID: handlerID, TriggeringEventID: eventID,
			})
			runs := countBlueprintRuns(t, database, task.ID)
			if tc.refused == nil {
				if err != nil || runs != 1 {
					t.Fatalf("event Delegate = %v with %d runs; want one run", err, runs)
				}
				return
			}
			if !errors.Is(err, tc.refused) {
				t.Fatalf("event Delegate = %v; want %v", err, tc.refused)
			}
			if cause, ok := RefusalCause(err); !ok || cause != domain.DelegationRefusedBlueprintUnusable {
				t.Errorf("RefusalCause = (%q, %v); want (%q, true)", cause, ok, domain.DelegationRefusedBlueprintUnusable)
			}
			if runs != 0 {
				t.Errorf("blueprint runs after the refused firing = %d; want 0", runs)
			}
		})
	}
}
