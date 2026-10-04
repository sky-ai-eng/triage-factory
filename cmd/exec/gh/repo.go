package gh

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/cmd/exec/prog"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// resolveRepo determines the target (owner, repo) for a gh subcommand
// by inspecting the full args slice plus the current directory.
//
// Resolution order, highest priority first:
//
//  1. --repo owner/repo flag. If --repo is present in args but has no
//     value (e.g. it's the last token), we error immediately — the user
//     expressed explicit intent and the safe behavior is to fail loudly
//     rather than silently fall through and possibly target the wrong repo.
//  2. remote.origin.url of the checkout containing the current directory.
//     `git config` finds the checkout from any subfolder of it, and a
//     self-contained PR clone's origin is the base repo, which is what the
//     PR verbs address.
//
// Nothing run-scoped stands in between: a run that works in two repos would
// otherwise keep addressing the first one after the agent moved to the second.
// From a folder outside every checkout the error lists the run's checkouts
// (read through checkouts, which may be nil) so the agent can cd into the
// right one. Never falls back to a default — running a gh command against the
// wrong repo (log downloads, comments, reviews) is costly enough to warrant a
// hard error over a silent misfire.
func resolveRepo(ctx context.Context, checkouts runCheckouts, args []string) (owner, repo string, err error) {
	// 1. Explicit flag. hasFlag + flagVal together disambiguate "flag
	// not present" from "flag present but empty" — the latter is
	// user error, the former is a normal fallthrough to git.
	if hasFlag(args, "--repo") {
		flagValue := flagVal(args, "--repo")
		if flagValue == "" {
			return "", "", fmt.Errorf("--repo requires a value in the form owner/repo")
		}
		return splitOwnerRepoStr(flagValue, "--repo flag")
	}

	// 2. origin of the checkout containing the current directory.
	out, gitErr := exec.Command("git", "config", "--get", "remote.origin.url").Output()
	if gitErr == nil {
		if o, r, ok := parseGitRemoteURL(strings.TrimSpace(string(out))); ok {
			return o, r, nil
		}
	}

	return "", "", noCheckoutError(ctx, checkouts)
}

// runCheckouts is the part of agenthost.Client the gh verbs read the run's
// checkouts through: the conversation_worktrees rows, whose paths are recorded
// in host view, and the root pair that translates them into this process's.
type runCheckouts interface {
	ListConversationWorktrees(ctx context.Context) ([]domain.ConversationWorktree, error)
	WorkspaceRoots(ctx context.Context) (hostRoot, agentRoot string, err error)
}

// listRunCheckouts returns the run's materialized checkouts with each path in
// this process's view — a directory it can cd into or run git against.
func listRunCheckouts(ctx context.Context, checkouts runCheckouts) ([]domain.ConversationWorktree, error) {
	rows, err := checkouts.ListConversationWorktrees(ctx)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	hostRoot, agentRoot, err := checkouts.WorkspaceRoots(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.ConversationWorktree, len(rows))
	for i, w := range rows {
		w.Path = agenthost.AgentViewPath(hostRoot, agentRoot, w.Path)
		out[i] = w
	}
	return out, nil
}

// noCheckoutError is resolveRepo's failure from a folder that is not inside a
// checkout with a GitHub origin. It names the run's checkouts, one
// `owner/repo  <path>` line each, because the fix is almost always to cd into
// one of them. When the list can't be read (a manual invocation outside a run)
// the error still names both ways out.
func noCheckoutError(ctx context.Context, checkouts runCheckouts) error {
	const head = "could not resolve the repo: the current directory is not inside a checkout with a GitHub origin remote"
	if checkouts != nil {
		if rows, err := listRunCheckouts(ctx, checkouts); err == nil {
			if len(rows) == 0 {
				return fmt.Errorf("%s, and this run has no checkouts yet. Pass --repo owner/repo, or run `%s workspace add <owner/repo>` and cd into the path it prints", head, prog.Prefix())
			}
			var b strings.Builder
			b.WriteString(head)
			b.WriteString(". This run's checkouts:\n")
			for _, w := range rows {
				fmt.Fprintf(&b, "  %s  %s\n", w.RepoID, w.Path)
			}
			b.WriteString("cd into the one you mean, or pass --repo owner/repo")
			return errors.New(b.String())
		}
	}
	return fmt.Errorf("%s. cd into a checkout of the repo you mean, or pass --repo owner/repo", head)
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
