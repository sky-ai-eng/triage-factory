package domain

import "time"

// RunWorktree is one row in the conversation_worktrees table — a worktree
// reservation associated with one delegated agent run. Lifecycle is
// managed by db.RunWorktreeStore; the value type lives in domain
// because it's passed across packages (cmd/exec/workspace,
// internal/delegate).
//
// Keyed (run_id, repo_id, ref): a single run can materialize several
// worktrees in one repo (e.g. two PRs reviewed in one interactive run),
// so ref discriminates them. Ref is the materialization selector AND the
// worktree's path-slug subdirectory:
//
//   - "@default"  — the repo's default branch (detached checkout)
//   - "pr-<N>"    — pull request N's head
//   - "<branch>"  — a named branch (--ref), path-slugified
//
// It replaced the vestigial feature_branch column (TFAC-502): the push
// gate reads the worktree's live current branch, never this row, so a
// stored prescribed branch carried no authority — ref instead records
// the checkout intent for `workspace list` and the per-run worktree path.
type RunWorktree struct {
	RunID     string
	RepoID    string
	Path      string
	Ref       string
	CreatedAt time.Time
}
