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

// platformGitEnvVars stops every interactive credential prompt the agent's git
// could open: GCM_INTERACTIVE=never for Git Credential Manager should a helper
// still be reached, and GIT_TERMINAL_PROMPT=0 for git's own username and
// password prompt.
func platformGitEnvVars() []string {
	return []string{"GCM_INTERACTIVE=never", "GIT_TERMINAL_PROMPT=0"}
}

// platformGitEnvStrip names inherited variables dropped from the agent's env
// with no replacement. An askpass program is how git, and ssh through git, ask
// for a secret without a terminal; an editor that launched TF commonly exports
// one that answers from the operator's signed-in session, which would hand the
// agent that credential. SSH_ASKPASS_REQUIRE makes ssh use that program even
// with a terminal, and VS Code routes its askpass through VSCODE_GIT_ASKPASS_*
// and an IPC handle to the editor, so every VSCODE_GIT_ variable goes too (a
// trailing * names a prefix). With these gone git has no helper to ask, and the
// prompts above are off.
//
// This is a courtesy, not a boundary: the agent runs as the operator on
// Windows, with no sandbox, and can call a credential manager or read the
// stored credentials itself. It only keeps git from handing out a session's
// credential by default.
func platformGitEnvStrip() []string {
	return []string{"GIT_ASKPASS", "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE", "VSCODE_GIT_*"}
}
