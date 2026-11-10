//go:build !windows

package agentproc

// checkGitBash is a no-op off Windows: the agent's shell is the host's own.
func checkGitBash() error { return nil }

// gitBashEnv adds nothing off Windows.
func gitBashEnv() []string { return nil }
