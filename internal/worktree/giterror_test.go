package worktree

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// TestGitError_RendersTheTextGitRunCtxAuthAlwaysHas: the typed error says
// exactly what the formatted one did, so a caller matching on the text sees
// no change, and errors.As still reaches the process's exit.
func TestGitError_RendersTheTextGitRunCtxAuthAlwaysHas(t *testing.T) {
	dir := t.TempDir()
	args := []string{"rev-parse", "--verify", "refs/heads/does-not-exist"}

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitBaseEnv()
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatal("fixture command succeeded; it has to fail")
	}
	want := fmt.Sprintf("%s: %s", runErr, string(out))

	err := gitRunCtx(context.Background(), dir, args...)
	if err == nil {
		t.Fatal("gitRunCtx succeeded on a command that fails")
	}
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	var gitErr *GitError
	if !errors.As(err, &gitErr) {
		t.Fatalf("gitRunCtx returned %T, want *GitError", err)
	}
	if gitErr.Output != string(out) || len(gitErr.Args) != len(args) || gitErr.Args[0] != args[0] {
		t.Errorf("GitError = %+v, want the command's args and output", gitErr)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("errors.As does not reach the *exec.ExitError through %T", err)
	}
	if exitErr.ExitCode() == 0 {
		t.Errorf("exit code = 0 for a failed command")
	}

	wrapped := fmt.Errorf("fetch ref main: %w", err)
	if !errors.As(wrapped, &gitErr) {
		t.Error("a %w wrap hides the GitError")
	}
}

// TestGitError_ACancelledCommandSaysCancelled: a command its context stopped
// keeps the text it always had, and unwraps to the context's error.
func TestGitError_ACancelledCommandSaysCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := gitRunCtx(ctx, t.TempDir(), "status")
	if err == nil {
		t.Fatal("a cancelled command succeeded")
	}
	if err.Error() != "cancelled" {
		t.Errorf("Error() = %q, want cancelled", err.Error())
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(%v, context.Canceled) = false", err)
	}
	if IsTransientGitError(err) {
		t.Error("a cancelled command reads as a network failure")
	}
}

func TestIsTransientGitError(t *testing.T) {
	gitErr := func(output string) error {
		return fmt.Errorf("bare clone: %w", &GitError{Args: []string{"clone"}, Output: output, Err: errors.New("exit status 128")})
	}
	transient := map[string]string{
		"could not resolve host":                "fatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com",
		"connection timed out":                  "fatal: unable to access 'https://github.com/o/r/': Failed to connect to github.com port 443: Connection timed out",
		"operation timed out":                   "fatal: unable to access 'https://github.com/o/r/': Operation timed out after 300000 milliseconds with 0 out of 0 bytes received",
		"connection refused":                    "ssh: connect to host github.com port 22: Connection refused",
		"failed to connect":                     "fatal: unable to access 'https://github.com/o/r/': Failed to connect to proxy.internal port 3128",
		"connection reset":                      "fatal: unable to access 'https://github.com/o/r/': Recv failure: Connection reset by peer",
		"early eof":                             "fetch-pack: unexpected disconnect while reading sideband packet\nfatal: early EOF",
		"rpc failed":                            "error: RPC failed; curl 92 HTTP/2 stream 5 was not closed cleanly: CANCEL (err 8)",
		"the requested url returned error: 5":   "fatal: unable to access 'http://127.0.0.1:41000/o/r/': The requested URL returned error: 502",
		"the requested url returned error: 429": "fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 429",
		"gnutls_handshake":                      "fatal: unable to access 'https://github.com/o/r/': gnutls_handshake() failed: The TLS connection was non-properly terminated.",
		"ssl_error":                             "fatal: unable to access 'https://github.com/o/r/': OpenSSL SSL_connect: SSL_ERROR_SYSCALL in connection to github.com:443",
	}
	for marker, output := range transient {
		if !IsTransientGitError(gitErr(output)) {
			t.Errorf("%s: IsTransientGitError(%q) = false, want true", marker, output)
		}
	}
	covered := map[string]bool{}
	for _, marker := range transientGitMarkers {
		for name := range transient {
			if name == marker {
				covered[marker] = true
			}
		}
	}
	for _, marker := range transientGitMarkers {
		if !covered[marker] {
			t.Errorf("marker %q has no case above", marker)
		}
	}

	refusals := []string{
		"remote: Repository not found.\nfatal: repository 'https://github.com/o/r/' not found",
		"fatal: Authentication failed for 'https://github.com/o/r/'",
		"fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 403",
		"fatal: unable to access 'https://github.com/o/r/': The requested URL returned error: 404",
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled",
		"git@github.com: Permission denied (publickey).\nfatal: Could not read from remote repository.",
		"fatal: couldn't find remote ref refs/heads/gone",
		"",
	}
	for _, output := range refusals {
		if IsTransientGitError(gitErr(output)) {
			t.Errorf("IsTransientGitError(%q) = true, want false — a refusal says the same thing on the next attempt", output)
		}
	}

	if IsTransientGitError(errors.New("fatal: unable to access: Could not resolve host: github.com")) {
		t.Error("IsTransientGitError matched text on an error that is not a GitError")
	}
	if IsTransientGitError(nil) {
		t.Error("IsTransientGitError(nil) = true")
	}
}
