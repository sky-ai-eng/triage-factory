package dbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskLatestEventFixture is what a per-backend test hands to
// RunTaskLatestEventConformance.
type TaskLatestEventFixture struct {
	Stores db.Stores
	OrgID  string
	TeamID string

	// InsertionOrdered is true for the dialect that records the order events
	// were inserted in, and so orders two events sharing a created_at
	// (SQLite, by rowid). Postgres records none.
	InsertionOrdered bool

	// SetCreatedAt rewrites an event's created_at, in the layout the dialect
	// stores it in.
	SetCreatedAt func(t *testing.T, eventID string, at time.Time)
}

// TaskLatestEventFactory builds a fresh fixture per subtest.
type TaskLatestEventFactory func(t *testing.T) TaskLatestEventFixture

// RunTaskLatestEventConformance pins TaskStore.LatestEventIDSystem: the
// spawning event before any bump, the bumping event after one (and still
// after that bump is folded into a live conversation), and "" for a task with
// no task_events row. primary_event_id names the spawning event throughout.
// It pins LatestEventAfterSystem beside it: ordered by created_at, not
// insertion; an event missing from the timeline still compared; and a
// created_at tie ordered by insertion only in the dialect that records it.
func RunTaskLatestEventConformance(t *testing.T, mk TaskLatestEventFactory) {
	t.Helper()
	ctx := context.Background()

	// seed creates a CI task spawned by one failing check and returns the
	// fixture, the task, and a recorder for further events on the same
	// entity.
	seed := func(t *testing.T, suffix string) (TaskLatestEventFixture, domain.Task, func(meta string) string) {
		t.Helper()
		f := mk(t)
		stores, orgID := f.Stores, f.OrgID
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
		task, _, err := stores.Tasks.FindOrCreateAtSystem(ctx, orgID, f.TeamID, entity.ID,
			domain.EventGitHubPRCICheckFailed, "", spawnID, 0.5, time.Now().UTC())
		if err != nil || task == nil {
			t.Fatalf("seed task: %+v, %v", task, err)
		}
		return f, *task, record
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
		f, task, _ := seed(t, "spawned")
		stores, orgID := f.Stores, f.OrgID
		if err := stores.Tasks.RecordEventSystem(ctx, orgID, task.ID, task.PrimaryEventID, "spawned"); err != nil {
			t.Fatalf("record spawned row: %v", err)
		}
		if got := latest(t, stores, orgID, task.ID); got != task.PrimaryEventID {
			t.Errorf("LatestEventIDSystem = %q, want the spawning event %q", got, task.PrimaryEventID)
		}
	})

	t.Run("the_bumping_event_after_a_bump", func(t *testing.T) {
		f, task, record := seed(t, "bumped")
		stores, orgID := f.Stores, f.OrgID
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
		f, task, _ := seed(t, "close-resume")
		stores, orgID := f.Stores, f.OrgID
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
		f, task, _ := seed(t, "none")
		stores, orgID := f.Stores, f.OrgID
		if got := latest(t, stores, orgID, task.ID); got != "" {
			t.Errorf("LatestEventIDSystem = %q, want \"\" for a task with no task_events row", got)
		}
	})

	after := func(t *testing.T, f TaskLatestEventFixture, taskID, eventID string) string {
		t.Helper()
		got, err := f.Stores.Tasks.LatestEventAfterSystem(ctx, f.OrgID, taskID, eventID)
		if err != nil {
			t.Fatalf("LatestEventAfterSystem: %v", err)
		}
		return got
	}

	// spawnedAndBumped seeds a task spawned by one event and bumped by a
	// second recorded after it, stamps each event's created_at, and returns
	// the fixture, the task, and the bumping event.
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	spawnedAndBumped := func(t *testing.T, suffix string, spawnedAt, bumpedAt time.Time) (TaskLatestEventFixture, domain.Task, string) {
		t.Helper()
		f, task, record := seed(t, suffix)
		if err := f.Stores.Tasks.RecordEventSystem(ctx, f.OrgID, task.ID, task.PrimaryEventID, "spawned"); err != nil {
			t.Fatalf("record spawned row: %v", err)
		}
		bumpID := record(`{"check_name":"links"}`)
		if err := f.Stores.Tasks.RecordEventSystem(ctx, f.OrgID, task.ID, bumpID, "bumped"); err != nil {
			t.Fatalf("record bumped row: %v", err)
		}
		f.SetCreatedAt(t, task.PrimaryEventID, spawnedAt)
		f.SetCreatedAt(t, bumpID, bumpedAt)
		return f, task, bumpID
	}

	t.Run("after_an_event_the_later_bump", func(t *testing.T) {
		f, task, bumpID := spawnedAndBumped(t, "after-later", at, at.Add(time.Second))
		if got := after(t, f, task.ID, task.PrimaryEventID); got != bumpID {
			t.Errorf("LatestEventAfterSystem(spawn) = %q, want the bump %q", got, bumpID)
		}
		if got := after(t, f, task.ID, bumpID); got != "" {
			t.Errorf("LatestEventAfterSystem(bump) = %q, want \"\": nothing is newer than the newest event", got)
		}
	})

	// created_at orders the events, not the order they were recorded in.
	t.Run("after_an_event_ordered_by_created_at", func(t *testing.T) {
		f, task, bumpID := spawnedAndBumped(t, "after-created-at", at, at.Add(-time.Second))
		if got := after(t, f, task.ID, task.PrimaryEventID); got != "" {
			t.Errorf("LatestEventAfterSystem(spawn) = %q, want \"\": the bump is older", got)
		}
		if got := after(t, f, task.ID, bumpID); got != task.PrimaryEventID {
			t.Errorf("LatestEventAfterSystem(bump) = %q, want the spawning event %q", got, task.PrimaryEventID)
		}
	})

	t.Run("after_an_event_missing_from_the_timeline", func(t *testing.T) {
		f, task, record := seed(t, "after-missing")
		if err := f.Stores.Tasks.RecordEventSystem(ctx, f.OrgID, task.ID, task.PrimaryEventID, "spawned"); err != nil {
			t.Fatalf("record spawned row: %v", err)
		}
		f.SetCreatedAt(t, task.PrimaryEventID, at)
		older, newer := record(`{"check_name":"older"}`), record(`{"check_name":"newer"}`)
		f.SetCreatedAt(t, older, at.Add(-time.Second))
		f.SetCreatedAt(t, newer, at.Add(time.Second))
		if got := after(t, f, task.ID, older); got != task.PrimaryEventID {
			t.Errorf("LatestEventAfterSystem(older) = %q, want the spawning event %q", got, task.PrimaryEventID)
		}
		if got := after(t, f, task.ID, newer); got != "" {
			t.Errorf("LatestEventAfterSystem(newer) = %q, want \"\": the timeline holds nothing newer", got)
		}
	})

	// Two events share a created_at. Only an insertion-ordered dialect can
	// say the bump, recorded second, is the newer; neither may hand back the
	// spawning event as newer than the bump.
	t.Run("at_a_created_at_tie", func(t *testing.T) {
		f, task, bumpID := spawnedAndBumped(t, "tie", at, at)
		if got := after(t, f, task.ID, bumpID); got != "" {
			t.Errorf("LatestEventAfterSystem(bump) = %q, want \"\": the spawning event was recorded first", got)
		}
		wantAfterSpawn := ""
		if f.InsertionOrdered {
			wantAfterSpawn = bumpID
		}
		if got := after(t, f, task.ID, task.PrimaryEventID); got != wantAfterSpawn {
			t.Errorf("LatestEventAfterSystem(spawn) = %q, want %q (insertion ordered: %v)", got, wantAfterSpawn, f.InsertionOrdered)
		}
		if f.InsertionOrdered {
			if got := latest(t, f.Stores, f.OrgID, task.ID); got != bumpID {
				t.Errorf("LatestEventIDSystem = %q, want the bump %q, recorded second", got, bumpID)
			}
		}
	})
}
