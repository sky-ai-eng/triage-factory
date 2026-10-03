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
// network failure rather than a refusal.
func IsTransientGitError(err error) bool {
	var gitErr *GitError
	if !errors.As(err, &gitErr) {
		return false
	}
	out := strings.ToLower(gitErr.Output)
	for _, marker := range transientGitMarkers {
		if strings.Contains(out, marker) {
			return true
		}
	}
	return false
}
