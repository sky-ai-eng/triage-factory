package worktree

import "testing"

// setTestHome points every spelling of the home directory at dir: HOME for the
// Unix lookups and USERPROFILE, which is what os.UserHomeDir reads on Windows.
// A test that sets only HOME would write into the developer's real profile
// there.
func setTestHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

// setTestTmp points every spelling of the temp directory at dir: TMPDIR for
// Unix and TMP/TEMP, which is what os.TempDir reads on Windows.
func setTestTmp(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
}

// longTestTempDir is t.TempDir() in the long-name form RunsBase resolves its
// temp dir to, so a test can compare paths against it byte for byte.
func longTestTempDir(t *testing.T) string {
	t.Helper()
	return longPathName(t.TempDir())
}
