package routing

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/github/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// A close cleans up none of the task's deliverables. The one deliverable a
// pull request's end retires is a staged review of it, which can no longer be
// posted, and that dismissal re-checks the task the review held open.

// silentResolver hands out one client pointed at a GitHub stub that fails the
// test on any request: nothing the close does may reach GitHub. The arms the
// spawner does not use panic so a test that starts depending on them says so.
type silentResolver struct{ client *ghclient.Client }

func (r silentResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r silentResolver) ClientForInstallation(context.Context, string, domain.OrgGitHubAppInstallation) (*ghclient.Client, error) {
	return r.client, nil
}

func (r silentResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r silentResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	panic("TokenFor is not used by a close")
}

func (r silentResolver) BaseURLFor(context.Context, string) (string, error) {
	panic("BaseURLFor is not used by a close")
}

func (r silentResolver) OrgIdentityFor(context.Context, string) (string, string, bool, error) {
	panic("OrgIdentityFor is not used by a close")
}

// seedUnresolvedDeliverables hangs a draft PR and a finalized pending review off
// conversationID, both about the pull request entityID names. Returns their ids.
func seedUnresolvedDeliverables(t *testing.T, database *sql.DB, conversationID, entityID, repoPath string, number int) (prID, reviewID string) {
	t.Helper()
	store := sqlitestore.New(database)
	pr := domain.NewPullRequestDeliverable(repoPath, number, fmt.Sprintf("PR_%s_%d", repoPath, number), "tf/fix", "main",
		"https://example.test/"+repoPath+"/pull/7", "Proposed title", "Proposed body", true)
	pr.ConversationID = conversationID
	pr.OrgID = runmode.LocalDefaultOrgID
	pr.TeamID = runmode.LocalDefaultTeamID
	pr.EntityID = entityID
	storedPR, err := store.Deliverables.UpsertSystem(t.Context(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, store.Orgs, runmode.LocalDefaultOrgID, pr))
	if err != nil {
		t.Fatalf("seed draft PR deliverable: %v", err)
	}
	return storedPR.ID, seedPendingReview(t, database, conversationID, entityID, repoPath, number)
}

// seedPendingReview hangs a finalized pending review of the pull request
// entityID names off conversationID, and returns its id.
func seedPendingReview(t *testing.T, database *sql.DB, conversationID, entityID, repoPath string, number int) string {
	t.Helper()
	store := sqlitestore.New(database)
	rv := domain.NewReviewDeliverable(repoPath, number, fmt.Sprintf("PR_%s_%d", repoPath, number), "headsha", conversationID)
	rv.ConversationID = conversationID
	rv.OrgID = runmode.LocalDefaultOrgID
	rv.TeamID = runmode.LocalDefaultTeamID
	rv.EntityID = entityID
	rd, _ := domain.ParseReviewDeliverableDetails(rv.DetailsJSON)
	rd.ReviewBody = "agent draft body"
	rd.ReviewEvent = "COMMENT"
	rv.DetailsJSON = domain.MarshalReviewDeliverableDetails(rd)
	stored, err := store.Deliverables.UpsertSystem(t.Context(), runmode.LocalDefaultOrgID, dbtest.InOrgScope(t, store.Orgs, runmode.LocalDefaultOrgID, rv))
	if err != nil {
		t.Fatalf("seed review deliverable: %v", err)
	}
	return stored.ID
}

// realSpawner is a delegate.Spawner wired to the test DB, a GitHub stub that
// fails the test on any request, and a hub whose broadcasts the returned
// recorder captures: the follow-up of a dismissal is a method on it, so a stub
// delegator would prove nothing about what that follow-up sends or closes.
func realSpawner(t *testing.T, database *sql.DB) (*delegate.Spawner, *frameRecorder) {
	t.Helper()
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected %s %s — a close makes no GitHub call", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(stub.Close)

	hub := websocket.NewHub()
	frames := &frameRecorder{}
	hub.SetBackplane(frames)
	sp := delegate.NewSpawner(database, sqlitestore.New(database), nil, hub, "haiku")
	sp.SetRunCredentialResolvers(silentResolver{client: ghclient.NewClient(stub.URL, "pat")}, nil, nil)
	return sp, frames
}

// frameRecorder is the hub's backplane: it remembers every frame broadcast.
type frameRecorder struct {
	mu     sync.Mutex
	events []websocket.Event
}

func (f *frameRecorder) Publish(evt websocket.Event) {
	f.mu.Lock()
	f.events = append(f.events, evt)
	f.mu.Unlock()
}

// ofType is the frames of eventType broadcast so far.
func (f *frameRecorder) ofType(eventType string) []websocket.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []websocket.Event
	for _, e := range f.events {
		if e.Type == eventType {
			out = append(out, e)
		}
	}
	return out
}

