package tracker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The upstream state the first cycle after a re-enable finds: the PR merged
// while the source was paused, and picked up a review on the way. Both of those
// would emit against the pre-pause snapshot.
const reseedMergedPRNode = `{"id":"PR_node9","number":9,"title":"Paused PR","author":{"login":"bob"},
	 "state":"MERGED","merged":true,"url":"https://github.com/octo/repo/pull/9",
	 "repository":{"nameWithOwner":"octo/repo"},
	 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-20T00:00:00Z","mergedAt":"2026-06-20T00:00:00Z",
	 "reviews":{"nodes":[{"state":"APPROVED","author":{"login":"carol"},"submittedAt":"2026-06-19T00:00:00Z"}]}}`

const reseedMergedPRRefresh = `{"data":{"nodes":[` + reseedMergedPRNode + `]}}`

const reseedMergedPRBasic = `{
	"number": 9, "node_id": "PR_node9", "title": "Paused PR",
	"state": "closed", "merged": true,
	"html_url": "https://github.com/octo/repo/pull/9",
	"user": {"login": "bob"},
	"head": {"sha": "sha9b", "ref": "feat"}, "base": {"ref": "main"}
}`

// pausedPRSnapshot is the state a normal cycle left behind before the pause: an
// open PR, no reviews, an older head.
func pausedPRSnapshot() string {
	b, _ := json.Marshal(domain.PRSnapshot{
		NodeID: "PR_node9", Number: 9, Title: "Paused PR", Author: "bob",
		Repo: "octo/repo", URL: "https://github.com/octo/repo/pull/9",
		State: "OPEN", HeadRef: "feat", BaseRef: "main", HeadSHA: "sha9a",
	})
	return string(b)
}

// TestRefreshGitHub_ReEnableAfterPauseEmitsNothing is the event-source pause's
// re-enable acceptance: pause, let the upstream move, re-enable, poll — and the
// first cycle seeds silently.
//
// The pause clears the snapshot, which is the whole mechanism: without it the
// first cycle would diff a pause-old snapshot against now and emit every
// transition that happened while the org had the source turned off, minting a
// task for each. The control case below proves that is what would happen.
//
// The merged PR is the case that needs saying out loud. DiffPRSnapshots
// deliberately emits on FIRST discovery for a terminal state, because there
// will be no next diff to catch it — so a re-seed that went through the diff
// would fire pr_merged for a PR that merged during the pause. It does not go
// through the diff: a snapshot-less row takes the tracker's quiet-seed path,
// which writes the snapshot and emits nothing at all.
func TestRefreshGitHub_ReEnableAfterPauseEmitsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_, _ = w.Write([]byte(reseedMergedPRRefresh))
		case strings.Contains(r.URL.Path, "/pulls/9"):
			_, _ = w.Write([]byte(reseedMergedPRBasic))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	ent, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "octo/repo#9", "pr", "Paused PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := stores.Entities.UpdateSnapshot(ctx, org, ent.ID, pausedPRSnapshot()); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// The disable, exactly as the PATCH route performs it.
	if _, err := stores.Entities.ClearSnapshotsForSourceSystem(ctx, org, "github"); err != nil {
		t.Fatalf("clear snapshots: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, _, err := tr.RefreshGitHub(ctx, ghclient.NewClient(srv.URL, "tok"), "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Errorf("first cycle after re-enable emitted %d events, want 0: %v", len(evts), eventTypes(evts))
	}
	got, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#9")
	if err != nil || got == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", got, err)
	}
	if !strings.Contains(got.SnapshotJSON, "PR_node9") {
		t.Errorf("snapshot not re-seeded: %q", got.SnapshotJSON)
	}
	if got.State != "closed" {
		t.Errorf("state = %q, want closed — the seed still closes a terminal PR, it just does not announce it", got.State)
	}
}

// TestRefreshGitHub_PauseWithoutClearWouldEmit is the control for the test
// above: the same upstream movement against a snapshot that was NOT cleared
// emits, which is precisely the burst the clear exists to prevent.
func TestRefreshGitHub_PauseWithoutClearWouldEmit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			_, _ = w.Write([]byte(reseedMergedPRRefresh))
			return
		}
		t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		http.Error(w, "unexpected", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	ent, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "octo/repo#9", "pr", "Paused PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := stores.Entities.UpdateSnapshot(ctx, org, ent.ID, pausedPRSnapshot()); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	if _, _, err := tr.RefreshGitHub(ctx, ghclient.NewClient(srv.URL, "tok"), "", nil, nil); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}
	if evts := pub.nonSystemEvents(); len(evts) == 0 {
		t.Fatal("uncleared snapshot emitted nothing; the clear's whole purpose is to suppress what this cycle produces")
	}
}

