package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
