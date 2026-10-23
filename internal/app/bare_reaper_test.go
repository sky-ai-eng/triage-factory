package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/db/dbtest"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestTrackedRepositoryIDs_ReapsWhatNoTrackedRepositoryOwns runs local's
// reaper against its real keep-set: a clone of a tracked repository is kept,
// and a clone of an untracked repository, of an id with no registry row, and,
// after the org moves hosts, of the old host's tracked repository are
// reclaimed. With no GitHub host there is no keep-set, and nothing is.
func TestTrackedRepositoryIDs_ReapsWhatNoTrackedRepositoryOwns(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	ctx := context.Background()
	stores, conn := openConnectionTestStores(t)
	org, team := runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID

	if err := dbtest.TrackRepos(ctx, stores.Repos, stores.TeamGitHubRepos, org, team, dbtest.TestGitHubHost,
		[]domain.TeamGitHubRepo{{Owner: "octo", Repo: "kept"}, {Owner: "octo", Repo: "dropped"}}); err != nil {
		t.Fatalf("track repos: %v", err)
	}
	if err := dbtest.TrackRepos(ctx, stores.Repos, stores.TeamGitHubRepos, org, team, dbtest.TestGitHubHost,
		[]domain.TeamGitHubRepo{{Owner: "octo", Repo: "kept"}}); err != nil {
		t.Fatalf("untrack octo/dropped: %v", err)
	}
	repoID := func(name string) string {
		t.Helper()
		row, err := stores.Repos.GetByRefSystem(ctx, org, domain.RepoRef{Host: dbtest.TestGitHubHost, Owner: "octo", Repo: name})
		if err != nil || row == nil {
			t.Fatalf("resolve octo/%s: row=%v err=%v", name, row, err)
		}
		return row.ID
	}
	kept, dropped := seedFakeBare(t, repoID("kept")), seedFakeBare(t, repoID("dropped"))
	rowless := seedFakeBare(t, "no-registry-row")

	policy := worktree.Policy{Tracked: trackedRepositoryIDs(stores.Repos, stores.Orgs)}
	if evicted, _ := worktree.EnforceBudget(ctx, policy); evicted != 2 {
		t.Errorf("evicted=%d, want the untracked and the rowless clone", evicted)
	}
	assertBare(t, "tracked", kept, true)
	assertBare(t, "untracked", dropped, false)
	assertBare(t, "rowless", rowless, false)

	if err := db.SetLocalGitHubURLForTest(conn, ""); err != nil {
		t.Fatalf("clear github url: %v", err)
	}
	if ids, ok := trackedRepositoryIDs(stores.Repos, stores.Orgs)(ctx); ok {
		t.Errorf("keep-set with no GitHub host = %v, want no answer", ids)
	}
	if evicted, _ := worktree.EnforceBudget(ctx, policy); evicted != 0 {
		t.Errorf("evicted=%d with no GitHub host, want 0", evicted)
	}
	assertBare(t, "tracked, host cleared", kept, true)

	if err := db.SetLocalGitHubURLForTest(conn, dbtest.TestOtherGitHubHost); err != nil {
		t.Fatalf("move github host: %v", err)
	}
	if evicted, _ := worktree.EnforceBudget(ctx, policy); evicted != 1 {
		t.Errorf("evicted=%d after the host move, want the old host's clone", evicted)
	}
	assertBare(t, "old host's tracked", kept, false)
}

// seedFakeBare puts a directory where the bare clone of repositoryID lives,
// last used well past the reaper's grace for an untracked clone. The reaper
// reads only the directory and its worktrees/ admin dir, so no git is needed.
func seedFakeBare(t *testing.T, repositoryID string) string {
	t.Helper()
	dir, err := worktree.RepoDir(repositoryID)
	if err != nil {
		t.Fatalf("repo dir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir bare: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write bare: %v", err)
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatalf("backdate bare: %v", err)
	}
	return dir
}

func assertBare(t *testing.T, name, dir string, want bool) {
	t.Helper()
	_, err := os.Stat(dir)
	switch {
	case want && err != nil:
		t.Errorf("%s clone was reclaimed: %v", name, err)
	case !want && !os.IsNotExist(err):
		t.Errorf("%s clone survived: stat err=%v", name, err)
	}
}
