//go:build windows

package agentproc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// envGitBashPath is the variable Claude Code reads to find the bash it runs
// the agent's shell commands in. Claude Code on Windows has no other shell: it
// needs Git for Windows' bash and refuses to start without one.
const envGitBashPath = "CLAUDE_CODE_GIT_BASH_PATH"

// checkGitBash fails when the agent runtime cannot find a Git Bash, with the
// two ways to fix it. It runs before the SDK install is consulted, so a host
// with the SDK already installed but no Git for Windows is refused here and
// not with Claude Code's own error from inside a run.
func checkGitBash() error {
	_, err := resolveGitBash()
	return err
}

// gitBashEnv is the CLAUDE_CODE_GIT_BASH_PATH entry for the agent's
// environment: the bash TF resolved, so Claude Code uses the one the preflight
// approved rather than searching on its own. Empty when none resolves, which
// the preflight has already refused; a spawn that gets here anyway (a PATH
// changed since boot) lets Claude Code report its own error.
func gitBashEnv() []string {
	bash, err := resolveGitBash()
	if err != nil {
		agentprocLog.Warn("no Git Bash to hand the agent runtime", "error", err)
		return nil
	}
	return []string{envGitBashPath + "=" + bash}
}

// resolveGitBash finds Git Bash from this process's environment.
func resolveGitBash() (string, error) {
	return findGitBash(os.Getenv(envGitBashPath), exec.LookPath)
}

// findGitBash resolves Git Bash from an explicit setting and, failing that,
// from where `git` is installed. It never searches PATH for bash itself: a
// bare `bash` on a Windows PATH is as likely to be the WSL launcher in
// System32, which runs a Linux userland where Windows paths mean nothing.
//
// An explicit setting that names no file is an error, not a cue to look
// elsewhere: the user asked for that bash, and quietly using a different one
// hides the mistake.
func findGitBash(setting string, lookPath func(string) (string, error)) (string, error) {
	if setting = strings.TrimSpace(setting); setting != "" {
		if !isRegularFile(setting) {
			return "", fmt.Errorf("%s=%q is not a file: point it at the bash.exe of a Git for Windows install, or unset it", envGitBashPath, setting)
		}
		return setting, nil
	}
	gitPath, err := lookPath("git")
	if err != nil {
		return "", errNoGitBash(fmt.Errorf("git is not on PATH: %w", err))
	}
	// Git for Windows puts git.exe in <root>\cmd (what its installer adds to
	// PATH) or in <root>\mingw64\bin, with bash in <root>\bin.
	dir := filepath.Dir(gitPath)
	for _, rel := range []string{filepath.Join("..", "bin", "bash.exe"), filepath.Join("..", "..", "bin", "bash.exe")} {
		candidate := filepath.Clean(filepath.Join(dir, rel))
		if isRegularFile(candidate) {
			return candidate, nil
		}
	}
	return "", errNoGitBash(fmt.Errorf("found git at %s but no bash.exe in the install's bin directory", gitPath))
}

func errNoGitBash(cause error) error {
	return fmt.Errorf("the agent runtime needs Git Bash, which ships with Git for Windows (https://git-scm.com/download/win): %w. "+
		"Install it, or set %s to the full path of its bash.exe", cause, envGitBashPath)
}

func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			agentprocLog.Warn("stat candidate Git Bash failed", "path", path, "error", err)
		}
		return false
	}
	return info.Mode().IsRegular()
}
