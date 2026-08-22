package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// TestCreateInRoot_MintsScaffoldDirs pins the owner/ and owner/repo/ levels the
// checkout and PR creates mint under a run root to the run-tree scaffold mode,
// through the real create rather than the helper alone.
//
// What this defends: the launch-time ownership hand-off chowns the run tree
// recursively and preserves the modes it finds, so a level that reaches it
// group-unwritable stays that way for the life of the tree — and every later
// `workspace add` under that owner fails EACCES in the orchestrator, whatever
// the repo or the credential. The umask here is what an ordinary mkdir would
// lose the bit to.
func TestCreateInRoot_MintsScaffoldDirs(t *testing.T) {
	old := unix.Umask(0o027)
	defer unix.Umask(old)

	withTestHome(t)
	upstream := makeTestUpstream(t)
	// The PR create fetches refs/pull/<n>/head; point one at the same tip so
	// both entry points reach their scaffold mkdir and then succeed.
	coRun(t, upstream, "update-ref", "refs/pull/7/head", "main")

	for _, tc := range []struct {
		name   string
		create func(runRoot string) (string, error)
	}{
		{"checkout", func(runRoot string) (string, error) {
			return CreateForCheckoutInRoot(context.Background(),
				"sky-ai-eng", "orrery", upstream, "", "root-key", runRoot)
		}},
		{"pr", func(runRoot string) (string, error) {
			return CreateForPRInRoot(context.Background(),
				"sky-ai-eng", "orrery", upstream, upstream, "main", 7, "root-key", runRoot)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runRoot := t.TempDir()
			if _, err := tc.create(runRoot); err != nil {
				t.Fatalf("create: %v", err)
			}
			for _, rel := range []string{"sky-ai-eng", filepath.Join("sky-ai-eng", "orrery")} {
				st, err := os.Stat(filepath.Join(runRoot, rel))
				if err != nil {
					t.Fatalf("stat %s: %v", rel, err)
				}
				// 0o020 is the group-write bit the hand-off would otherwise
				// freeze off; asserted by name rather than as a whole mode so
				// this reads as the property it defends.
				if st.Mode().Perm()&0o020 == 0 {
					t.Errorf("%s mode = %#o; want group-writable so the orchestrator can still "+
						"materialize checkouts after the ownership hand-off", rel, st.Mode().Perm())
				}
			}
		})
	}
}

// TestRunTreeScaffoldModeIsSandboxOwned keeps the mode's definition in one
// place: worktree mints scaffold directories but does not decide what the
// scaffold is, and a create that reached for a literal here would drift from
// the hand-off that re-asserts it.
func TestRunTreeScaffoldModeIsSandboxOwned(t *testing.T) {
	root := t.TempDir()
	if err := sandbox.MkdirRunTreeScaffold(root, filepath.Join("owner", "repo")); err != nil {
		t.Fatalf("MkdirRunTreeScaffold: %v", err)
	}
	st, err := os.Stat(filepath.Join(root, "owner"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Mode().Perm()&0o020 == 0 {
		t.Errorf("mode = %#o; want group-writable", st.Mode().Perm())
	}
}
