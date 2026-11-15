// Team knowledge staged into a run's tree: the task team's own knowledge base
// plus every other team's published root, copied in before the agent starts.
// The framework blocks say where the tree is and tell the agent to list it, so
// nothing per run describes what arrived.
//
// Which knowledge a run gets is a pure function of the task's team id. There is
// no model in the path, nothing to wait on, and no cross-pod call: the team is
// a column the task already carries, and the two roots under it are the whole
// visibility model.

package delegate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/kbstore"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// The knowledge tree the agent reads, inside the run's scratch dir:
//
//	_tfac/knowledge/team/private/…    the task team's own notes
//	_tfac/knowledge/team/shared/…     what the task team has published
//	_tfac/knowledge/org/<team>/…      what every OTHER team has published
//
// The task team's two roots keep their names because the difference is real to
// the agent's readers: what it learns from `shared/` is knowledge the whole org
// can see, and what it learns from `private/` is not. Other teams contribute
// only their published root, so there is no root level under org/ to name —
// the team is the distinction there.
const (
	knowledgeDirName     = worktree.KnowledgeDir
	knowledgeTeamDirName = "team"
	knowledgeOrgDirName  = "org"

	// knowledgeWarnBytes is the soft cap on one run's staged knowledge. Over it
	// the run still gets everything — trimming an org's knowledge is a human
	// decision, not one a copy step makes silently — but the log says so, since
	// past this point the copy is a real cost on every claim and most of it is
	// unlikely to be read.
	knowledgeWarnBytes = 2 * 1024 * 1024
)

// stagedKnowledge counts what one launch copied in, for the log.
type stagedKnowledge struct {
	files int
	bytes int64
}

// stageTeamKnowledge copies this run's knowledge into the run tree.
//
// teamID is the task's team. It resolves the whole answer: that team's KB in
// full, and every other active team's published root. A task with no team (or
// a spawner with no KB seam wired) stages nothing, which is the same tree an
// org with no knowledge produces.
//
// Every launch REBUILDS the tree rather than adding to it: whatever a previous
// launch into this run root staged is cleared first, so the tree is what the
// knowledge base holds now and nothing else.
//
// Advisory throughout, like the prior-memory materializer beside it: a store
// failure, a mkdir failure or a per-file copy failure is logged and skipped.
// An agent running without staged knowledge is still useful; a run that failed
// because someone's runbook was unreadable is not.
func (s *Spawner) stageTeamKnowledge(ctx context.Context, orgID, teamID, cwd string) {
	kb := s.TeamKB()
	if kb == nil || teamID == "" {
		return
	}
	root := filepath.Join(cwd, scratchDirName, knowledgeDirName)
	// A full rebuild, not an overlay. See clearStagedKnowledge for why the
	// difference is load-bearing rather than tidiness.
	clearStagedKnowledge(root)

	staged := stagedKnowledge{}
	s.stageOneKnowledgeSet(ctx, kb, orgID, teamID, kbstore.Roots(), root, knowledgeTeamDirName, &staged)

	for _, other := range s.otherTeamsWithKnowledge(ctx, orgID, teamID) {
		dir := path.Join(knowledgeOrgDirName, other.Slug)
		s.stageOneKnowledgeSet(ctx, kb, orgID, other.ID, []kbstore.Root{kbstore.RootShared}, root, dir, &staged)
	}

	if staged.files == 0 {
		return
	}
	if staged.bytes > knowledgeWarnBytes {
		delegateLog.Warn("staged team knowledge is over the soft cap; consider trimming what the org publishes",
			"team", teamID, "files", staged.files, "bytes", staged.bytes, "soft_cap", knowledgeWarnBytes)
	}
	delegateLog.Info("staged team knowledge for run", "team", teamID, "files", staged.files, "bytes", staged.bytes)
}

// clearStagedKnowledge empties the knowledge tree so this launch can rebuild
// it from the store. Three things make that a correctness step rather than
// housekeeping, and the second is the sharp one:
//
//   - A document deleted from the knowledge base would otherwise stay readable,
//     and the framework block tells the agent to list the tree and read what
//     it finds there.
//   - A document PUBLISHED between two launches would appear under both roots
//     at once: the old copy under `team/private/`, the new one under
//     `team/shared/`. The root a document sits under is a claim about who else
//     can see it, so a tree carrying both teaches the agent something false
//     about material it may go on to quote in a pull request.
//   - Anything the agent itself left here is discarded, which is what the
//     framework block promises. Without the clear that promise held only for
//     the first launch into a run root.
//
// A blueprint's steps share one run tree, so this fires between them; a warm
// step whose tree already belongs to the sandbox identity never reaches here at
// all (the caller skips staging entirely, and the first step's copy stands).
//
// Best-effort: a tree that cannot be removed is logged and left, which is the
// state a launch that skipped this would have had anyway.
func clearStagedKnowledge(root string) {
	if err := os.RemoveAll(root); err != nil {
		delegateLog.Warn("clear staged knowledge failed; the agent may read stale copies", "path", root, "error", err)
	}
}

