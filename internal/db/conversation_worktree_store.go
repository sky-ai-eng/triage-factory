package db

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

//go:generate go run github.com/vektra/mockery/v2 --name=ConversationWorktreeStore --output=./mocks --case=underscore --with-expecter

// ConversationWorktreeStore owns the conversation_worktrees table — one row per
// (run_id, repository, ref) reservation tracking the lazy worktree
// materializations a run accumulates as the agent calls
// `triagefactory exec workspace add` against each repo it needs. The
// ref discriminator (TFAC-502) lets a single run hold several worktrees
// in one repo (e.g. two PRs reviewed in one interactive run): "@default",
// "pr-<N>", or a slugified branch name.
//
// The row references the repository by its registry row id, so a rename moves
// nothing here. Every method still takes and returns repoID as "owner/repo" —
// the caller is an agent's argv — and the impls resolve it. Resolution is a
// LOOKUP, never a create: in multi mode this runs on the executor, whose
// Postgres role holds no INSERT on repositories at all. A worktree is reserved
// for a repository the run was already authorized to clone, so the registry
// row exists; Insert reports an error rather than minting one if it does not.
//
// Lifted out of the pre-D2 package-level functions in
// internal/db/conversation_worktrees.go so multi-mode Postgres callers route
// through $N placeholders + explicit org_id + the dual-pool
// admin/app split.
//
// Method naming follows the dual-pool convention from
// TaskMemoryStore / EventStore / UsersStore:
//
//   - Plain methods (Insert, GetByRepoRef, List, DeleteByRepoRef) run on
//     the app pool in Postgres (RLS-active). Callers are the agent
//     CLI subcommand cmd/exec/workspace, which runs as a subprocess
//     of the delegated agent. The CLI's pool routing is the
//     responsibility of a separate cmd/exec auth pass; local mode
//     is unaffected because SQLite has no auth concept.
//
//   - `...System` methods (ListSystem, DeleteByPathSystem) run on
//     the admin pool (BYPASSRLS). Consumers are background
//     goroutines without a JWT-claims context — the delegate
//     spawner's runAgent / chain orchestrator clean up materialized
//     worktrees on terminal exit.
//
// SQLite collapses both pools onto the single connection. The
// `...System` methods are thin wrappers around their non-System
// counterparts; assertLocalOrg gates every entry point.
type ConversationWorktreeStore interface {
	// Insert reserves a row for a worktree the caller is about to
	// create on disk. Used as the cross-process serialization point:
	// two concurrent `workspace add owner/repo` invocations for the
	// same (run, repo, ref) that both passed the GetByRepoRef "not
	// found" check race here, and the PK conflict on (run_id, repo_id,
	// ref) deterministically picks one winner.
	//
	// On PK conflict the winning row's path is returned with
	// inserted=false so the caller skips its create step entirely.
	// On a fresh insert returns inserted=true and the same path the
	// caller supplied.
	Insert(ctx context.Context, orgID string, w domain.ConversationWorktree) (inserted bool, winningPath string, err error)

	// GetByRepoRef fetches the worktree row for a (run_id, repo_id,
	// ref) triple, or (nil, nil) if none exists. Used by the workspace
	// CLI to short-circuit the create+insert path when the agent
	// re-invokes `workspace add` against an already-materialized
	// (repo, ref).
	GetByRepoRef(ctx context.Context, orgID, conversationID, repoID, ref string) (*domain.ConversationWorktree, error)

	// List returns every worktree materialized for a run, in
	// insertion order. The spawner's cleanup defer iterates this
	// list and calls worktree.RemoveAt on each path before nuking
	// the run-root.
	List(ctx context.Context, orgID, conversationID string) ([]domain.ConversationWorktree, error)

	// ListSystem mirrors List for goroutine-internal callers. The
	// delegate spawner's runAgent + chain orchestrator cleanup
	// defers iterate this from a context detached from any request
	// — no JWT claims in scope.
	ListSystem(ctx context.Context, orgID, conversationID string) ([]domain.ConversationWorktree, error)

	// DeleteByRepoRef removes the row for a (run_id, repo_id, ref)
	// triple. Used by the workspace CLI to release a reservation after
	// createWorktree fails, or to clear a stale row whose on-disk
	// path was reaped (e.g. startup orphan sweep) so a subsequent
	// `workspace add` can re-reserve. Idempotent: deleting a row
	// that doesn't exist is a no-op (no error).
	DeleteByRepoRef(ctx context.Context, orgID, conversationID, repoID, ref string) error

	// DeleteByPathSystem removes the row for a (run_id, path)
	// pair. Used by the spawner cleanup defer that iterates List
	// and removes worktree rows one-by-one as their on-disk dirs
	// are reaped, so a per-path failure to remove from disk leaves
	// the corresponding DB row intact for the next sweep. Admin
	// pool only; the only caller is the delegate goroutine.
	DeleteByPathSystem(ctx context.Context, orgID, conversationID, path string) error

	// --- Admin-pool variants for the cmd/exec event-triggered branch ---
	//
	// `triagefactory exec workspace add` invoked by an event-triggered
	// agent run has no user identity to wrap synthetic claims around,
	// so its reservation reads/writes route through the admin pool
	// here. Manual runs go through SyntheticClaimsWithTx + the
	// non-System methods.
	InsertSystem(ctx context.Context, orgID string, w domain.ConversationWorktree) (inserted bool, winningPath string, err error)
	GetByRepoRefSystem(ctx context.Context, orgID, conversationID, repoID, ref string) (*domain.ConversationWorktree, error)
	DeleteByRepoRefSystem(ctx context.Context, orgID, conversationID, repoID, ref string) error
}
