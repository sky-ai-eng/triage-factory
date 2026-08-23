package sandbox

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// TestMkdirRunTreeScaffold_ModeSurvivesUmask is the regression that matters:
// the scaffold's group-write bit is what lets the orchestrator materialize a
// checkout into a run root already handed to the sandbox identity, and the
// ownership hand-off preserves whatever modes it finds. A level minted at the
// 0755 a umask-masked mkdir produces is therefore frozen unwritable the first
// time a run relaunches, and every later `workspace add` under that owner
// fails EACCES — which is why the mode is asserted rather than merely passed
// to mkdirat.
func TestMkdirRunTreeScaffold_ModeSurvivesUmask(t *testing.T) {
	// 0o027 clears group-write from any mode mkdirat is asked for. Process-wide
	// and therefore not parallel-safe; restored immediately.
	old := unix.Umask(0o027)
	defer unix.Umask(old)

	root := t.TempDir()
	if err := mkdirRunTreeScaffold(root, filepath.Join("sky-ai-eng", "triage-factory")); err != nil {
		t.Fatalf("mkdirRunTreeScaffold: %v", err)
	}

	for _, rel := range []string{"sky-ai-eng", filepath.Join("sky-ai-eng", "triage-factory")} {
		st, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if got := st.Mode().Perm(); got != runTreeDirMode {
			t.Errorf("%s mode = %#o; want %#o (umask must not reach the scaffold)", rel, got, runTreeDirMode)
		}
	}
}

// TestMkdirRunTreeScaffold_Idempotent pins the repeat call a second
// `workspace add` under the same owner makes: existing levels are reused, and
// a level whose mode is already correct is left exactly as it is.
func TestMkdirRunTreeScaffold_Idempotent(t *testing.T) {
	root := t.TempDir()
	rel := filepath.Join("sky-ai-eng", "orrery")
	if err := mkdirRunTreeScaffold(root, rel); err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := mkdirRunTreeScaffold(root, rel); err != nil {
		t.Fatalf("second: %v", err)
	}
	if err := mkdirRunTreeScaffold(root, filepath.Join("sky-ai-eng", "sky")); err != nil {
		t.Fatalf("sibling under an existing owner: %v", err)
	}
	st, err := os.Stat(filepath.Join(root, "sky-ai-eng"))
	if err != nil {
		t.Fatalf("stat owner: %v", err)
	}
	if got := st.Mode().Perm(); got != runTreeDirMode {
		t.Errorf("owner mode = %#o; want %#o", got, runTreeDirMode)
	}
}

// TestMkdirRunTreeScaffold_RepairsAnOwnedLevel covers the level minted before
// the mode was asserted at creation: still owned by this process, so the mode
// can be corrected in place rather than leaving a tree that the next relaunch
// would freeze.
func TestMkdirRunTreeScaffold_RepairsAnOwnedLevel(t *testing.T) {
	root := t.TempDir()
	owner := filepath.Join(root, "sky-ai-eng")
	if err := os.Mkdir(owner, 0o755); err != nil {
		t.Fatalf("seed owner dir: %v", err)
	}
	if err := mkdirRunTreeScaffold(root, filepath.Join("sky-ai-eng", "triage-factory")); err != nil {
		t.Fatalf("mkdirRunTreeScaffold: %v", err)
	}
	st, err := os.Stat(owner)
	if err != nil {
		t.Fatalf("stat owner: %v", err)
	}
	if got := st.Mode().Perm(); got != runTreeDirMode {
		t.Errorf("owner mode = %#o; want %#o (a level we own is corrected in place)", got, runTreeDirMode)
	}
}

// TestMkdirRunTreeScaffold_RefusesSymlinkedLevel pins the containment property
// the walk exists for. The run root is writable by the jailed agent, so a
// scaffold level is an inode the agent can replace; resolving the path as a
// string would let it redirect the mode assertion at a directory outside the
// tree. O_NOFOLLOW refuses the entry outright instead.
func TestMkdirRunTreeScaffold_RefusesSymlinkedLevel(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "sky-ai-eng")); err != nil {
		t.Fatalf("plant symlink: %v", err)
	}

	err := mkdirRunTreeScaffold(root, filepath.Join("sky-ai-eng", "triage-factory"))
	if err == nil {
		t.Fatal("mkdirRunTreeScaffold followed a symlinked level; want refusal")
	}
	// Either errno proves the refusal came from the open's own flags rather
	// than from something incidental: O_NOFOLLOW refuses a symlink with ELOOP,
	// and O_DIRECTORY's not-a-directory check answers ENOTDIR first when both
	// flags are set.
	if !errors.Is(err, unix.ELOOP) && !errors.Is(err, unix.ENOTDIR) {
		t.Errorf("error = %v; want ELOOP or ENOTDIR (the open's flags refusing the entry)", err)
	}
	if _, serr := os.Stat(filepath.Join(outside, "triage-factory")); serr == nil {
		t.Error("a directory was created through the symlink, outside the run root")
	}
}

// TestMkdirRunTreeScaffold_RefusesEscapingSubpath covers the owner/repo pair
// arriving from a repository row: the segments are interpolated into a path
// under the run root, so a traversal in either has to fail rather than resolve.
func TestMkdirRunTreeScaffold_RefusesEscapingSubpath(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"..", filepath.Join("..", "elsewhere"), "/etc", "."} {
		if err := mkdirRunTreeScaffold(root, rel); err == nil {
			t.Errorf("subpath %q was accepted; want refusal", rel)
		}
	}
}
