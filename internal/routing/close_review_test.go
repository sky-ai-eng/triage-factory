package routing

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TestCloseReviewResolved_ReviewerWhoseLatestRequestsChanges pins the
// changes-requested close for a reviewer who emits more than once in one poll.
// The tracker emits one event per review, so a reviewer who commented and then
// requested changes since the last poll emits review_commented ahead of
// review_changes_requested, against a snapshot whose latest review for them
// requests changes. The comment must not close the outstanding request; a
// comment that is the reviewer's latest review still does.
func TestCloseReviewResolved_ReviewerWhoseLatestRequestsChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		latest string
		want   string
	}{
		{name: "latest review requests changes", latest: "CHANGES_REQUESTED", want: "queued"},
		{name: "latest review is the comment", latest: "COMMENTED", want: "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := newTestDB(t)
			r := newQueueWorkerRouter(t, database)
			entityID := newEntity(t, database, "owner/repo#review-twice")
			taskID := seedTaskOfType(t, database, entityID, domain.EventGitHubPRReviewChangesRequested, "")
			snap, _ := json.Marshal(domain.PRSnapshot{Reviews: []domain.ReviewState{{Author: "bob", State: tc.latest}}})
			if _, err := database.Exec(`UPDATE entities SET snapshot_json = ? WHERE id = ?`, string(snap), entityID); err != nil {
				t.Fatalf("seed snapshot: %v", err)
			}

			enqueueReviewEvent(t, database, entityID, domain.EventGitHubPRReviewCommented, "bob")
			if err := r.drainEventQueue(context.Background()); err != nil {
				t.Fatalf("drainEventQueue: %v", err)
			}
			if status, _ := taskCloseReason(t, database, taskID); status != tc.want {
				t.Errorf("changes_requested task = %q, want %q", status, tc.want)
			}
		})
	}
}
