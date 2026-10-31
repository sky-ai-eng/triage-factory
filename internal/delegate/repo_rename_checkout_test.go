package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop/tooldefs"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// renameInLedger makes every row of repositoryID read back under repoID, as
// the store's join to repositories reads a row once the repository is renamed.
// Paths stay as recorded: a rename moves no directory.
func (l *checkoutLedger) renameInLedger(repositoryID, repoID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.rows {
		if l.rows[i].RepositoryID == repositoryID {
			l.rows[i].RepoID = repoID
		}
	}
}

// TestSnapshotWorkspace_CarriesACheckoutMadeBeforeARename: a checkout made
// while the repository was acme/app sits at <root>/acme/app/<slug>, and once
// the repository is renamed its ledger rows read back as acme/app-renamed. The
// snapshot still carries it, and a cold restore rebuilds it where it was, with
// the work in it.
func TestSnapshotWorkspace_CarriesACheckoutMadeBeforeARename(t *testing.T) {
	f := newSnapshotFixture(t, "task-renamed-snapshot")
	co := f.addCheckout(t, "acme/app", "default")
	dirtyCheckout(t, co)
	f.ledger.renameInLedger(testRepositoryID("acme", "app"), "acme/app-renamed")

	f.snapshot(t, "", domain.ConversationRuntimeNative)
	members := snapshotMembers(t, f.s.Storage(), snapshotKey(runmode.LocalDefaultOrgID, f.key))
	if !members[snapCheckoutsPrefix+"acme/app/default/bundle"] {
		t.Fatalf("the snapshot dropped the checkout made before the rename (members: %v)", members)
	}

	f.loseRoot(t)
	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), f.restorer(), failingFreshBuilder(t)); err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	assertFileContains(t, filepath.Join(co, "committed.txt"), "unpushed commit")
	assertFileContains(t, filepath.Join(co, "README.md"), "uncommitted edit")
}

// TestRecordTaskCheckouts_FindsACheckoutMadeBeforeARename: a conversation
// opening in a warm tree is recorded against every checkout of its task on
// disk, including one made under the repository's name before a rename, at
// the path it sits at. Without the row the git gate gives it no push in there.
func TestRecordTaskCheckouts_FindsACheckoutMadeBeforeARename(t *testing.T) {
	f := newSnapshotFixture(t, "task-renamed-warm")
	co := f.addCheckout(t, "acme/app", "default")
	f.ledger.renameInLedger(testRepositoryID("acme", "app"), "acme/app-renamed")

	if err := f.s.recordTaskCheckouts(context.Background(), runmode.LocalDefaultOrgID, f.convAs("conv-2"), f.root); err != nil {
		t.Fatalf("recordTaskCheckouts: %v", err)
	}
	recorded := f.ledger.recordedRows()
	if len(recorded) != 1 || recorded[0].ConversationID != "conv-2" || recorded[0].Path != co {
		t.Errorf("recorded rows = %+v, want one for conv-2 at %s", recorded, co)
	}
}

// TestTaskCheckoutPath_FindsThePRCheckoutMadeBeforeARename: the PR checkout a
// step is pointed at is the one on disk. After a rename the repository's
// current name names a directory that was never made, and the step would be
// told, and record, a path with no checkout behind it.
func TestTaskCheckoutPath_FindsThePRCheckoutMadeBeforeARename(t *testing.T) {
	f := newSnapshotFixture(t, "task-renamed-pr-path")
	pr := f.addCheckout(t, "acme/app", "pr-7")
	repositoryID := testRepositoryID("acme", "app")
	f.ledger.renameInLedger(repositoryID, "acme/app-renamed")

	got := f.s.taskCheckoutPath(context.Background(), runmode.LocalDefaultOrgID, f.key, f.root, repositoryID, "acme", "app-renamed", "pr-7")
	if got != pr {
		t.Errorf("PR checkout path = %s, want %s, where the checkout is", got, pr)
	}

	// A tree whose PR checkout was made under the current name has it there.
	g := newSnapshotFixture(t, "task-current-pr-path")
	current := g.addCheckout(t, "acme/app", "pr-7")
	if got := g.s.taskCheckoutPath(context.Background(), runmode.LocalDefaultOrgID, g.key, g.root, repositoryID, "acme", "app", "pr-7"); got != current {
		t.Errorf("PR checkout path = %s, want %s", got, current)
	}
}

