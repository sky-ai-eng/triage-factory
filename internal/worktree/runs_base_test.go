package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestEnsureRunsBase_ModeByRunMode pins who the runs base admits. In multi a
// jail identity on the host has to traverse it to its own tree, so the mode is
// pinned to runsBaseMode whatever the directory had. Locally nothing but this
// uid reaches a run tree: the base is created owner-only, and one that already
// exists is left with its own mode.
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
			base := filepath.Join(tmp, runsDir)
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
