package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// EntityMemoryDir is the scratch-relative directory a run reads its prior
// memory from. Exported because two packages must agree on it byte for byte:
// the delegate materializes into it, and the link below stands in for it under
// a jail. A prompt names this path to the agent, so it is a contract, not an
// implementation detail.
const EntityMemoryDir = "entity-memory"

// memoryLinkRel is the path, relative to a run tree, where the agent's prompt
// tells it prior memory lives.
var memoryLinkRel = filepath.Join(ScratchDir, EntityMemoryDir)

// EnsureSandboxMemoryLink plants `<dir>/_tfac/entity-memory` as a symlink to the
// jail's read-only memory mount point, so a sandboxed agent finds its
// predecessors' handoff at the path its prompt names without TF ever writing
// that tree into the workspace.
//
// Called while the tree is orchestrator-owned — strictly before the launch-time
// chown hands it to the per-run sandbox uid. After that hand-off the
// capability-less orchestrator cannot write inside the tree at all, which is
// exactly why the memory tree itself no longer lives there: planting once, up
// front, is what lets every later step mount a freshly materialized tree with
// zero writes into the workspace.
//
// Force-replaces whatever occupies the path (a real directory a tree built
// before the mount existed carried, a stale symlink), and no-ops when the
// correct symlink is already there — so a warm tree reused across steps takes no
// write.
//
// The one occupant it will not replace is repo content. A run tree can BE a repo
// checkout, git excludes do nothing for an already-tracked path, and replacing a
// tracked directory with a symlink would ride the agent's next `git add -A`
// straight into its PR. Refusing costs this run its prior memory, which is worth
// less than the user's committed files; the error is the diagnostic.
//
// No-op unless this host actually sandboxes: local mode materializes real files
// there (it owns the tree, and released local behavior is deliberately
// untouched), and there is no mount to point at.
//
// An agent that later deletes or replaces the symlink only blinds itself to its
// own handoff, and cannot affect mount placement — that is fixed at spec time
// regardless of anything in the tree.
func EnsureSandboxMemoryLink(ctx context.Context, dir string) error {
	if dir == "" || !agentproc.WillSandbox() {
		return nil
	}
	link := filepath.Join(dir, memoryLinkRel)
	if fi, err := os.Lstat(link); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			if target, rerr := os.Readlink(link); rerr == nil && target == sandbox.TrustedMemoryDestination {
				return nil // already correct — take no write on a warm tree
			}
		}
		if tracked := TrackedUnder(ctx, dir, memoryLinkRel); len(tracked) > 0 {
			return fmt.Errorf("repo tracks %d file(s) under %s; leaving them in place", len(tracked), memoryLinkRel)
		}
		if err := os.RemoveAll(link); err != nil {
			return fmt.Errorf("clear %s: %w", memoryLinkRel, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", memoryLinkRel, err)
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return fmt.Errorf("create scratch dir: %w", err)
	}
	if err := os.Symlink(sandbox.TrustedMemoryDestination, link); err != nil {
		return fmt.Errorf("link %s: %w", memoryLinkRel, err)
	}
	return nil
}
