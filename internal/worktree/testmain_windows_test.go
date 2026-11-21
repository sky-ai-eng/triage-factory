//go:build windows

package worktree

import (
	"fmt"
	"os"
	"testing"
)

// TestMain pins core.autocrlf=false for the package's git. Git for Windows
// ships core.autocrlf=true in its system config and many developer profiles
// repeat it, which rewrites every checked-out file to CRLF and breaks the
// byte-exact assertions on file content. The setting rides the env-config
// layer, which outranks every config file, so the system config's git-lfs
// filters (several tests need them) stay in force.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "core.autocrlf",
		"GIT_CONFIG_VALUE_0": "false",
	} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintf(os.Stderr, "worktree tests: setenv %s: %v\n", k, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}
