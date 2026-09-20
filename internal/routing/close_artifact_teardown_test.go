package routing

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// A task's artifacts survive every boundary but the last one. The close cascade
// IS that last one for a task nobody dispositioned by hand — the PR merged
// under it, the entity found already terminal by the reconciler — so it retires
// what the work left behind, with rows saying no person authorized it.

// teardownResolver is a ghclient.Resolver that hands out one client pointed at
// a test GitHub, so the draft-PR close the teardown makes is observable. Only
// the two arms the teardown uses answer; the rest panic so a test that starts
// depending on them says so.
type teardownResolver struct{ client *ghclient.Client }

func (r teardownResolver) ClientFor(context.Context, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r teardownResolver) ClientForRepo(context.Context, string, string, string) (*ghclient.Client, error) {
	return r.client, nil
}

func (r teardownResolver) TokenFor(context.Context, string, string) (githubapp.Token, error) {
	panic("TokenFor is not used by the artifact teardown")
}

func (r teardownResolver) BaseURLFor(context.Context, string) (string, error) {
	panic("BaseURLFor is not used by the artifact teardown")
}

func (r teardownResolver) OrgIdentityFor(context.Context, string) (string, string, bool) {
	panic("OrgIdentityFor is not used by the artifact teardown")
}

// seedUnresolvedArtifacts hangs a draft PR and a finalized pending review off
// conversationID — the two shapes a stopped run strands. Returns their ids.
func seedUnresolvedArtifacts(t *testing.T, database *sql.DB, conversationID, repoPath string, number int) (prID, reviewID string) {
	t.Helper()
	store := sqlitestore.New(database)
	pr := domain.NewPullRequestArtifact(repoPath, number, "PR_node", "tf/fix", "main",
		"https://example.test/"+repoPath+"/pull/7", "Proposed title", "Proposed body", true)
	pr.ConversationID = conversationID
	pr.OrgID = runmode.LocalDefaultOrgID
	pr.TeamID = runmode.LocalDefaultTeamID
	storedPR, err := store.Artifacts.UpsertSystem(t.Context(), runmode.LocalDefaultOrgID, pr)
	if err != nil {
		t.Fatalf("seed draft PR artifact: %v", err)
	}

	rv := domain.NewReviewArtifact(repoPath, number, "headsha", conversationID)
	rv.ConversationID = conversationID
	rv.OrgID = runmode.LocalDefaultOrgID
	rv.TeamID = runmode.LocalDefaultTeamID
	rd, _ := domain.ParseReviewArtifactDetails(rv.DetailsJSON)
	rd.ReviewBody = "agent draft body"
	rd.ReviewEvent = "COMMENT"
	rv.DetailsJSON = domain.MarshalReviewArtifactDetails(rd)
	storedReview, err := store.Artifacts.UpsertSystem(t.Context(), runmode.LocalDefaultOrgID, rv)
	if err != nil {
		t.Fatalf("seed review artifact: %v", err)
	}
	return storedPR.ID, storedReview.ID
}

// teardownSpawner is a real delegate.Spawner wired to the test DB and a GitHub
// stub — the System adapter under test is a method on it, so a stub delegator
// would prove nothing about what the teardown actually writes. The returned
// recorder holds every state the PR-close PATCH was sent.
func teardownSpawner(t *testing.T, database *sql.DB) (*delegate.Spawner, *closeRecorder) {
	t.Helper()
	closes := &closeRecorder{}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("unexpected %s %s — the teardown closes PRs and touches nothing else", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		state, _ := body["state"].(string)
		closes.record(state)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
	}))
	t.Cleanup(stub.Close)

	sp := delegate.NewSpawner(database, sqlitestore.New(database), nil, websocket.NewHub(), "haiku")
	sp.SetRunCredentialResolvers(teardownResolver{client: ghclient.NewClient(stub.URL, "pat")}, nil, nil)
	return sp, closes
}

// closeRecorder is the GitHub end of the teardown: it remembers every state the
// PR-close PATCH was sent.
type closeRecorder struct {
	mu     sync.Mutex
	states []string
}

func (c *closeRecorder) record(state string) {
	c.mu.Lock()
	c.states = append(c.states, state)
	c.mu.Unlock()
}

func (c *closeRecorder) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.states...)
}

func artifactState(t *testing.T, database *sql.DB, artifactID string) string {
	t.Helper()
	var state string
	if err := database.QueryRow(`SELECT state FROM artifacts WHERE id = ?`, artifactID).Scan(&state); err != nil {
		t.Fatalf("read artifact %s: %v", artifactID, err)
	}
	return state
}

