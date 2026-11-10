//go:build windows

package githooks

// platformGitConfigPairs are the git config entries the agent's git carries on
// Windows beyond core.hooksPath: core.longpaths lifts the 260-character limit
// for a checkout under a deep run root, and an empty credential.helper resets
// the helper list so Git Credential Manager never prompts or stores a token.
func platformGitConfigPairs() [][2]string {
	return [][2]string{
		{"core.longpaths", "true"},
		{"credential.helper", ""},
	}
}

// platformGitEnvVars is GCM_INTERACTIVE=never, which stops Git Credential
// Manager prompting should a helper still be reached.
func platformGitEnvVars() []string { return []string{"GCM_INTERACTIVE=never"} }
