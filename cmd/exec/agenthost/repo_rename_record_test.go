package agenthost

import (
	"context"
	"errors"
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
// carries one, and returns the row's id.
func registerRepo(t *testing.T, stores db.Stores, owner, repo, externalID string) string {
	t.Helper()
	ctx := context.Background()
	ref := domain.RepoRef{Source: domain.RepoSourceGitHub, Host: testScope("github"), Owner: owner, Repo: repo, ExternalID: externalID}
	row, err := stores.Repos.FindOrCreateByRefSystem(ctx, runmode.LocalDefaultOrgID, ref)
	if err != nil {
		t.Fatalf("register %s/%s: %v", owner, repo, err)
	}
	if _, err := stores.Repos.FillMissingExternalIDsSystem(ctx, runmode.LocalDefaultOrgID, []domain.RepoRef{ref}); err != nil {
		t.Fatalf("record the id of %s/%s: %v", owner, repo, err)
	}
	return row.ID
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

// renamedRepoJSON is the repository GitHub answers for either name of the
// renamed repository: its id, under the name it has now.
const renamedRepoJSON = `{"id":` + renamedRepoID + `,"full_name":"octo/new-name"}`

// startRenamedRepoGitHub stands a GitHub that serves the renamed repository
// under both names, as GitHub follows an old name to its repository, answers
// its pull requests' reads with it as their base, and opens pull request #42
// on it.
func startRenamedRepoGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		path := strings.TrimPrefix(r.URL.Path, "/api/v3")
		_, number, isPull := strings.Cut(path, "/pulls/")
		switch {
		case r.Method == http.MethodGet && (path == "/repos/octo/old-name" || path == "/repos/octo/new-name"):
			_, _ = io.WriteString(w, renamedRepoJSON)
		case r.Method == http.MethodGet && isPull && fakePRNodeID(path) != "":
			_, _ = io.WriteString(w, `{"number":`+number+`,"node_id":"`+fakePRNodeID(path)+`","base":{"ref":"main","repo":`+renamedRepoJSON+`},"head":{"sha":"pr-head"}}`)
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
// the name the rename moved the repository to, and keyed on the pull
// request's node id, so the PR-coherence reads, which ask for the PR by its
// node id, find it: the conversation is a coherence target, and its review
// counts as anchored at the head it pinned.
func TestLocalClient_ReviewUnderOldName_FoundByCoherenceReads(t *testing.T) {
	gh := startRenamedRepoGitHub(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	repoID := registerRepo(t, stores, "octo", "old-name", renamedRepoID)
	renameRepo(t, stores)
	ctx := context.Background()

	handle, err := client.GithubCreatePendingReview(ctx, "octo", "old-name", 5, "head5", nil)
	if err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	arts := listConversationDeliverables(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 deliverable, got %d: %+v", len(arts), arts)
	}
	wantTarget := domain.ReviewTarget("octo/new-name", 5)
	if a := arts[0]; a.ID != handle || a.Target != wantTarget || a.DedupKey != domain.ReviewDedupKey(testPRNodeID(5), info.ConversationID) {
		t.Fatalf("review draft = id %q target %q key %q, want %q under octo/new-name", a.ID, a.Target, a.DedupKey, handle)
	}

	pr, _, err := stores.Entities.FindOrCreateSystem(ctx, info.OrgID, "github", testScope("github"), "octo/new-name#5", testPRNodeID(5), "pr", "Renamed PR", "")
	if err != nil {
		t.Fatalf("seed the PR entity: %v", err)
	}
	targets, err := stores.Conversations.ListPRCoherenceTargetsSystem(ctx, info.OrgID, db.PRCoherenceTargetQuery{
		EntityID:         pr.ID,
		EventID:          "event-new-commits",
		BaseRepositoryID: repoID,
		HeadRepositoryID: repoID,
		PRRef:            "pr-5",
	})
	if err != nil {
		t.Fatalf("ListPRCoherenceTargetsSystem: %v", err)
	}
	if len(targets) != 1 || targets[0].ConversationID != info.ConversationID {
		t.Errorf("coherence targets = %+v, want the reviewing conversation %s", targets, info.ConversationID)
	}

	reviews, err := stores.Deliverables.ListPendingReviewsByEntitySystem(ctx, info.OrgID, pr.ID)
	if err != nil {
		t.Fatalf("ListPendingReviewsByEntitySystem: %v", err)
	}
	if len(reviews) != 1 || !domain.ReviewAnchoredAtHead(reviews[0], "head5") {
		t.Errorf("pending reviews of %s = %+v, want the draft anchored at head5", wantTarget, reviews)
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
			for _, a := range listConversationDeliverables(t, stores, info.ConversationID) {
				byKey[a.DedupKey]++
			}
			want := map[string]int{
				domain.PullRequestDedupKey("PR_renamed"):                    1,
				domain.ReviewDedupKey(testPRNodeID(5), info.ConversationID): 1,
			}
			if !maps.Equal(byKey, want) {
				t.Errorf("deliverable keys = %v, want %v", byKey, want)
			}
		})
	}
}

// TestLocalClient_NewNameBeforeRename_DoesNotBlockIt pins the other direction:
// before TF applies the rename, an object recorded under both the old name and
// the new one GitHub already answers to is one row, under the registry's old
// name, and the rename moves its target. The new name already has a bare
// registry row, which carries no id, and GitHub's id for the repository finds
// the row that does: no second repository.
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
	// The reviewed pull request's entity carries its node id, as start-review's
	// read of the pull request records it; the rename finds the draft by it.
	if _, _, err := stores.Entities.FindOrCreateSystem(ctx, info.OrgID, "github", testScope("github"), "octo/old-name#5", testPRNodeID(5), "pr", "Reviewed PR", ""); err != nil {
		t.Fatalf("seed the reviewed PR's entity: %v", err)
	}

	for _, repo := range []string{"old-name", "new-name"} {
		if _, err := client.GithubCreatePendingReview(ctx, "octo", repo, 5, "head5", nil); err != nil {
			t.Fatalf("GithubCreatePendingReview via %s: %v", repo, err)
		}
		if _, _, _, err := client.GithubCreatePR(ctx, "octo", repo, "feature/x", "main", "t", "b", true); err != nil {
			t.Fatalf("GithubCreatePR via %s: %v", repo, err)
		}
	}
	for _, a := range listConversationDeliverables(t, stores, info.ConversationID) {
		if !strings.HasPrefix(a.Target, "octo/old-name#") {
			t.Errorf("deliverable %s target = %q, want it under the registry's octo/old-name", a.DedupKey, a.Target)
		}
	}

	renameRepo(t, stores)
	arts := listConversationDeliverables(t, stores, info.ConversationID)
	if len(arts) != 2 {
		t.Fatalf("want the review and the pull request, got %d: %+v", len(arts), arts)
	}
	for _, a := range arts {
		if !strings.HasPrefix(a.Target, "octo/new-name#") {
			t.Errorf("deliverable %s target = %q after the rename, want octo/new-name", a.DedupKey, a.Target)
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

	branch, ok := domain.NewBranchDeliverable("octo/old-name", "refs/heads/feature/x", "abc123", true)
	if !ok {
		t.Fatal("NewBranchDeliverable refused the fixture branch")
	}
	if _, err := client.UpsertDeliverable(context.Background(), branch); err != nil {
		t.Fatalf("UpsertDeliverable: %v", err)
	}
	arts := listConversationDeliverables(t, stores, info.ConversationID)
	if len(arts) != 1 {
		t.Fatalf("want 1 deliverable, got %d: %+v", len(arts), arts)
	}
	a := arts[0]
	row, err := stores.Repos.GetByRefSystem(context.Background(), info.OrgID, domain.RepoRefFromSlug(testScope("github"), "octo/new-name"))
	if err != nil || row == nil {
		t.Fatalf("resolve octo/new-name: %v, %v", row, err)
	}
	wantURL, _ := domain.BranchDeliverableWebURL(gh.URL, "octo/new-name", "refs/heads/feature/x")
	if a.Target != "octo/new-name" || a.DedupKey != domain.BranchDedupKey(row.ID, "refs/heads/feature/x") || a.URL != wantURL {
		t.Errorf("branch deliverable = target %q key %q url %q, want it under octo/new-name, keyed on row %s, linked at %q", a.Target, a.DedupKey, a.URL, row.ID, wantURL)
	}
}

// TestLocalClient_BranchWithAFailedIdentityRead_RecordsOnlyThePush pins that a
// branch whose repository's identity could not be read is refused as a
// deliverable, since its key is the row GitHub's id names, and that its push
// still leaves its ledger row, unlinked.
func TestLocalClient_BranchWithAFailedIdentityRead_RecordsOnlyThePush(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)

	branch, ok := domain.NewBranchDeliverable("octo/unregistered", "refs/heads/feature/x", "abc123", true)
	if !ok {
		t.Fatal("NewBranchDeliverable refused the fixture branch")
	}
	if _, err := client.UpsertDeliverable(context.Background(), branch); !errors.Is(err, errNoRegistryRow) {
		t.Fatalf("UpsertDeliverable err = %v, want errNoRegistryRow", err)
	}
	if arts := listConversationDeliverables(t, stores, info.ConversationID); len(arts) != 0 {
		t.Errorf("deliverables = %+v, want none for a branch with no registry row", arts)
	}
	acts, _, err := stores.ExternalActions.ListByOrgSystem(context.Background(), info.OrgID, domain.ExternalActionListOpts{})
	if err != nil {
		t.Fatalf("list external actions: %v", err)
	}
	if len(acts) != 1 || acts[0].Action != domain.ActionBranchPushed || acts[0].ExternalID != "refs/heads/feature/x" || acts[0].RepositoryID != "" {
		t.Errorf("external actions = %+v, want the one branch_pushed row, unlinked", acts)
	}
}

// TestLocalClient_RegistryHeldName_ResolvedFromTheRegistry pins that a
// repository GitHub's id names is recorded in the registry's spelling: GitHub
// names are case-insensitive, and the readers compare the stored target
// exactly. The one GitHub read is the pull request's, which answers its node
// id and its repository's id together.
func TestLocalClient_RegistryHeldName_ResolvedFromTheRegistry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerPRRead(w, r) {
			return
		}
		t.Errorf("unexpected GitHub call for a name the registry holds: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	stores, info, client := newGithubRecordingClient(t, srv.URL, true)
	registerRepo(t, stores, "octo", "repo", testRepoID("octo/repo"))

	if _, err := client.GithubCreatePendingReview(context.Background(), "Octo", "Repo", 7, "head7", nil); err != nil {
		t.Fatalf("GithubCreatePendingReview: %v", err)
	}
	arts := listConversationDeliverables(t, stores, info.ConversationID)
	if len(arts) != 1 || arts[0].Target != "octo/repo#7" {
		t.Errorf("review draft = %+v, want it under the registry's octo/repo", arts)
	}
}

// TestLocalClient_BranchPushedUnderAnUnappliedNewName_LinksItsRowByID pins a
// push through a checkout that spells the repository's new name before TF has
// applied the rename: GitHub's id for the repository names the row TF holds
// under the old name, so the branch is keyed on that row and recorded under
// its name, and no second registry row is created for the new one.
func TestLocalClient_BranchPushedUnderAnUnappliedNewName_LinksItsRowByID(t *testing.T) {
	gh := startRenamedRepoGitHub(t)
	stores, info, client := newGithubRecordingClient(t, gh.URL, true)
	repoID := registerRepo(t, stores, "octo", "old-name", renamedRepoID)
	ctx := context.Background()

	branch, ok := domain.NewBranchDeliverable("octo/new-name", "refs/heads/feature/x", "abc123", true)
	if !ok {
		t.Fatal("NewBranchDeliverable refused the fixture branch")
	}
	if _, err := client.UpsertDeliverable(ctx, branch); err != nil {
		t.Fatalf("UpsertDeliverable: %v", err)
	}
	arts := listConversationDeliverables(t, stores, info.ConversationID)
	if len(arts) != 1 || arts[0].Target != "octo/old-name" || arts[0].DedupKey != domain.BranchDedupKey(repoID, "refs/heads/feature/x") {
		t.Fatalf("branch deliverables = %+v, want one keyed on row %s under octo/old-name", arts, repoID)
	}
	acts := listExternalActions(t, stores)
	if len(acts) != 1 || acts[0].RepositoryID != repoID {
		t.Errorf("push rows = %+v, want one referencing row %s", acts, repoID)
	}
	if row, err := stores.Repos.GetByRefSystem(ctx, info.OrgID, domain.RepoRefFromSlug(testScope("github"), "octo/new-name")); err != nil || row != nil {
		t.Errorf("octo/new-name = %+v, %v; want no second registry row", row, err)
	}
}
