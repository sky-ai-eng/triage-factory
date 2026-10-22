package dbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskRequestedReviewerFactory is what a per-backend test hands to
// RunTaskRequestedReviewerConformance: the wired stores, the org every call
// names, and the team its tasks belong to.
type TaskRequestedReviewerFactory func(t *testing.T) (stores db.Stores, orgID, teamID string)

// RunTaskRequestedReviewerConformance pins domain.Task.RequestedReviewer
// across the task reads: a review_requested task reads the requested user's
// login, or the requested team's org/slug, off the event that last bumped it,
// whatever its dedup_key holds; every other task reads empty.
func RunTaskRequestedReviewerConformance(t *testing.T, mk TaskRequestedReviewerFactory) {
	t.Helper()
	ctx := context.Background()

	for i, c := range []struct {
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
			stores, orgID, teamID := mk(t)
			entity, _, err := stores.Entities.FindOrCreateSystem(ctx, orgID, "github", TestGitHubHost,
				fmt.Sprintf("octo/reviewer#%d", i+1), "", "pr", "Reviewer", "")
			if err != nil {
				t.Fatalf("seed entity: %v", err)
			}
			eventID, err := stores.Events.RecordSystem(ctx, orgID, domain.Event{
				EntityID: &entity.ID, EventType: c.eventType, DedupKey: c.dedupKey, MetadataJSON: c.metadata,
			})
			if err != nil {
				t.Fatalf("seed event: %v", err)
			}
			seeded, _, err := stores.Tasks.FindOrCreateAtSystem(ctx, orgID, teamID, entity.ID, c.eventType, c.dedupKey, eventID, 0.5, time.Now().UTC())
			if err != nil {
				t.Fatalf("seed task: %v", err)
			}
			task, err := stores.Tasks.Get(ctx, orgID, seeded.ID)
			if err != nil || task == nil {
				t.Fatalf("Get = %+v, %v", task, err)
			}
			if task.RequestedReviewer != c.want {
				t.Errorf("RequestedReviewer = %q, want %q", task.RequestedReviewer, c.want)
			}
		})
	}
}
