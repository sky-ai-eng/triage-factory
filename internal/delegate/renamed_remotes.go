package delegate

import (
	"context"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// renamedRemoteTimeoutSec bounds the jail-side rewrite: a handful of
// `git config` writes, with no network.
const renamedRemoteTimeoutSec = 30

// renamedRemotes returns the remote URL renames the task's checkouts under
// root need: a checkout made before its repository was renamed sits under the
// old name (snapshotCheckouts), and the remotes TF set when it made the
// checkout name the repository that way too. Everything that reads a remote
// learns the repository's name from it — git, whose fetches and pushes the
// repo gates and the credential lookup behind them match by name, and the gh
// verbs, which address the checkout's origin — so each is moved to the name
// the registry holds now.
//
// Best-effort: an unreadable ledger, repository row or checkout config leaves
// that checkout's remotes as they are.
func (s *Spawner) renamedRemotes(ctx context.Context, orgID, taskID, root string) []worktree.RemoteRename {
	if s.repos == nil {
		return nil
	}
	checkouts, err := s.snapshotCheckouts(ctx, orgID, taskID, root)
	if err != nil {
		delegateLog.Warn("list the task's checkouts to repoint renamed remotes failed", "task", taskID, "root", root, "error", err)
		return nil
	}
	rows := map[string]*domain.Repository{}
	// Linked worktrees of one repository share their bare's config, and a
	// rename applied twice would add the new URL a second time.
	seen := map[worktree.RemoteRename]bool{}
	var out []worktree.RemoteRename
	for _, c := range checkouts {
		row, ok := rows[c.repositoryID]
		if !ok {
			if row, err = s.repos.GetSystem(ctx, orgID, c.repositoryID); err != nil {
				delegateLog.Warn("read a checkout's repository to repoint renamed remotes failed", "repository_id", c.repositoryID, "error", err)
			}
			rows[c.repositoryID] = row
		}
		if row == nil || domain.SameRepoSlug(c.repoID, row.Slug()) {
			continue
		}
		renames, err := worktree.StaleRemoteURLs(c.path, row.Host, c.repoID, row.Slug())
		if err != nil {
			delegateLog.Warn("read a checkout's remotes to repoint them failed", "checkout", c.path, "error", err)
			continue
		}
		for _, r := range renames {
			if !seen[r] {
				seen[r] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// repointRenamedRemotes moves the remotes of the task's checkouts under root
// onto their repositories' current names (renamedRemotes), from this process.
// A tree already handed to a sandbox identity is not this process's to write;
// its engagement does the same from inside the jail
// (repointRenamedRemotesInJail).
func (s *Spawner) repointRenamedRemotes(ctx context.Context, orgID, taskID, root string) {
	if sandbox.RunTreeHandedOff(root) {
		return
	}
	renames := s.renamedRemotes(ctx, orgID, taskID, root)
	if len(renames) == 0 {
		return
	}
	if err := worktree.RewriteRemoteURLs(ctx, renames); err != nil {
		delegateLog.Warn("repoint the remotes of renamed repositories failed", "task", taskID, "root", root, "error", err)
		return
	}
	delegateLog.Info("repointed remotes of renamed repositories", "task", taskID, "root", root, "remotes", len(renames))
}

// repointRenamedRemotesInJail is repointRenamedRemotes for a tree the sandbox
// identity owns, which only that identity can write: the same `git config`
// writes, run by the engagement's tool host before the loop starts, so the
// agent's first git command already sees the new names. The paths are the
// jail's view of the tree.
func (s *Spawner) repointRenamedRemotesInJail(ctx context.Context, orgID, taskID, root string, tools agentloop.ToolHost) {
	renames := s.renamedRemotes(ctx, orgID, taskID, root)
	if len(renames) == 0 {
		return
	}
	cmd := renamedRemotesScript(root, renames)
	out, err := tools.Call("bash", map[string]any{"command": cmd, "timeout": renamedRemoteTimeoutSec})
	switch {
	case err != nil:
		delegateLog.Warn("repoint the remotes of renamed repositories in the jail failed", "task", taskID, "error", err)
	case out.Protocol != nil:
		delegateLog.Warn("repoint the remotes of renamed repositories in the jail failed", "task", taskID, "kind", out.Protocol.Kind)
	case out.ToolError != "":
		delegateLog.Warn("repoint the remotes of renamed repositories in the jail failed", "task", taskID, "error", out.ToolError)
	default:
		delegateLog.Info("repointed remotes of renamed repositories in the jail", "task", taskID, "remotes", len(renames))
	}
}

// renamedRemotesScript is the shell script applying renames to the tree at
// root as the jail sees it. It stops at the first failure.
func renamedRemotesScript(root string, renames []worktree.RemoteRename) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	for _, r := range renames {
		b.WriteString("git")
		for _, a := range r.Args(agentVisibleCheckout(root, r.ConfigPath)) {
			b.WriteString(" " + shellQuote(a))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// shellQuote quotes s as a single word for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
