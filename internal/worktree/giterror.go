package worktree

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// GitError is a failed git command with its arguments, combined output, and
// underlying process or context error.
type GitError struct {
	Args   []string
	Output string // combined stdout/stderr
	Err    error  // the *exec.ExitError or the context error
	// Proxy is the base URL of the per-run git proxy the command's network
	// git was routed through (CloneAuthViaGitProxy), empty when it went to
	// the git host directly.
	Proxy string
}

// Error is "cancelled" for a command its context stopped, and the exit error
// followed by git's output otherwise.
func (e *GitError) Error() string {
	if errors.Is(e.Err, context.Canceled) || errors.Is(e.Err, context.DeadlineExceeded) {
		return "cancelled"
	}
	return fmt.Sprintf("%s: %s", e.Err, e.Output)
}

func (e *GitError) Unwrap() error { return e.Err }

// transientGitMarkers are the phrases git prints when it could not reach the
// remote or lost the connection to it, lowercased. Each one is a failure of
// the network or of the server, never an answer about the request, so the
// list leaves out authentication and not-found errors on purpose: "repository
// not found", "authentication failed" and a 403 all say the same thing on
// the next attempt. The statuses are the ones upstream.ClassifyResponse reads
// as Transient or RateLimited (a 5xx, a 408, a 429), matched in git's "The
// requested URL returned error: <status>" line, which is also how a per-run
// git proxy's 502 for an unreachable upstream reaches git.
var transientGitMarkers = []string{
	"could not resolve host",
	"connection timed out",
	"operation timed out",
	"connection refused",
	"failed to connect",
	"connection reset",
	"early eof",
	"rpc failed",
	"the requested url returned error: 5",
	"the requested url returned error: 408",
	"the requested url returned error: 429",
	"gnutls_handshake",
	"ssl_error",
}

// IsTransientGitError reports whether err is a GitError whose output names a
// network failure rather than a refusal. A command its deadline stopped is
// never one, whatever it printed before it was stopped: it ran out of the
// time it was given, which is not an answer from the remote.
//
// Nor is a command routed through the run's own git proxy whose connection
// to the proxy never opened. Every network request of that command goes to
// the proxy, so the failure says the proxy is down, a fault on this host. The
// git host being unreachable behind a live proxy reaches git as the proxy's
// 502, and a transfer dropped partway can be the host's drop passed through,
// so both still count.
func IsTransientGitError(err error) bool {
	var gitErr *GitError
	if !errors.As(err, &gitErr) || errors.Is(gitErr.Err, context.DeadlineExceeded) {
		return false
	}
	out := strings.ToLower(gitErr.Output)
	if gitErr.Proxy != "" && neverReachedProxy(out, strings.ToLower(gitErr.Proxy)) {
		return false
	}
	for _, marker := range transientGitMarkers {
		if strings.Contains(out, marker) {
			return true
		}
	}
	return false
}

// neverReachedProxy reports whether a command's lowercased output says git
// could not open a connection to proxy, the base URL git reports as the one
// it could not access when the command is routed through it.
func neverReachedProxy(out, proxy string) bool {
	if !strings.Contains(out, "unable to access '"+strings.TrimRight(proxy, "/")+"/") {
		return false
	}
	return strings.Contains(out, "failed to connect") || strings.Contains(out, "couldn't connect to server")
}

// isMissingRemoteRef reports whether err is a fetch the remote answered by
// saying it has no such ref — a branch deleted upstream, or one never pushed.
func isMissingRemoteRef(err error) bool {
	var gitErr *GitError
	return errors.As(err, &gitErr) && strings.Contains(strings.ToLower(gitErr.Output), "couldn't find remote ref")
}
