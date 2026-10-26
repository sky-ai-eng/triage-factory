package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEnsureRunsBase_ModeByRunMode pins where the runs base is and who it
// admits. In multi it is under the state root, not the temp dir, and a jail
// identity on the host has to traverse it to its own tree, so the mode is
// pinned to runsBaseMode whatever the directory had. Locally it is in the temp
// dir and nothing but this uid reaches a run tree: the base is created
// owner-only, and one that already exists is left with its own mode.
func TestEnsureRunsBase_ModeByRunMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     runmode.Mode
		existing os.FileMode // 0: the base does not exist yet
		want     os.FileMode
	}{
		{"local, fresh", runmode.ModeLocal, 0, 0o700},
		{"local, existing", runmode.ModeLocal, 0o750, 0o750},
		{"multi, fresh", runmode.ModeMulti, 0, runsBaseMode},
		{"multi, existing", runmode.ModeMulti, 0o700, runsBaseMode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			stateRoot := t.TempDir()
			paths.SetForTest(t, stateRoot)
			base := filepath.Join(tmp, "triagefactory-runs")
			if tc.mode == runmode.ModeMulti {
				base = filepath.Join(stateRoot, "runs")
			}
			if tc.existing != 0 {
				if err := os.Mkdir(base, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(base, tc.existing); err != nil {
					t.Fatal(err)
				}
			}

			got, err := EnsureRunsBase()
			if err != nil {
				t.Fatalf("EnsureRunsBase: %v", err)
			}
			if got != base {
				t.Errorf("EnsureRunsBase = %q, want %q", got, base)
			}
			fi, err := os.Stat(base)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode().Perm() != tc.want {
				t.Errorf("runs base mode = %o, want %o", fi.Mode().Perm(), tc.want)
			}
		})
	}
}

// TestIsRunTreeFor_ByRunMode pins which runs base a recorded path has to be in
// to count as a key's run tree. In multi that is exactly <state root>/runs: a
// directory merely named "runs" elsewhere is not one, and neither is a tree in
// the temp dir, where an executor builds nothing. Locally it is any directory
// named triagefactory-runs, whatever $TMPDIR the tree was made under.
func TestIsRunTreeFor_ByRunMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	stateRoot := t.TempDir()
	paths.SetForTest(t, stateRoot)

	stateTree := filepath.Join(stateRoot, "runs", "task-1")
	tmpTree := filepath.Join(tmp, "triagefactory-runs", "task-1")
	otherTmpTree := filepath.Join("/var/elsewhere", "triagefactory-runs", "task-1")
	otherRuns := filepath.Join("/srv", "runs", "task-1")

	for _, tc := range []struct {
		mode runmode.Mode
		path string
		want bool
	}{
		{runmode.ModeMulti, stateTree, true},
		{runmode.ModeMulti, tmpTree, false},
		{runmode.ModeMulti, otherRuns, false},
		{runmode.ModeLocal, tmpTree, true},
		{runmode.ModeLocal, otherTmpTree, true},
		{runmode.ModeLocal, stateTree, false},
	} {
		t.Run(string(tc.mode)+" "+tc.path, func(t *testing.T) {
			runmode.SetForTest(t, tc.mode)
			if got := IsRunTreeFor(tc.path, "task-1"); got != tc.want {
				t.Errorf("IsRunTreeFor(%q) = %v, want %v", tc.path, got, tc.want)
			}
			if got := isTFRunWorktreePath(filepath.Join(tc.path, "owner", "repo", "slug")); got != tc.want {
				t.Errorf("isTFRunWorktreePath(a checkout under %q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestCleanupWithOptions_MultiSweepsTheStateRootRunsBase is the startup sweep
// on an executor: it empties <state root>/runs — trees, and the snapshot and
// capture staging a stopped process left beside them, which on the state root
// would otherwise outlive the container — and leaves the temp dir's runs base
// alone, since that one belongs to whatever local install shares the host.
func TestCleanupWithOptions_MultiSweepsTheStateRootRunsBase(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	t.Setenv("HOME", t.TempDir())
	stateRoot := t.TempDir()
	paths.SetForTest(t, stateRoot)

	base, err := EnsureRunsBase()
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(base, "task-1")
	capture := filepath.Join(base, ".capture-1")
	for _, d := range []string{tree, capture} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	archive := filepath.Join(base, ".snapshot-1.tar.zst")
	if err := os.WriteFile(archive, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	localTree := filepath.Join(tmp, "triagefactory-runs", "task-local")
	if err := os.MkdirAll(localTree, 0o700); err != nil {
		t.Fatal(err)
	}

	CleanupWithOptions(CleanupOptions{SkipClaudeProjectCleanup: true})

	for _, p := range []string{tree, capture, archive} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep (err=%v)", p, err)
		}
	}
	if _, err := os.Stat(base); err != nil {
		t.Errorf("the runs base itself went with its contents: %v", err)
	}
	if _, err := os.Stat(localTree); err != nil {
		t.Errorf("the temp dir's runs base was swept on an executor: %v", err)
	}
}