func deliverableState(t *testing.T, database *sql.DB, deliverableID string) string {
	t.Helper()
	var state string
	if err := database.QueryRow(`SELECT state FROM deliverables WHERE id = ?`, deliverableID).Scan(&state); err != nil {
		t.Fatalf("read deliverable %s: %v", deliverableID, err)
	}
	return state
}

func externalActionCount(t *testing.T, database *sql.DB, conversationID string) int {
	t.Helper()
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM external_actions WHERE conversation_id = ?`, conversationID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// TestCloseCascade_LeavesTheDraftAndDismissesTheMergedPullRequestsReview is
// the headline: a merge closes the task, the draft PR its run left stays a
// draft with nothing sent to GitHub, and the staged review of the merged pull
// request is dismissed and announced on its conversation.
func TestCloseCascade_LeavesTheDraftAndDismissesTheMergedPullRequestsReview(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp, frames := realSpawner(t, database)
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#7")
	_, convID := seedRunOnTask(t, database, taskID, "completed", dbtest.SeedConcluded)
	prID, reviewID := seedUnresolvedDeliverables(t, database, convID, entityID, "owner/repo", 7)

	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Fatalf("active tasks = %d, want 0 — the merge closes the task", n)
	}
	if got := deliverableState(t, database, prID); got != domain.DeliverableStatePRDraft {
		t.Errorf("draft PR deliverable state = %q, want %q — a close leaves it for a person", got, domain.DeliverableStatePRDraft)
	}
	if got := deliverableState(t, database, reviewID); got != domain.DeliverableStateReviewDismissed {
		t.Errorf("review deliverable state = %q, want %q", got, domain.DeliverableStateReviewDismissed)
	}
	if n := externalActionCount(t, database, convID); n != 0 {
		t.Errorf("audit rows = %d, want 0 — the close wrote nothing on GitHub", n)
	}
	got := frames.ofType("deliverable_updated")
	if len(got) != 1 || got[0].ConversationID != convID {
		t.Fatalf("deliverable_updated frames = %+v, want one on %s", got, convID)
	}
}

// TestCloseCascade_ADismissedReviewClosesTheTaskItHeldOpen: a run on one task
// staged a review of another pull request and finished, so its task waits on
// that review alone. The reviewed pull request's merge dismisses the review,
// and the task closes by the rule a person's resolution of it would have
// applied.
func TestCloseCascade_ADismissedReviewClosesTheTaskItHeldOpen(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp, _ := realSpawner(t, database)
	r.spawner = sp
	if _, err := database.Exec(`INSERT OR IGNORE INTO agents (id, org_id, display_name) VALUES (?, ?, 'Test Bot')`,
		runmode.LocalDefaultAgentID, runmode.LocalDefaultOrgID); err != nil {
		t.Fatalf("seed agent: %v", err)
	}

	reviewedID, _ := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#21")
	_, heldTaskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#22")
	_, convID := seedRunOnTask(t, database, heldTaskID, "completed", dbtest.SeedConcluded)
	if _, err := database.Exec(`UPDATE tasks SET team_id = ?, claimed_by_agent_id = ?, status = 'in_progress' WHERE id = ?`,
		runmode.LocalDefaultTeamID, runmode.LocalDefaultAgentID, heldTaskID); err != nil {
		t.Fatalf("claim the held task for the agent: %v", err)
	}
	reviewID := seedPendingReview(t, database, convID, reviewedID, "owner/repo", 21)

	enqueueMerged(t, database, reviewedID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	if got := deliverableState(t, database, reviewID); got != domain.DeliverableStateReviewDismissed {
		t.Errorf("review deliverable state = %q, want %q", got, domain.DeliverableStateReviewDismissed)
	}
	status, reason := taskCloseReason(t, database, heldTaskID)
	if status != "done" || reason != "run_completed" {
		t.Errorf("held task = (%q, %q), want (done, run_completed)", status, reason)
	}
}

// TestCloseCascade_ReplayedCloseDismissesNothing bounds it. A replayed close
// finds the entity already closed and writes nothing, so it hands the
// follow-up nothing either.
func TestCloseCascade_ReplayedCloseDismissesNothing(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stubDelegator{db: database}
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#replayed-dismissal")
	_, convID := seedRunOnTask(t, database, taskID, "completed", dbtest.SeedConcluded)
	reviewID := seedPendingReview(t, database, convID, entityID, "owner/repo", 3)

	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if got := sp.dismissedCopy(); len(got) != 1 || got[0].DeliverableID != reviewID || got[0].ConversationID != convID {
		t.Fatalf("dismissed reviews after the close = %+v, want exactly %s on %s", got, reviewID, convID)
	}

	// The same event again: the close phase is a routing obligation, so a
	// failed pass replays the whole event.
	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("replay drain: %v", err)
	}
	if got := sp.dismissedCopy(); len(got) != 1 {
		t.Errorf("dismissed reviews after the replay = %+v, want the first one only", got)
	}
}
