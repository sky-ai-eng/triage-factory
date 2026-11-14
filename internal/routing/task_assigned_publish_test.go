package routing

import (
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// publishedAssignments decodes the system:task:assigned events the router
// published.
func publishedAssignments(t *testing.T, pub *fakeDispositionPublisher) []events.SystemTaskAssignedMetadata {
	t.Helper()
	var out []events.SystemTaskAssignedMetadata
	for _, evt := range pub.eventsOfType(domain.EventSystemTaskAssigned) {
		if evt.OrgID != runmode.LocalDefaultOrgID || evt.EntityID != nil {
			t.Errorf("assignment event = org %q entity %v, want the task's org and no entity", evt.OrgID, evt.EntityID)
		}
		var meta events.SystemTaskAssignedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			t.Fatalf("decode metadata %q: %v", evt.MetadataJSON, err)
		}
		out = append(out, meta)
	}
	return out
}

// TestTaskAssigned_PublishedWhenTheAgentsStampLands pins the router's half of
// the trigger: the agent's claim stamp landing publishes one
// system:task:assigned naming the task and the agent, from either place the
// router learns of a landed stamp, and a stamp the store refused publishes
// none.
func TestTaskAssigned_PublishedWhenTheAgentsStampLands(t *testing.T) {
	// fire runs the trigger, whose stamp rides the run's transaction and is
	// read back from the row.
	fire := func(t *testing.T, s claimScenario) {
		t.Helper()
		if fired := mustAutoDelegate(t, s.router, s.task, s.trigger, s.entity, s.event, runmode.LocalDefaultTeamID); !fired {
			t.Fatal("expected the trigger to fire")
		}
	}
	cases := []struct {
		name string
		act  func(t *testing.T, s claimScenario)
		want int
	}{
		{name: "a fire whose stamp lands", act: fire, want: 1},
		{name: "a fire whose stamp a person's claim refused", act: func(t *testing.T, s claimScenario) {
			tasks := testTaskStore(s.db)
			if ok, err := tasks.ClaimQueuedForUser(t.Context(), runmode.LocalDefaultOrgID, s.task.ID, runmode.LocalDefaultUserID,
				dbtest.TaskVersion(t, tasks, runmode.LocalDefaultOrgID, s.task.ID)); err != nil || !ok {
				t.Fatalf("ClaimQueuedForUser: ok=%v err=%v", ok, err)
			}
			fire(t, s)
		}},
		{name: "a second fire under the claim the agent holds", act: func(t *testing.T, s claimScenario) {
			fire(t, s)
			s.router.syncClaimAfterCommit(t.Context(), runmode.LocalDefaultOrgID, s.task, runmode.LocalDefaultAgentID)
		}, want: 1},
		{name: "a stamp that landed in its own commitment", act: func(t *testing.T, s claimScenario) {
			s.router.claimCommitted(runmode.LocalDefaultOrgID, s.task, runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID, true)
		}, want: 1},
		{name: "a stamp its own commitment refused", act: func(t *testing.T, s claimScenario) {
			s.router.claimCommitted(runmode.LocalDefaultOrgID, s.task, runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID, false)
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := setupClaimScenario(t, "assigned-"+string(rune('a'+i)))
			pub := &fakeDispositionPublisher{}
			s.router.publisher = pub

			tc.act(t, s)

			got := publishedAssignments(t, pub)
			if len(got) != tc.want {
				t.Fatalf("published %d assignment events, want %d: %+v", len(got), tc.want, got)
			}
			want := events.SystemTaskAssignedMetadata{TaskID: s.task.ID, AgentID: runmode.LocalDefaultAgentID}
			for _, meta := range got {
				if meta != want {
					t.Errorf("metadata = %+v, want %+v", meta, want)
				}
			}
		})
	}
}