// TestEnsureWorkspace_ColdRehydrate_ReadsThePRUnderTheRenamedName: a snapshot
// of a PR checkout taken after its repository was renamed restores at the
// path it was made under, and the pull request its push tracking is rebuilt
// from is read under the repository's current name, the name the engagement's
// credentials are scoped to.
func TestEnsureWorkspace_ColdRehydrate_ReadsThePRUnderTheRenamedName(t *testing.T) {
	ctx := context.Background()
	f := newSnapshotFixture(t, "task-renamed-restore")
	runmode.SetLocalSandboxForTest(t, true)
	entityID, rows := f.taskRegistry(t, dbtest.TestGitHubHost, dbtest.TestGitHubHost)
	row := rows[dbtest.TestGitHubHost]
	pr := f.addCheckoutOf(t, worktree.Repo{ID: row.ID, Owner: "acme", Name: "app"}, "pr-7")
	dirtyCheckout(t, pr)

	if _, err := f.s.repos.FillMissingExternalIDsSystem(ctx, runmode.LocalDefaultOrgID, []domain.RepoRef{
		{Host: dbtest.TestGitHubHost, Owner: "acme", Repo: "app", ExternalID: "4242"},
	}); err != nil {
		t.Fatalf("seed the repository's id: %v", err)
	}
	if out, err := f.s.repos.RenameSystem(ctx, runmode.LocalDefaultOrgID, domain.RepoRef{
		Host: dbtest.TestGitHubHost, Owner: "acme", Repo: "app-renamed", ExternalID: "4242",
	}); err != nil || !out.Renamed {
		t.Fatalf("RenameSystem = %+v, %v", out, err)
	}
	f.ledger.renameInLedger(row.ID, "acme/app-renamed")

	f.snapshot(t, "", domain.ConversationRuntimeNative)
	f.loseRoot(t)

	origin := f.upstream(t, "acme/app")
	var (
		mu    sync.Mutex
		paths []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case !strings.HasPrefix(r.URL.Path, "/repos/acme/app-renamed/"):
			http.NotFound(w, r)
			return
		case r.URL.Path != "/repos/acme/app-renamed/pulls/7":
			_, _ = w.Write([]byte("[]"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"head":   map[string]any{"ref": "feature", "repo": map[string]any{"clone_url": origin, "ssh_url": origin}},
			"base":   map[string]any{"ref": "main", "repo": map[string]any{"clone_url": origin}},
		})
	}))
	t.Cleanup(srv.Close)
	f.s.ghClient = ghclient.NewProxyClient(srv.URL, "placeholder")

	restorer := f.s.checkoutRestorerFor(runmode.LocalDefaultOrgID, entityID, nil, nil)
	if _, _, _, err := f.s.ensureWorkspace(ctx, runmode.LocalDefaultOrgID, f.conv(""), restorer, failingFreshBuilder(t)); err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	assertFileContains(t, filepath.Join(pr, "committed.txt"), "unpushed commit")
	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 || paths[0] != "/repos/acme/app-renamed/pulls/7" {
		t.Errorf("GitHub requests = %v, want the pull request read under acme/app-renamed", paths)
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "/repos/acme/app-renamed/") {
			t.Errorf("GitHub request %s addresses the repository by a name it no longer has", p)
		}
	}
	if remotes := strings.Fields(gitOut(t, pr, "remote")); len(remotes) < 2 {
		t.Errorf("remotes = %v, want a push remote beside origin, rebuilt from the pull request", remotes)
	}
}

