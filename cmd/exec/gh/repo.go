package gh

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// resolveRepo determines the target (owner, repo) for a gh subcommand
// by inspecting the full args slice plus ambient state.
//
// Resolution order, highest priority first:
//
//  1. --repo owner/repo flag. If --repo is present in args but has no
//     value (e.g. it's the last token), we error immediately — the user
//     expressed explicit intent and the safe behavior is to fail loudly
//     rather than silently fall through to env/git resolution and
//     possibly target the wrong repo.
//  2. TRIAGE_FACTORY_REPO env var (set by the spawner for delegated runs;
//     never has a value for Jira-without-repo runs).
//  3. git config remote.origin.url of the current working directory
//     (fallback for manual invocation from a checkout).
//
// Returns a clear error if none of the above resolve. Never falls back
// to a hardcoded default — running a gh command against the wrong repo
// (log downloads, comments, reviews) is costly enough to warrant a hard
// error over a silent misfire.
func resolveRepo(args []string) (owner, repo string, err error) {
	// 1. Explicit flag. hasFlag + flagVal together disambiguate "flag
	// not present" from "flag present but empty" — the latter is
	// user error, the former is a normal fallthrough to env/git.
	if hasFlag(args, "--repo") {
		flagValue := flagVal(args, "--repo")
		if flagValue == "" {
			return "", "", fmt.Errorf("--repo requires a value in the form owner/repo")
		}
		return splitOwnerRepoStr(flagValue, "--repo flag")
	}

	// 2. Env var from delegation context
	if env := os.Getenv("TRIAGE_FACTORY_REPO"); env != "" {
		return splitOwnerRepoStr(env, "TRIAGE_FACTORY_REPO env var")
	}

	// 3. git config origin of cwd
	cmd := exec.Command("git", "config", "--get", "remote.origin.url")
	out, gitErr := cmd.Output()
	if gitErr == nil {
		if o, r, ok := parseGitRemoteURL(strings.TrimSpace(string(out))); ok {
			return o, r, nil
		}
	}

	return "", "", fmt.Errorf("could not resolve repo: pass --repo owner/repo, set TRIAGE_FACTORY_REPO, or run from a git checkout with an origin remote")
}

// splitOwnerRepoStr splits an "owner/repo" string, returning a descriptive
// error tied to the source (flag, env, etc.) so failures are diagnosable.
//
// owner and repo must each be a single path segment. GitHub names never
// contain slashes, so rejecting them isn't a usability cost — and it's a
// security guard: owner/repo flow into filesystem paths (e.g. the pr-diff
// _tfac directory), where a crafted "--repo owner/../../.." would
// otherwise let filepath.Join + Clean escape the intended directory and a
// subsequent RemoveAll touch paths outside it.
func splitOwnerRepoStr(value, source string) (owner, repo string, err error) {
	parts := strings.SplitN(value, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid %s: expected owner/repo, got %q", source, value)
	}
	owner, repo = parts[0], parts[1]
	if !validRepoComponent(owner) || !validRepoComponent(repo) {
		return "", "", fmt.Errorf("invalid %s: owner and repo must each be a single path segment (no '/', '\\', or '..'), got %q", source, value)
	}
	return owner, repo, nil
}

// validRepoComponent reports whether s is safe to use as a single owner or
// repo path segment: non-empty, not a directory-traversal token, and free of
// path separators.
func validRepoComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, `/\`)
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
// triage-factory is GitHub-focused and GitHub paths are always exactly
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
