package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/eventing/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestTaskAssigned_PublishedByEveryDoorThatAssigns pins which writes publish
// system:task:assigned: each of the server's five doors that give a task a
// new assignee publishes one, naming the task and who holds it now, and a
// re-claim by the person who already holds the task publishes none.
func TestTaskAssigned_PublishedByEveryDoorThatAssigns(t *testing.T) {
	const fromUser = "00000000-0000-0000-0000-0000000000a1"
	const toUser = "00000000-0000-0000-0000-0000000000a2"
	local := runmode.LocalDefaultUserID

	// claimTask posts the board's claim of a seeded task.
	claimTask := func(opts lifecycleTaskOpts, body map[string]any) func(t *testing.T, s *Server) string {
		return func(t *testing.T, s *Server) string {
			t.Helper()
			taskID := seedLifecycleTask(t, s.db, "assigned", opts)
			rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/claim", withTaskVersion(t, s, taskID, body))
			if rec.Code != http.StatusOK {
				t.Fatalf("claim status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			return taskID
		}
	}
	cases := []struct {
		name string
		// act drives the door and returns the task it assigned.
		act func(t *testing.T, s *Server) (taskID string)
		// want is the assignee the event names; nil for a door that
		// publishes nothing.
		want *events.SystemTaskAssignedMetadata
	}{
		{name: "a claim of an unclaimed task",
			act:  claimTask(lifecycleTaskOpts{}, map[string]any{}),
			want: &events.SystemTaskAssignedMetadata{UserID: local}},
		{name: "a takeover from the agent",
			act:  claimTask(lifecycleTaskOpts{status: "in_progress", claimedByAgentID: runmode.LocalDefaultAgentID}, map[string]any{}),
			want: &events.SystemTaskAssignedMetadata{UserID: local}},
		{name: "a reassignment to another person",
			act: func(t *testing.T, s *Server) string {
				for _, u := range []string{fromUser, toUser} {
					execSQL(t, s.db, `INSERT INTO users (id, display_name) VALUES (?, 'Seed User')`, u)
				}
				execSQL(t, s.db, `INSERT INTO memberships (user_id, team_id, role) VALUES (?, ?, 'member')`, toUser, runmode.LocalDefaultTeamID)
				return claimTask(lifecycleTaskOpts{status: "in_progress", claimedByUserID: fromUser}, map[string]any{"target_user_id": toUser})(t, s)
			},
			want: &events.SystemTaskAssignedMetadata{UserID: toUser}},
		{name: "a person's Delegate",
			act: func(t *testing.T, s *Server) string {
				taskID := seedLifecycleTask(t, s.db, "assigned", lifecycleTaskOpts{})
				rec := doJSON(t, s, http.MethodPost, "/api/tasks/"+taskID+"/delegate", withTaskVersion(t, s, taskID, map[string]any{}))
				if rec.Code != http.StatusOK {
					t.Fatalf("delegate status = %d, want 200; body=%s", rec.Code, rec.Body.String())
				}
				return taskID
			},
			want: &events.SystemTaskAssignedMetadata{AgentID: runmode.LocalDefaultAgentID}},
		{name: "a claim from the Jira stock deck",
			act: func(t *testing.T, s *Server) string {
				keyring.MockInit()
				seedStockConfig(t, s)
				seedStockTicket(t, s, "SKY-1")
				s.jiraResolver = &recordingJiraResolver{userClient: newJiraIssueFake(t, statusToDoID, "").client()}
				if rec, body := postStock(t, s, "claim", "e_stock_SKY-1"); body.Applied != 1 {
					t.Fatalf("stock claim = %d %s, want it applied", rec.Code, rec.Body.String())
				}
				var taskID string
				if err := s.db.QueryRow(`SELECT id FROM tasks WHERE entity_id = 'e_stock_SKY-1'`).Scan(&taskID); err != nil {
					t.Fatalf("read the claimed task: %v", err)
				}
				return taskID
			},
			want: &events.SystemTaskAssignedMetadata{UserID: local}},
		{name: "a re-claim by the person who holds the task",
			act: claimTask(lifecycleTaskOpts{status: "in_progress", claimedByUserID: local}, map[string]any{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			bus := eventbus.New()
			s.SetEventBus(bus)
			// One subscriber sees every event in publish order, so the
			// marker published after the door has run bounds what the door
			// published.
			const marker = "test:door-returned"
			got := make(chan domain.Event, 8)
			bus.Subscribe(eventbus.Subscriber{Name: "test-task-assigned", Handle: func(e domain.Event) { got <- e }})

			taskID := tc.act(t, s)
			bus.Publish(domain.Event{EventType: marker})

			var published []domain.Event
			for done := false; !done; {
				select {
				case e := <-got:
					switch e.EventType {
					case marker:
						done = true
					case domain.EventSystemTaskAssigned:
						published = append(published, e)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("the bus never delivered the marker")
				}
			}
			if tc.want == nil {
				if len(published) != 0 {
					t.Fatalf("published %d assignment events, want none: %+v", len(published), published)
				}
				return
			}
			if len(published) != 1 {
				t.Fatalf("published %d assignment events, want one: %+v", len(published), published)
			}
			evt := published[0]
			var meta events.SystemTaskAssignedMetadata
			if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
				t.Fatalf("decode metadata %q: %v", evt.MetadataJSON, err)
			}
			want := *tc.want
			want.TaskID = taskID
			if meta != want || evt.OrgID != runmode.LocalDefaultOrgID || evt.EntityID != nil {
				t.Errorf("event = org %q entity %v metadata %+v, want org %q, no entity, metadata %+v",
					evt.OrgID, evt.EntityID, meta, runmode.LocalDefaultOrgID, want)
			}
		})
	}
}
