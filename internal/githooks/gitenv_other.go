//go:build !windows

package githooks

// platformGitConfigPairs is empty off Windows: the agent's git environment
// there is unchanged.
func platformGitConfigPairs() [][2]string { return nil }

// platformGitEnvVars is empty off Windows.
func platformGitEnvVars() []string { return nil }