// renamedRemoteCheckout builds a PR checkout of acme/app whose remotes name
// the repository by GitHub's clone URL, https://github.com/acme/app.git, as a
// checkout made from it does, then renames the repository to acme/app-renamed
// in the registry and the ledger. It returns the checkout's path.
func renamedRemoteCheckout(t *testing.T, f *snapshotFixture) string {
	t.Helper()
	ctx := context.Background()
	runmode.SetLocalSandboxForTest(t, true)
	_, rows := f.taskRegistry(t, dbtest.TestGitHubHost, dbtest.TestGitHubHost)
	row := rows[dbtest.TestGitHubHost]
	pr := f.addCheckoutOf(t, worktree.Repo{ID: row.ID, Owner: "acme", Name: "app"}, "pr-7")
	remotes := strings.Fields(gitOut(t, pr, "remote"))
	if len(remotes) < 2 {
		t.Fatalf("remotes = %v, want origin and the PR's push remote", remotes)
	}
	for _, remote := range remotes {
		gitT(t, pr, "remote", "set-url", remote, "https://github.com/acme/app.git")
	}
	if _, err := f.s.repos.FillMissingExternalIDsSystem(ctx, runmode.LocalDefaultOrgID, []domain.RepoRef{
		{Host: dbtest.TestGitHubHost, Owner: "acme", Repo: "app", ExternalID: "4242"},
	}); err != nil {
		t.Fatalf("seed the repository's id: %v", err)
	}
	if out, err := f.s.repos.RenameSystem(ctx, runmode.LocalDefaultOrgID, domain.RepoRef{
		Host: dbtest.TestGitHubHost, Owner: "acme", Repo: "app-renamed", ExternalID: "4242",
	}); err != nil || !out.Renamed {
		t.Fatalf("RenameSystem = %+v, %v", out, err)
	}
	f.ledger.renameInLedger(row.ID, "acme/app-renamed")
	return pr
}

// assertRemotesName asserts every remote of the checkout at dir has url.
func assertRemotesName(t *testing.T, dir, url string) {
	t.Helper()
	for _, remote := range strings.Fields(gitOut(t, dir, "remote")) {
		if got := strings.TrimSpace(gitOut(t, dir, "remote", "get-url", remote)); got != url {
			t.Errorf("remote %s = %s, want %s", remote, got, url)
		}
	}
}

// TestEnsureWorkspace_WarmTreeRepointsRemotesOfARenamedRepository: a warm tree
// whose checkout was made before its repository was renamed comes back with
// its origin and its push remote naming the repository's new name, which is
// what the gh verbs, the repo gates and the credential lookup read.
func TestEnsureWorkspace_WarmTreeRepointsRemotesOfARenamedRepository(t *testing.T) {
	f := newSnapshotFixture(t, "task-renamed-warm-remotes")
	pr := renamedRemoteCheckout(t, f)

	_, prov, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), checkoutRestorer{}, failingFreshBuilder(t))
	if err != nil || prov != domain.WorkspaceProvenanceWarm {
		t.Fatalf("ensureWorkspace = (%q, %v), want warm", prov, err)
	}
	assertRemotesName(t, pr, "https://github.com/acme/app-renamed.git")
}

// TestRepointRenamedRemotesInJail_RunsTheRewriteThroughTheToolHost: a tree the
// sandbox identity owns is rewritten by the engagement's tool host, in one
// script that does what the host-side rewrite does, and a tree with nothing
// to move costs no call at all.
func TestRepointRenamedRemotesInJail_RunsTheRewriteThroughTheToolHost(t *testing.T) {
	f := newSnapshotFixture(t, "task-renamed-jail-remotes")
	pr := renamedRemoteCheckout(t, f)

	host := &recordingToolHost{}
	f.s.repointRenamedRemotesInJail(context.Background(), runmode.LocalDefaultOrgID, f.key, f.root, host)
	cmds := host.commands()
	if len(cmds) != 1 {
		t.Fatalf("tool host calls = %d, want 1", len(cmds))
	}
	// The recording host validates nothing, and the real jail refuses a call
	// missing any required argument.
	for _, key := range tooldefs.Bash.Params.Required {
		if _, ok := host.args[0][key]; !ok {
			t.Errorf("the jail call omits %q, which the bash schema requires", key)
		}
	}
	assertRemotesName(t, pr, "https://github.com/acme/app.git")
	if out, err := exec.Command("bash", "-c", cmds[0]).CombinedOutput(); err != nil {
		t.Fatalf("run the jail script: %v: %s\n%s", err, out, cmds[0])
	}
	assertRemotesName(t, pr, "https://github.com/acme/app-renamed.git")

	f.s.repointRenamedRemotesInJail(context.Background(), runmode.LocalDefaultOrgID, f.key, f.root, host)
	if n := len(host.commands()); n != 1 {
		t.Errorf("tool host calls after the rewrite = %d, want none beyond the first", n-1)
	}
}
