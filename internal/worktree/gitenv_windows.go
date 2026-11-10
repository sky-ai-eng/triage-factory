//go:build windows

package worktree

// platformGitConfigPairs are the git config entries TF's own git calls carry on
// Windows. core.longpaths lifts the 260-character limit for checkouts under a
// deep run root; an empty credential.helper resets the helper list so Git
// Credential Manager (installed with Git for Windows and configured system
// wide) never opens a prompt or stores a token TF handed git through an
// extraHeader.
func platformGitConfigPairs() [][2]string {
	return [][2]string{
		{"core.longpaths", "true"},
		{"credential.helper", ""},
	}
}

// platformGitEnvVars are the environment variables TF's git calls carry on
// Windows: GCM_INTERACTIVE=never stops Git Credential Manager from prompting
// should a helper still be reached.
func platformGitEnvVars() []string { return []string{"GCM_INTERACTIVE=never"} }
