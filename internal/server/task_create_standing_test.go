package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// POST /api/tasks creates the station's task (201) and finds it again (200).
// A Jira status change task keyed on the status id, whose events carry the
// name, is answered at its own key; a key nothing carries is a 422.
func TestTaskCreate_AnswersAnActiveTaskNoEventNames(t *testing.T) {
	s := newTestServer(t)
	entity := seedStation(t, s, "owner/repo#202", domain.EventGitHubPRCICheckPassed)
	eid := entity.ID
	if _, err := s.events.Record(context.Background(), runmode.LocalDefaultOrgID, domain.Event{
		EntityID: &eid, EventType: domain.EventJiraIssueStatusChanged, DedupKey: "In Review",
	}); err != nil {
		t.Fatalf("record event: %v", err)
	}
	post := func(dedupKey string) (int, taskJSON) {
		t.Helper()
		rec := doJSON(t, s, http.MethodPost, "/api/tasks", map[string]string{
			"entity_id":  entity.ID,
			"event_type": domain.EventJiraIssueStatusChanged,
			"dedup_key":  dedupKey,
		})
		var got taskJSON
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		return rec.Code, got
	}

	code, created := post("In Review")
	if code != http.StatusCreated || created.ID == "" || created.EntityID != entity.ID {
		t.Fatalf("POST under the event's key = %d %+v, want 201 and the station's task", code, created)
	}
	// The drop is idempotent: a second one finds the same row.
	if code, again := post("In Review"); code != http.StatusOK || again.ID != created.ID {
		t.Errorf("second POST = %d resolving %q, want 200 resolving %s", code, again.ID, created.ID)
	}
	execSQL(t, s.db, `UPDATE tasks SET dedup_key = '10003' WHERE id = ?`, created.ID)

	code, found := post("10003")
	if code != http.StatusOK || found.ID != created.ID {
		t.Errorf("POST under the task's key = %d resolving %q, want 200 resolving %s", code, found.ID, created.ID)
	}
	if code, _ := post("10001"); code != http.StatusUnprocessableEntity {
		t.Errorf("POST under a key nothing carries = %d, want 422", code)
	}
}