// TestCloseCascade_TearsDownTheTasksArtifacts is the headline: a merge closes
// the task, and the artifacts its run left go with it. The draft PR is closed
// TF-side and on GitHub; the staged review is dismissed with no GitHub call at
// all; and the audit row for the close names no actor, because no person asked
// for any of it.
func TestCloseCascade_TearsDownTheTasksArtifacts(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp, closes := teardownSpawner(t, database)
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#teardown")
	_, convID := seedRunOnTask(t, database, taskID, "completed", "completed")
	prID, reviewID := seedUnresolvedArtifacts(t, database, convID, "owner/repo", 7)

	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Fatalf("active tasks = %d, want 0 — the merge closes the task", n)
	}
	if got := artifactState(t, database, prID); got != domain.ArtifactStatePRClosed {
		t.Errorf("draft PR artifact state = %q, want %q", got, domain.ArtifactStatePRClosed)
	}
	if got := artifactState(t, database, reviewID); got != domain.ArtifactStateReviewDismissed {
		t.Errorf("review artifact state = %q, want %q", got, domain.ArtifactStateReviewDismissed)
	}
	if got := closes.recorded(); len(got) != 1 || got[0] != "closed" {
		t.Errorf("PR closes sent to GitHub = %v, want exactly [closed]", got)
	}

	var actor sql.NullString
	var credential string
	if err := database.QueryRow(
		`SELECT actor_user_id, credential FROM external_actions WHERE conversation_id = ? AND action = ?`,
		convID, domain.ActionPRClosed,
	).Scan(&actor, &credential); err != nil {
		t.Fatalf("read the pr_closed audit row: %v", err)
	}
	if actor.Valid {
		t.Errorf("actor_user_id = %q, want NULL — an event-driven close has no human authorizer", actor.String)
	}
	if credential != domain.CredentialGitHubApp {
		t.Errorf("credential = %q, want %q", credential, domain.CredentialGitHubApp)
	}
	// The staged review is TF-side, so retiring it is a local flip and belongs
	// in no log of org-credential writes.
	var reviewRows int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM external_actions WHERE conversation_id = ? AND action <> ?`,
		convID, domain.ActionPRClosed,
	).Scan(&reviewRows); err != nil {
		t.Fatalf("count non-close audit rows: %v", err)
	}
	if reviewRows != 0 {
		t.Errorf("audit rows beyond the PR close = %d, want 0", reviewRows)
	}
}

// TestCloseCascade_ObligationCloseTearsDownToo pins the other door into the
// same cascade. The poll's close obligation closes a task nobody dispositioned
// and no transition reached — a close lost upstream — through the same
// terminating close a real transition takes, so it inherits the teardown
// rather than needing its own.
func TestCloseCascade_ObligationCloseTearsDownToo(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp, closes := teardownSpawner(t, database)
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "octo/repo#9")
	_, convID := seedRunOnTask(t, database, taskID, "completed", "completed")
	prID, reviewID := seedUnresolvedArtifacts(t, database, convID, "octo/repo", 9)

	// The divergence the obligation repairs: the entity's snapshot says
	// merged, but no close ever ran against it. The next poll observes the
	// merged state again and records the obligation.
	if _, err := sqlitestore.New(database).Entities.UpdateSnapshot(context.Background(), runmode.LocalDefaultOrgID, entityID, prSnapshotJSON(t, "MERGED")); err != nil {
		t.Fatalf("stamp a terminal snapshot: %v", err)
	}
	pollGitHub(t, database, newFakeGitHub(t, "MERGED"))
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("drainEventQueue: %v", err)
	}

	if n := activeTaskCount(t, database, entityID); n != 0 {
		t.Fatalf("active tasks = %d, want 0 — the obligation closes the task", n)
	}
	if got := artifactState(t, database, prID); got != domain.ArtifactStatePRClosed {
		t.Errorf("draft PR artifact state = %q, want %q", got, domain.ArtifactStatePRClosed)
	}
	if got := artifactState(t, database, reviewID); got != domain.ArtifactStateReviewDismissed {
		t.Errorf("review artifact state = %q, want %q", got, domain.ArtifactStateReviewDismissed)
	}
	if got := closes.recorded(); len(got) != 1 || got[0] != "closed" {
		t.Errorf("PR closes sent to GitHub = %v, want exactly [closed]", got)
	}
}

// TestCloseCascade_ReplayedCloseTearsNothingDown bounds it. A replayed close
// finds the task already terminal and writes nothing, so it must not reach the
// teardown either — the artifacts under an already-closed task may be a resume's,
// and a second pass has no way to tell.
func TestCloseCascade_ReplayedCloseTearsNothingDown(t *testing.T) {
	database := newTestDB(t)
	r := newQueueWorkerRouter(t, database)
	sp := &stubDelegator{db: database}
	r.spawner = sp

	entityID, taskID := seedCIFailedTaskOnEntity(t, r, database, "owner/repo#replayed-teardown")
	seedRunOnTask(t, database, taskID, "completed", "completed")

	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if got := sp.tornDownCopy(); len(got) != 1 || got[0] != taskID {
		t.Fatalf("teardown calls after the close = %v, want exactly [%s]", got, taskID)
	}

	// The same event again: the close phase is a routing obligation, so a
	// failed pass replays the whole event.
	enqueueMerged(t, database, entityID)
	if err := r.drainEventQueue(context.Background()); err != nil {
		t.Fatalf("replay drain: %v", err)
	}
	if got := sp.tornDownCopy(); len(got) != 1 {
		t.Errorf("teardown calls after the replay = %v, want the first one only", got)
	}
}