// stageOneKnowledgeSet copies one team's roots into dir (relative to the
// knowledge root) and counts what landed. Entries are staged under the same
// relative shape they have in the KB, so a folder a team filed knowledge into
// is a folder the agent walks.
func (s *Spawner) stageOneKnowledgeSet(
	ctx context.Context,
	kb kbstore.KB,
	orgID, teamID string,
	roots []kbstore.Root,
	knowledgeRoot, dir string,
	staged *stagedKnowledge,
) {
	entries, err := kb.List(ctx, orgID, teamID, kbstore.ListQuery{Roots: roots})
	if err != nil {
		delegateLog.Warn("list team knowledge failed; this run stages none of it", "team", teamID, "error", err)
		return
	}
	for _, e := range entries {
		// Under the team's own directory the two roots are named; a
		// contributing team's set is all published, so its entries sit
		// directly under the team's directory with no redundant level.
		rel := e.Path
		if len(roots) > 1 {
			rel = path.Join(string(e.Root), e.Path)
		}
		dst := filepath.Join(knowledgeRoot, filepath.FromSlash(dir), filepath.FromSlash(rel))
		n, err := s.copyKnowledgeFile(ctx, kb, orgID, teamID, e.Ref(), dst)
		if err != nil {
			delegateLog.Warn("stage knowledge file failed", "team", teamID, "ref", e.Ref().String(), "error", err)
			continue
		}
		staged.files++
		staged.bytes += n
	}
}

// copyKnowledgeFile streams one KB entry to dst, returning the bytes written.
func (s *Spawner) copyKnowledgeFile(ctx context.Context, kb kbstore.KB, orgID, teamID string, ref kbstore.Ref, dst string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, fmt.Errorf("create knowledge dir: %w", err)
	}
	src, err := kb.Open(ctx, orgID, teamID, ref)
	if err != nil {
		return 0, err
	}
	defer func() { _ = src.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, fmt.Errorf("create knowledge file: %w", err)
	}
	n, copyErr := io.Copy(out, src)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		// A failed copy leaves a truncated file behind, and the caller's next
		// move is to log this entry and skip it — which would leave half a
		// document in a tree the agent is told to list and read. Half a
		// runbook read as a whole one is worse than an absent one, so the
		// fragment goes.
		if rmErr := os.Remove(dst); rmErr != nil && !os.IsNotExist(rmErr) {
			delegateLog.Warn("remove partially staged knowledge file failed; the agent may read a truncated document",
				"path", dst, "error", rmErr)
		}
		if copyErr != nil {
			return n, copyErr
		}
		return n, closeErr
	}
	return n, nil
}

// otherTeamsWithKnowledge lists the org's other active teams, whose published
// roots this run is staged with. Archived teams are excluded: archiving winds a
// team down, and a run's context should reflect the org as it is rather than
// growing with its history. Their knowledge is still readable by a person on
// the Knowledge page — this is about what a run is handed, not about what
// exists.
//
// A team whose slug is not a single usable directory name falls back to its id,
// so the staged tree can never gain a level or escape its root because of what
// somebody named their team.
func (s *Spawner) otherTeamsWithKnowledge(ctx context.Context, orgID, teamID string) []domain.Team {
	if s.teams == nil {
		return nil
	}
	all, err := s.teams.ListActiveForOrgSystem(ctx, orgID)
	if err != nil {
		delegateLog.Warn("list org teams for knowledge staging failed; this run stages only its own team's knowledge",
			"org", orgID, "error", err)
		return nil
	}
	out := make([]domain.Team, 0, len(all))
	for _, t := range all {
		if t.ID == teamID {
			continue
		}
		if kbstore.ValidatePath(t.Slug) != nil || strings.Contains(t.Slug, "/") {
			t.Slug = t.ID
		}
		out = append(out, t)
	}
	return out
}
