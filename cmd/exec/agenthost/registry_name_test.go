package agenthost

import (
	"context"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// renamedRepoID is GitHub's id for the repository the tests below rename from
// octo/old-name to octo/new-name.
const renamedRepoID = "4242"

// registerRepo gives the registry a row for owner/repo on the test org's
// GitHub host carrying GitHub's id for it, the way a tracked repository's row
// carries one.
func registerRepo(t *testing.T, stores db.Stores, owner, repo, externalID string) {
	t.Helper()
	ctx := context.Background()
	ref := domain.RepoRef{Source: domain.RepoSourceGitHub, Host: testScope("github"), Owner: owner, Repo: repo, ExternalID: externalID}
	if _, err := stores.Repos.FindOrCreateByRefSystem(ctx, runmode.LocalDefaultOrgID, ref); err != nil {
		t.Fatalf("register %s/%s: %v", owner, repo, err)
	}
	if _, err := stores.Repos.FillMissingExternalIDsSystem(ctx, runmode.LocalDefaultOrgID, []domain.RepoRef{ref}); err != nil {
		t.Fatalf("record the id of %s/%s: %v", owner, repo, err)
	}
}

// renameRepo applies the rename of octo/old-name to octo/new-name the way the
// poller does when GitHub reports the repository's id under its new name.
func renameRepo(t *testing.T, stores db.Stores) {
	t.Helper()
	out, err := stores.Repos.RenameSystem(context.Background(), runmode.LocalDefaultOrgID, domain.RepoRef{
		Source: domain.RepoSourceGitHub, Host: testScope("github"), Owner: "octo", Repo: "new-name", ExternalID: renamedRepoID,
	})
	if err != nil {
		t.Fatalf("rename octo/old-name -> octo/new-name: %v", err)
	}
	if !out.Renamed {
		t.Fatalf("rename octo/old-name -> octo/new-name was a no-op: %+v", out)
	}
}

// startRenamedRepoGitHub stands a GitHub that serves the renamed repository
// under both names, as GitHub follows an old name to its repository, and
// opens pull request #42 on it.
func startRenamedRepoGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/api/v3")
		switch {
		case r.Method == http.MethodGet && (path == "/repos/octo/old-name" || path == "/repos/octo/new-name"):
			_, _ = io.WriteString(w, `{"id":`+renamedRepoID+`,"full_name":"octo/new-name"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/pulls"):
			_, _ = io.WriteString(w, `{"number":42,"html_url":"https://github.com/octo/new-name/pull/42","node_id":"PR_renamed"}`)
		default:
			t.Errorf("unexpected GitHub call: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestLocalClient_ReviewUnderOldName_FoundByCoherenceReads pins that a review draft
// started through a checkout that still spells the old name is recorded under
// the name the rename moved the repository to, so the PR-coherence reads,
// which ask for the PR under its current name, find it: the conversation is a
// coherence target, and its review counts as anchored at the head it pinned.
func TestLocalClient_ReviewUnderOldName_FoundByCoherenceReads(t *testing.T) {
	gh := startRenamedRepoGitHub(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	registerRepo(t, stores, "octo", "old-name", renamedRepoID)
	renameRepo(t, stores)
	ctx := context.Background()

	handle, err := client.GithubCreatePendingReview(ctx, "octo", "old-name", 5, "head5", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
	}
	wantTarget := domain.ReviewTarget("octo/new-name", 5)
	if a := arts[0]; a.ID != handle || a.Target != wantTarget || a.DedupKey != domain.ReviewDedupKey("octo/new-name", 5, info.ConversationID) {
		t.Fatalf("review draft = id %q target %q key %q, want %q under octo/new-name", a.ID, a.Target, a.DedupKey, handle)
	}

	pr, _, err := stores.Entities.FindOrCreateSystem(ctx, info.OrgID, "github", testScope("github"), "octo/new-name#5", "", "pr", "Renamed PR", "")
	if err != nil {
		t.Fatalf("seed the PR entity: %v", err)
	}
	targets, err := stores.Conversations.ListPRCoherenceTargetsSystem(ctx, info.OrgID, db.PRCoherenceTargetQuery{
		EntityID:     pr.ID,
		EventID:      "event-new-commits",
		BaseRepo:     "octo/new-name",
		HeadRepo:     "octo/new-name",
		ReviewTarget: wantTarget,
		PRRef:        "pr-5",
	})
	if err != nil {
		t.Fatalf("ListPRCoherenceTargetsSystem: %v", err)
	}
	if len(targets) != 1 || targets[0].ConversationID != info.ConversationID {
		t.Errorf("coherence targets = %+v, want the reviewing conversation %s", targets, info.ConversationID)
	}

	reviews, err := stores.Artifacts.ListPendingReviewsByTargetSystem(ctx, info.OrgID, testScope("github"), wantTarget)
	if err != nil {
		t.Fatalf("ListPendingReviewsByTargetSystem: %v", err)
	}
	if len(reviews) != 1 || !domain.ReviewAnchoredAtHead(reviews[0], "head5") {
		t.Errorf("pending reviews on %s = %+v, want the draft anchored at head5", wantTarget, reviews)
	}
}

// TestLocalClient_OldNameAfterRename_JoinsTheRow pins that recording an object
// under the old name after the rename lands on the row already recorded for
// it under the new name, for a pull request and a review draft alike.
func TestLocalClient_OldNameAfterRename_JoinsTheRow(t *testing.T) {
	for _, eventTriggered := range []bool{true, false} {
		name := "manual"
		if eventTriggered {
			name = "event-triggered"
		}
		t.Run(name, func(t *testing.T) {
			gh := startRenamedRepoGitHub(t)
			stores, info, client := newGithubRecordingClient(t, gh.URL, eventTriggered)
			registerRepo(t, stores, "octo", "old-name", renamedRepoID)
			renameRepo(t, stores)
			ctx := context.Background()

			for _, repo := range []string{"new-name", "old-name"} {
				if _, _, _, err := client.GithubCreatePR(ctx, "octo", repo, "feature/x", "main", "t", "b", true); err != nil {
					t.Fatalf("GithubCreatePR via %s: %v", repo, err)
				}
			}
			first, err := client.GithubCreatePendingReview(ctx, "octo", "new-name", 5, "head5", nil)
			if err != nil {
				t.Fatalf("GithubCreatePendingReview via new-name: %v", err)
			}
			second, err := client.GithubCreatePendingReview(ctx, "octo", "old-name", 5, "head5", nil)
			if err != nil {
				t.Fatalf("GithubCreatePendingReview via old-name: %v", err)
			}
			if first != second {
				t.Errorf("review handles = %q and %q, want one draft", first, second)
			}

			byKey := map[string]int{}
			for _, a := range listConversationArtifacts(t, stores, info.ConversationID) {
				byKey[a.DedupKey]++
			}
			want := map[string]int{
				"github:pull_request:octo/new-name#42":                         1,
				domain.ReviewDedupKey("octo/new-name", 5, info.ConversationID): 1,
			}
			if !maps.Equal(byKey, want) {
				t.Errorf("artifact keys = %v, want %v", byKey, want)
			}
		})
	}
}

// TestLocalClient_NewNameBeforeRename_DoesNotBlockIt pins the other direction:
// before TF applies the rename, an object recorded under both the old name and
// the new one GitHub already answers to is one row under the registry's old
// name, so the rename's rewrite has no second row to collide with and moves it.
// The new name already has a bare registry row, as an entity created through
// it mints one, and that row carries no id to answer with.
func TestLocalClient_NewNameBeforeRename_DoesNotBlockIt(t *testing.T) {
	gh := startRenamedRepoGitHub(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	registerRepo(t, stores, "octo", "old-name", renamedRepoID)
	ctx := context.Background()
	if _, err := stores.Repos.FindOrCreateByRefSystem(ctx, info.OrgID, domain.RepoRef{
		Source: domain.RepoSourceGitHub, Host: testScope("github"), Owner: "octo", Repo: "new-name",
	}); err != nil {
		t.Fatalf("mint the bare octo/new-name row: %v", err)
	}

	for _, repo := range []string{"old-name", "new-name"} {
		if _, err := client.GithubCreatePendingReview(ctx, "octo", repo, 5, "head5", nil); err != nil {
			t.Fatalf("GithubCreatePendingReview via %s: %v", repo, err)
		}
		if _, _, _, err := client.GithubCreatePR(ctx, "octo", repo, "feature/x", "main", "t", "b", true); err != nil {
			t.Fatalf("GithubCreatePR via %s: %v", repo, err)
		}
	}
	for _, a := range listConversationArtifacts(t, stores, info.ConversationID) {
		if !strings.HasPrefix(a.Target, "octo/old-name#") {
			t.Errorf("artifact %s target = %q, want it under the registry's octo/old-name", a.DedupKey, a.Target)
		}
	}

	renameRepo(t, stores)
	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 2 {
		t.Fatalf("want the review and the pull request, got %d: %+v", len(arts), arts)
	}
	for _, a := range arts {
		if !strings.HasPrefix(a.Target, "octo/new-name#") {
			t.Errorf("artifact %s target = %q after the rename, want octo/new-name", a.DedupKey, a.Target)
		}
	}
}

// TestLocalClient_BranchPushedUnderOldName_RecordedUnderCurrent pins that a branch pushed through
// a checkout that still spells the old name is recorded, and linked, under the
// repository's current name, the name its pull request is recorded under.
func TestLocalClient_BranchPushedUnderOldName_RecordedUnderCurrent(t *testing.T) {
	gh := startRenamedRepoGitHub(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	registerRepo(t, stores, "octo", "old-name", renamedRepoID)
	renameRepo(t, stores)

	branch, ok := domain.NewBranchArtifact("octo/old-name", "refs/heads/feature/x", "abc123", true)
	if !ok {
		t.Fatal("NewBranchArtifact refused the fixture branch")
	}
	if _, err := client.UpsertArtifact(context.Background(), branch); err != nil {
		t.Fatalf("UpsertArtifact: %v", err)
	}
	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d: %+v", len(arts), arts)
	}
	a := arts[0]
	wantURL, _ := domain.BranchArtifactWebURL(gh.URL, "octo/new-name", "refs/heads/feature/x")
	if a.Target != "octo/new-name" || a.DedupKey != "git:branch:octo/new-name:refs/heads/feature/x" || a.URL != wantURL {
		t.Errorf("branch artifact = target %q key %q url %q, want it under octo/new-name linked at %q", a.Target, a.DedupKey, a.URL, wantURL)
	}
}

// TestLocalClient_RegistryHeldName_AsksNoGitHub pins that a name the registry holds
// under an id is answered from the registry alone, in the registry's spelling:
// GitHub names are case-insensitive, and the readers compare the stored target
// exactly.
func TestLocalClient_RegistryHeldName_AsksNoGitHub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call for a name the registry holds: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)
	registerRepo(t, stores, "octo", "repo", "1")

	if _, err := client.GithubCreatePendingReview(context.Background(), "Octo", "Repo", 7, "head7", nil); err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	arts := listConversationArtifacts(t, stores, info.ConversationID)
	if len(arts) != 1 || arts[0].Target != "octo/repo#7" {
		t.Errorf("review draft = %+v, want it under the registry's octo/repo", arts)
	}
}

// TestRelayRuntime_RepoSlugByExternalID pins the sidecar's half of the id
// lookup: the id relays to the orchestrator, which answers from its registry on
// the org's GitHub host, and an id no row carries answers no name.
func TestRelayRuntime_RepoSlugByExternalID(t *testing.T) {
	stores, info := newCaptureStores(t, true)
	registerRepo(t, stores, "octo", "old-name", renamedRepoID)
	renameRepo(t, stores)
	rt := newRelayRuntime(directDispatchConn{srv: NewRelayServer(stores, info, nil)}, info, nil)

	for id, want := range map[string]string{renamedRepoID: "octo/new-name", "9999": ""} {
		got, err := rt.RepoSlugByExternalID(context.Background(), id)
		if err != nil {
			t.Fatalf("RepoSlugByExternalID(%s) relay: %v", id, err)
		}
		if got != want {
			t.Errorf("RepoSlugByExternalID(%s) = %q, want %q", id, got, want)
		}
	}
}
