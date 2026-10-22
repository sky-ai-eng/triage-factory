package dbtest

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskWithEventSeeder inserts an entity, an event of eventType carrying
// metadataJSON, and a queued task keyed dedupKey whose primary event it is,
// returning the task's id.
type TaskWithEventSeeder func(t *testing.T, eventType, dedupKey, metadataJSON string) (taskID string)

// TaskRequestedReviewerFactory is what a per-backend test hands to
// RunTaskRequestedReviewerConformance: the wired TaskStore, the org every
// call names, and the seeder.
type TaskRequestedReviewerFactory func(t *testing.T) (store db.TaskStore, orgID string, seed TaskWithEventSeeder)

// RunTaskRequestedReviewerConformance pins domain.Task.RequestedReviewer
// across the task reads: a review_requested task reads the requested user's
// login, or the requested team's org/slug, off the event that last bumped it,
// whatever its dedup_key holds; every other task reads empty.
func RunTaskRequestedReviewerConformance(t *testing.T, mk TaskRequestedReviewerFactory) {
	t.Helper()
	ctx := context.Background()

	for _, c := range []struct {
		name, eventType, dedupKey, metadata, want string
	}{
		{"a user, keyed by GitHub id", domain.EventGitHubPRReviewRequested, "user_id:5",
			`{"requested_login":"alice","requested_user_id":"5"}`, "alice"},
		{"a team, keyed by GitHub id", domain.EventGitHubPRReviewRequested, "team_id:11",
			`{"requested_team":"acme/backend","requested_team_id":"11"}`, "acme/backend"},
		{"a user, keyed by login", domain.EventGitHubPRReviewRequested, "user:bob",
			`{"requested_login":"bob"}`, "bob"},
		{"another event type", domain.EventGitHubPRCICheckFailed, "",
			`{"requested_login":"alice"}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, orgID, seed := mk(t)
			taskID := seed(t, c.eventType, c.dedupKey, c.metadata)
			task, err := store.Get(ctx, orgID, taskID)
			if err != nil || task == nil {
				t.Fatalf("Get = %+v, %v", task, err)
			}
			if task.RequestedReviewer != c.want {
				t.Errorf("RequestedReviewer = %q, want %q", task.RequestedReviewer, c.want)
			}
		})
	}
}