// TestRefreshJira_ReEnableAfterPauseEmitsNothing is the Jira twin, and the
// sharper of the two: DiffJiraSnapshots' first-discovery branch emits for EVERY
// issue — assigned, available, or completed — so a re-seed that diffed would
// mint a task per known ticket, not just per terminal one.
func TestRefreshJira_ReEnableAfterPauseEmitsNothing(t *testing.T) {
	searchResp := `{"issues":[
		{"key":"SKY-9","fields":{
			"summary":"Paused issue","status":{"name":"Done"},
			"assignee":{"displayName":"Alice","accountId":"acc-1"},
			"created":"2026-06-01T00:00:00.000+0000","updated":"2026-06-20T00:00:00.000+0000"
		}}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(searchResp))
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID

	ent, _, err := stores.Entities.FindOrCreate(ctx, org, "jira", "SKY-9", "issue", "Paused issue", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	prev, _ := json.Marshal(domain.JiraSnapshot{
		Key: "SKY-9", Summary: "Paused issue", Status: "In Progress",
		Assignee: "Alice", AssigneeAccountID: "acc-1",
		UpdatedAt: "2026-06-01T00:00:00.000+0000",
	})
	if _, err := stores.Entities.UpdateSnapshot(ctx, org, ent.ID, string(prev)); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if _, err := stores.Entities.ClearSnapshotsForSourceSystem(ctx, org, "jira"); err != nil {
		t.Fatalf("clear snapshots: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	client := jiraclient.NewClient(jiraclient.DataCenterPAT(srv.URL, "pat"))
	rules := JiraRules{{Key: "SKY", DoneMembers: jiraRefs("Done")}}
	if _, err := tr.RefreshJira(ctx, client, srv.URL, rules); err != nil {
		t.Fatalf("RefreshJira: %v", err)
	}

	if evts := pub.nonSystemEvents(); len(evts) != 0 {
		t.Errorf("first cycle after re-enable emitted %d events, want 0: %v", len(evts), eventTypes(evts))
	}
	got, err := stores.Entities.GetBySource(ctx, org, "jira", "SKY-9")
	if err != nil || got == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", got, err)
	}
	if !strings.Contains(got.SnapshotJSON, "SKY-9") {
		t.Errorf("snapshot not re-seeded: %q", got.SnapshotJSON)
	}
	if got.State != "closed" {
		t.Errorf("state = %q, want closed — the issue reached Done while the source was paused", got.State)
	}
}

// reseedNewPRList is the open-PR listing the first cycle back finds: a PR that
// was opened while the source was off, so TF has never seen it, with a review
// already requested from a TF-known user.
const reseedNewPRList = `[
  {
    "number": 10, "node_id": "PR_node10", "title": "Opened during the pause",
    "state": "open", "draft": false,
    "html_url": "https://github.com/octo/repo/pull/10",
    "created_at": "2026-06-10T10:00:00Z", "updated_at": "2026-06-18T12:00:00Z",
    "user": {"login": "alice"},
    "head": {"sha": "sha10", "ref": "feature", "repo": {"full_name": "octo/repo"}},
    "base": {"ref": "main", "repo": {"full_name": "octo/repo"}},
    "requested_reviewers": [{"login": "carol"}],
    "requested_teams": [], "labels": []
  }
]`

// reseedNewPRNode is the Phase-2 refresh answer for the newly discovered PR:
// the same state discovery just seeded, so the only thing this cycle can emit
// about it is the discovery backfill.
const reseedNewPRNode = `{"id":"PR_node10","number":10,"title":"Opened during the pause","author":{"login":"alice"},
	 "state":"OPEN","merged":false,"url":"https://github.com/octo/repo/pull/10",
	 "repository":{"nameWithOwner":"octo/repo"},
	 "createdAt":"2026-06-10T10:00:00Z","updatedAt":"2026-06-18T12:00:00Z",
	 "headRefName":"feature","baseRefName":"main",
	 "reviewRequests":{"nodes":[{"requestedReviewer":{"login":"carol"}}]}}`

// reseedGraphQLFor answers a refresh with only the nodes the query actually
// asked for. The batches are split (the merged PR rides the terminal fragment,
// the open one the full fragment), so a stub that answered every query with the
// same node would hand one entity another's state and manufacture a diff that
// production could never produce.
func reseedGraphQLFor(body string) string {
	var nodes []string
	if strings.Contains(body, "PR_node9") {
		nodes = append(nodes, reseedMergedPRNode)
	}
	if strings.Contains(body, "PR_node10") {
		nodes = append(nodes, reseedNewPRNode)
	}
	return `{"data":{"nodes":[` + strings.Join(nodes, ",") + `]}}`
}

// TestRefreshGitHub_ReEnable_KnownEntitySilentNewEntityDiscovered pins the
// asymmetry the first cycle after a re-enable actually has, because "does it
// diff three weeks of history at me" has two different answers depending on
// which population an entity is in, and both land in this one cycle.
//
//   - An entity TF already tracked had its snapshot cleared by the disable, so
//     however far upstream moved it re-seeds SILENTLY. There is no diff, so
//     there is no burst — not even for the merge that happened while the source
//     was off. Normal diffing resumes next cycle against the fresh baseline.
//   - An entity created DURING the pause was never discovered, so it arrives on
//     the ordinary discovery path and behaves exactly as it would on a repo
//     tracked for the first time: seeded, with its already-standing review
//     request backfilled into the queue.
//
// The second is deliberate rather than an oversight. A review request that has
// been waiting three weeks is precisely the thing a reviewer needs to see when
// the source comes back, and suppressing it would need discovery to know about
// a pause that is over.
func TestRefreshGitHub_ReEnable_KnownEntitySilentNewEntityDiscovered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			body, _ := io.ReadAll(r.Body)
			_, _ = w.Write([]byte(reseedGraphQLFor(string(body))))
		case strings.Contains(r.URL.Path, "/pulls/9"):
			_, _ = w.Write([]byte(reseedMergedPRBasic))
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			_, _ = w.Write([]byte(reseedNewPRList))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	database := newMigratedSQLite(t)
	stores := sqlitestore.New(database)
	org := runmode.LocalDefaultOrgID
	if err := stores.Repos.SetConfigured(ctx, org, []string{"octo/repo"}); err != nil {
		t.Fatalf("SetConfigured: %v", err)
	}

	// The entity TF knew before the pause, with the snapshot a normal cycle
	// left behind.
	known, _, err := stores.Entities.FindOrCreate(ctx, org, "github", "octo/repo#9", "pr", "Paused PR", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if _, err := stores.Entities.UpdateSnapshot(ctx, org, known.ID, pausedPRSnapshot()); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	if _, err := stores.Entities.ClearSnapshotsForSourceSystem(ctx, org, "github"); err != nil {
		t.Fatalf("clear snapshots: %v", err)
	}

	pub := &recordingPublisher{}
	tr := New(database, pub, stores.Tasks, stores.Entities, stores.Repos, stores.EventQueue, org)
	resolver := fakeReviewerResolver{users: map[string]bool{"carol": true}}
	if _, _, err := tr.RefreshGitHub(ctx, ghclient.NewClient(srv.URL, "tok"), "", []string{"octo/repo"}, resolver); err != nil {
		t.Fatalf("RefreshGitHub: %v", err)
	}

	// Exactly one event, and it belongs to the PR TF had never seen.
	evts := pub.nonSystemEvents()
	if len(evts) != 1 {
		t.Fatalf("first cycle after re-enable emitted %d events, want 1: %v", len(evts), eventTypes(evts))
	}
	if evts[0].EventType != domain.EventGitHubPRReviewRequested {
		t.Errorf("event type = %q, want %q", evts[0].EventType, domain.EventGitHubPRReviewRequested)
	}
	fresh, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#10")
	if err != nil || fresh == nil {
		t.Fatalf("the PR opened during the pause was not discovered: ent=%v err=%v", fresh, err)
	}
	if evts[0].EntityID == nil || *evts[0].EntityID != fresh.ID {
		t.Errorf("event is on entity %v, want the newly discovered %s", evts[0].EntityID, fresh.ID)
	}

	// And the entity TF already knew re-seeded without a word, merge included.
	got, err := stores.Entities.GetBySource(ctx, org, "github", "octo/repo#9")
	if err != nil || got == nil {
		t.Fatalf("GetBySource: ent=%v err=%v", got, err)
	}
	if got.State != "closed" || !strings.Contains(got.SnapshotJSON, "PR_node9") {
		t.Errorf("known entity state=%q snapshot=%q; want closed and re-seeded", got.State, got.SnapshotJSON)
	}
}
