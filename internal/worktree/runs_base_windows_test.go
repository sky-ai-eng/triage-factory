//go:build windows

package worktree

import (
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// A TEMP spelled with 8.3 short names (`ERIC~1.ARN`) resolves to the long form,
// the path Claude Code realpaths its cwd to.
func TestRunsBase_ResolvesShortTempToLongForm(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	dir := t.TempDir()
	long := longPathName(dir)
	if long == dir {
		t.Skip("temp dir has no short-name spelling on this machine, so there is nothing to resolve")
	}
	setTestTmp(t, dir)
	if got, want := RunsBase(), filepath.Join(long, runsDir); got != want {
		t.Errorf("RunsBase() = %q, want the long form %q", got, want)
	}
}

// A TEMP GetLongPathName cannot resolve (it does not exist) is used as given.
func TestRunsBase_KeepsTempThatCannotBeResolved(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	setTestTmp(t, missing)
	if got, want := RunsBase(), filepath.Join(missing, runsDir); got != want {
		t.Errorf("RunsBase() = %q, want %q", got, want)
	}
}

// NTFS names compare case-insensitively, so a runs base recorded as
// TriageFactory-Runs by another process is still the runs base.
func TestIsRunsBase_IgnoresCaseOnWindows(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	if !isRunsBase(filepath.Join(t.TempDir(), "TriageFactory-Runs")) {
		t.Error("isRunsBase rejected a differently-cased runs base name")
	}
	if isRunsBase(filepath.Join(t.TempDir(), "triagefactory-run")) {
		t.Error("isRunsBase accepted a different name")
	}
}
