package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// SourceStatusHarness is one backend's wiring for RunSourceStatusConformance.
// The suite is schema-blind: SeedTask owns the entity, event and task rows in
// the backend's own dialect.
type SourceStatusHarness struct {
	Tasks  db.TaskStore
	Swipes db.SwipeStore
	OrgID  string
	// UserID is the claimant, and the user a swipe written through Swipes is
	// attributed to, which UndoLastSwipe's guard is keyed on.
	UserID  string
	AgentID string
	// SeedTask creates a queued, unclaimed task on a fresh entity of source
	// whose snapshot_json is snapshot, and returns the task id.
	SeedTask func(t *testing.T, source, snapshot string) string
}

// RunSourceStatusConformance pins tasks.source_status: the user-claim doors
// record the state of the task's Jira or Linear entity snapshot in the UPDATE
// that writes the claim, nothing for any other source, and returning the task
// to the queue clears it.
func RunSourceStatusConformance(t *testing.T, factory func(t *testing.T) SourceStatusHarness) {
	t.Helper()
	ctx := context.Background()

	const (
		jiraSnapshot = `{"key":"SKY-1","status":"To Do","status_id":"10000",` +
			`"assignee":"Ann","assignee_account_id":"acc-ann"}`
		linearSnapshot = `{"identifier":"ENG-7","state":{"id":"st-todo","name":"Todo","type":"unstarted"},` +
			`"assignee":"Lin","assignee_user_id":"lin-user"}`
	)

	sourceStatus := func(t *testing.T, h SourceStatusHarness, taskID string) *domain.SourceStatus {
		t.Helper()
		task, err := h.Tasks.Get(ctx, h.OrgID, taskID)
		if err != nil || task == nil {
			t.Fatalf("Get(%s) = (%v, %v)", taskID, task, err)
		}
		return task.SourceStatus
	}
	claim := func(t *testing.T, h SourceStatusHarness, taskID string) {
		t.Helper()
		if ok, err := h.Tasks.ClaimQueuedForUser(ctx, h.OrgID, taskID, h.UserID); err != nil || !ok {
			t.Fatalf("ClaimQueuedForUser = (%v, %v), want the claim to land", ok, err)
		}
	}

	cases := []struct {
		name     string
		source   string
		snapshot string
		want     *domain.SourceStatus
	}{
		{"jira", "jira", jiraSnapshot, &domain.SourceStatus{StatusID: "10000", StatusName: "To Do", AssigneeID: "acc-ann"}},
		{"jira_unassigned", "jira", `{"key":"SKY-2","status":"Backlog","status_id":"10004","assignee":""}`, &domain.SourceStatus{StatusID: "10004", StatusName: "Backlog"}},
		{"jira_name_only", "jira", `{"key":"SKY-3","status":"To Do"}`, &domain.SourceStatus{StatusName: "To Do"}},
		{"linear", "linear", linearSnapshot, &domain.SourceStatus{StatusID: "st-todo", StatusName: "Todo", AssigneeID: "lin-user"}},
		{"github", "github", `{"state":"OPEN"}`, nil},
		{"jira_no_status", "jira", `{"key":"SKY-4"}`, nil},
	}
	for _, tc := range cases {
		t.Run("ClaimQueuedForUser_records_"+tc.name, func(t *testing.T) {
			h := factory(t)
			taskID := h.SeedTask(t, tc.source, tc.snapshot)
			if got := sourceStatus(t, h, taskID); got != nil {
				t.Fatalf("source_status before the claim = %+v, want nil", got)
			}
			claim(t, h, taskID)
			got := sourceStatus(t, h, taskID)
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("source_status = %+v, want %+v", got, tc.want)
			}
		})
	}

	t.Run("TakeoverClaimFromAgent_records_the_snapshot_state", func(t *testing.T) {
		h := factory(t)
		taskID := h.SeedTask(t, "jira", jiraSnapshot)
		if ok, err := h.Tasks.StampAgentClaimIfUnclaimed(ctx, h.OrgID, taskID, h.AgentID, ""); err != nil || !ok {
			t.Fatalf("StampAgentClaimIfUnclaimed = (%v, %v)", ok, err)
		}
		if got := sourceStatus(t, h, taskID); got != nil {
			t.Fatalf("source_status after the agent's claim = %+v, want nil: only a user claim records it", got)
		}
		if ok, err := h.Tasks.TakeoverClaimFromAgent(ctx, h.OrgID, taskID, h.UserID); err != nil || !ok {
			t.Fatalf("TakeoverClaimFromAgent = (%v, %v)", ok, err)
		}
		want := domain.SourceStatus{StatusID: "10000", StatusName: "To Do", AssigneeID: "acc-ann"}
		if got := sourceStatus(t, h, taskID); got == nil || *got != want {
			t.Errorf("source_status = %+v, want %+v", got, want)
		}
	})

	t.Run("RequeueTask_clears_it", func(t *testing.T) {
		h := factory(t)
		taskID := h.SeedTask(t, "linear", linearSnapshot)
		claim(t, h, taskID)
		if ok, err := h.Swipes.RequeueTask(ctx, h.OrgID, taskID); err != nil || !ok {
			t.Fatalf("RequeueTask = (%v, %v)", ok, err)
		}
		if got := sourceStatus(t, h, taskID); got != nil {
			t.Errorf("source_status after requeue = %+v, want nil", got)
		}
	})

	t.Run("UndoLastSwipe_clears_it", func(t *testing.T) {
		h := factory(t)
		taskID := h.SeedTask(t, "jira", jiraSnapshot)
		claim(t, h, taskID)
		if _, audited, err := h.Swipes.RecordSwipe(ctx, h.OrgID, taskID, "claim", 0, nil); err != nil || !audited {
			t.Fatalf("RecordSwipe = (%v, %v)", audited, err)
		}
		if ok, err := h.Swipes.UndoLastSwipe(ctx, h.OrgID, taskID, h.UserID); err != nil || !ok {
			t.Fatalf("UndoLastSwipe = (%v, %v)", ok, err)
		}
		if got := sourceStatus(t, h, taskID); got != nil {
			t.Errorf("source_status after undo = %+v, want nil", got)
		}
	})
}
