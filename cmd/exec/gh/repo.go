package gh

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// resolveRepo determines the target (owner, repo) for a gh subcommand.
//
// Resolution order, highest priority first:
//
//  1. Explicit --repo owner/repo flag (caller passes the value already
//     extracted from args via flagVal)
//  2. TODOTRIAGE_REPO env var (set by the spawner for delegated runs; never
//     has a value for Jira-without-repo runs)
//  3. git config remote.origin.url of the current working directory
//     (fallback for manual invocation from a checkout)
//
// Returns a clear error if none of the above resolve. Never falls back to
// a hardcoded default — running a gh command against the wrong repo (log
// downloads, comments, reviews) is costly enough to warrant a hard error
// over a silent misfire.
func resolveRepo(flagValue string) (owner, repo string, err error) {
	// 1. Explicit flag
	if flagValue != "" {
		return splitOwnerRepoStr(flagValue, "--repo flag")
	}

	// 2. Env var from delegation context
	if env := os.Getenv("TODOTRIAGE_REPO"); env != "" {
		return splitOwnerRepoStr(env, "TODOTRIAGE_REPO env var")
	}

	// 3. git config origin of cwd
	cmd := exec.Command("git", "config", "--get", "remote.origin.url")
	out, gitErr := cmd.Output()
	if gitErr == nil {
		if o, r, ok := parseGitRemoteURL(strings.TrimSpace(string(out))); ok {
			return o, r, nil
		}
	}

	return "", "", fmt.Errorf("could not resolve repo: pass --repo owner/repo, set TODOTRIAGE_REPO, or run from a git checkout with an origin remote")
}

// splitOwnerRepoStr splits an "owner/repo" string, returning a descriptive
// error tied to the source (flag, env, etc.) so failures are diagnosable.
func splitOwnerRepoStr(value, source string) (owner, repo string, err error) {
	parts := strings.SplitN(value, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid %s: expected owner/repo, got %q", source, value)
	}
	return parts[0], parts[1], nil
}

// parseGitRemoteURL extracts owner and repo from any of git's common remote
// URL formats. Returns ok=false for unparseable input rather than an error
// because the caller treats .git/config as a best-effort fallback.
//
// Supported:
//
//	https://github.com/owner/repo.git
//	https://github.com/owner/repo
//	git@github.com:owner/repo.git
//	git@github.com:owner/repo
//	ssh://git@github.com/owner/repo.git
//	git://github.com/owner/repo.git
func parseGitRemoteURL(url string) (owner, repo string, ok bool) {
	if url == "" {
		return "", "", false
	}

	// SCP-style: git@host:owner/repo(.git)
	if strings.HasPrefix(url, "git@") {
		colon := strings.Index(url, ":")
		if colon < 0 {
			return "", "", false
		}
		return splitRepoPath(url[colon+1:])
	}

	// URL-style: scheme://host/owner/repo(.git)
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://"} {
		if !strings.HasPrefix(url, prefix) {
			continue
		}
		rest := url[len(prefix):]
		slash := strings.Index(rest, "/")
		if slash < 0 {
			return "", "", false
		}
		return splitRepoPath(rest[slash+1:])
	}

	return "", "", false
}

// splitRepoPath takes the path portion of a git URL (after the host) and
// extracts owner + repo, stripping trailing slashes and the .git suffix.
//
// Requires exactly two path segments. Multi-segment paths are rejected as
// ambiguous rather than guessing which segments form the owner/repo pair:
//
//   - Bitbucket's /scm/project/repo.git — taking the first two silently
//     targets "scm/project" instead of "project/repo"; taking the last
//     two works here but fails elsewhere
//   - GitLab nested groups /group/subgroup/repo.git — neither "first two"
//     nor "last two" is universally correct without knowing how the user
//     wants nested groups flattened
//   - GHES/Gitea custom layouts
//
// todo-triage is GitHub-focused and GitHub paths are always exactly
// owner/repo, so a 2-segment requirement covers every supported case.
// Users with non-GitHub remotes get a clean rejection from resolveRepo
// and a clear prompt to pass --repo explicitly instead of silently
// targeting the wrong repository.
func splitRepoPath(path string) (owner, repo string, ok bool) {
	// Tolerate a trailing slash, then the .git suffix, then another
	// trailing slash (for the unusual "owner/repo.git/" form).
	path = strings.TrimSuffix(path, "/")
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
