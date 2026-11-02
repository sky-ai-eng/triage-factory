package dbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskLatestEventFactory is what a per-backend test hands to
// RunTaskLatestEventConformance: the wired stores, the org every call names,
// and the team its tasks belong to.
type TaskLatestEventFactory func(t *testing.T) (stores db.Stores, orgID, teamID string)

// RunTaskLatestEventConformance pins TaskStore.LatestEventIDSystem: the
// spawning event before any bump, the bumping event after one (and still
// after that bump is folded into a live conversation), and "" for a task with
// no task_events row. primary_event_id names the spawning event throughout.
func RunTaskLatestEventConformance(t *testing.T, mk TaskLatestEventFactory) {
	t.Helper()
	ctx := context.Background()

	// seed creates a CI task spawned by one failing check and returns the
	// stores, the org, the task, and a recorder for further events on the
	// same entity.
	seed := func(t *testing.T, suffix string) (db.Stores, string, domain.Task, func(meta string) string) {
		t.Helper()
		stores, orgID, teamID := mk(t)
		entity, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost,
			fmt.Sprintf("octo/latest-%s#1", suffix), "", "pr", "Latest", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		record := func(meta string) string {
			t.Helper()
			id, err := stores.Events.RecordSystem(ctx, orgID, domain.Event{
				EntityID: &entity.ID, EventType: domain.EventGitHubPRCICheckFailed, MetadataJSON: meta,
			})
			if err != nil {
				t.Fatalf("record event: %v", err)
			}
			return id
		}
		spawnID := record(`{"check_name":"run"}`)
		task, _, err := stores.Tasks.FindOrCreateAtSystem(ctx, orgID, teamID, entity.ID,
			domain.EventGitHubPRCICheckFailed, "", spawnID, 0.5, time.Now().UTC())
		if err != nil || task == nil {
			t.Fatalf("seed task: %+v, %v", task, err)
		}
		return stores, orgID, *task, record
	}

	latest := func(t *testing.T, stores db.Stores, orgID, taskID string) string {
		t.Helper()
		got, err := stores.Tasks.LatestEventIDSystem(ctx, orgID, taskID)
		if err != nil {
			t.Fatalf("LatestEventIDSystem: %v", err)
		}
		return got
	}

	t.Run("the_spawning_event_before_any_bump", func(t *testing.T) {
		stores, orgID, task, _ := seed(t, "spawned")
		if err := stores.Tasks.RecordEventSystem(ctx, orgID, task.ID, task.PrimaryEventID, "spawned"); err != nil {
			t.Fatalf("record spawned row: %v", err)
		}
		if got := latest(t, stores, orgID, task.ID); got != task.PrimaryEventID {
			t.Errorf("LatestEventIDSystem = %q, want the spawning event %q", got, task.PrimaryEventID)
		}
	})

	t.Run("the_bumping_event_after_a_bump", func(t *testing.T) {
		stores, orgID, task, record := seed(t, "bumped")
		if err := stores.Tasks.RecordEventSystem(ctx, orgID, task.ID, task.PrimaryEventID, "spawned"); err != nil {
			t.Fatalf("record spawned row: %v", err)
		}
		bumpID := record(`{"check_name":"links"}`)
		if _, err := stores.Tasks.BumpSystem(ctx, orgID, task.ID, bumpID); err != nil {
			t.Fatalf("BumpSystem: %v", err)
		}
		if err := stores.Tasks.RecordEventSystem(ctx, orgID, task.ID, bumpID, "bumped"); err != nil {
			t.Fatalf("record bumped row: %v", err)
		}
		if got := latest(t, stores, orgID, task.ID); got != bumpID {
			t.Errorf("LatestEventIDSystem = %q, want the bumping event %q", got, bumpID)
		}

		// The same bump, folded into a live conversation, is still the
		// newest event that bumped the task.
		if _, err := stores.Tasks.MarkEventInjectedSystem(ctx, orgID, task.ID, bumpID, db.AgentClaimStamp{}); err != nil {
			t.Fatalf("MarkEventInjectedSystem: %v", err)
		}
		if got := latest(t, stores, orgID, task.ID); got != bumpID {
			t.Errorf("LatestEventIDSystem after the injection = %q, want the bumping event %q", got, bumpID)
		}

		reread, err := stores.Tasks.GetSystem(ctx, orgID, task.ID)
		if err != nil || reread == nil {
			t.Fatalf("GetSystem = %+v, %v", reread, err)
		}
		if reread.PrimaryEventID != task.PrimaryEventID {
			t.Errorf("primary_event_id = %q after the bump, want the spawning event %q", reread.PrimaryEventID, task.PrimaryEventID)
		}
	})

	// A closed task can still be resumed, and a resume writes its row just
	// before the claim that builds the briefing, so both rows are newer than
	// any event that bumped the task.
	t.Run("a_close_or_resume_does_not_count", func(t *testing.T) {
		stores, orgID, task, _ := seed(t, "close-resume")
		if err := stores.Tasks.RecordEventSystem(ctx, orgID, task.ID, task.PrimaryEventID, "spawned"); err != nil {
			t.Fatalf("record spawned row: %v", err)
		}
		for kind, eventType := range map[string]string{
			"closed":  domain.EventGitHubPRMerged,
			"resumed": domain.EventSystemConversationResumed,
		} {
			id, err := stores.Events.RecordSystem(ctx, orgID, domain.Event{EventType: eventType, MetadataJSON: `{}`})
			if err != nil {
				t.Fatalf("record %s event: %v", kind, err)
			}
			if err := stores.Tasks.RecordEventSystem(ctx, orgID, task.ID, id, kind); err != nil {
				t.Fatalf("record %s row: %v", kind, err)
			}
		}
		if got := latest(t, stores, orgID, task.ID); got != task.PrimaryEventID {
			t.Errorf("LatestEventIDSystem = %q, want the spawning event %q", got, task.PrimaryEventID)
		}
	})

	t.Run("empty_for_a_task_with_no_row", func(t *testing.T) {
		stores, orgID, task, _ := seed(t, "none")
		if got := latest(t, stores, orgID, task.ID); got != "" {
			t.Errorf("LatestEventIDSystem = %q, want \"\" for a task with no task_events row", got)
		}
	})
}
