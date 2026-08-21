package tracker

import (
	"context"
	"encoding/json"
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
const reseedMergedPRRefresh = `{"data":{"nodes":[
	{"id":"PR_node9","number":9,"title":"Paused PR","author":{"login":"bob"},
	 "state":"MERGED","merged":true,"url":"https://github.com/octo/repo/pull/9",
	 "repository":{"nameWithOwner":"octo/repo"},
	 "createdAt":"2026-06-01T00:00:00Z","updatedAt":"2026-06-20T00:00:00Z","mergedAt":"2026-06-20T00:00:00Z",
	 "reviews":{"nodes":[{"state":"APPROVED","author":{"login":"carol"},"submittedAt":"2026-06-19T00:00:00Z"}]}}
]}}`

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
