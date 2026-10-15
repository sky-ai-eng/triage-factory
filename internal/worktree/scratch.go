package worktree

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// KnowledgeDir is the _tfac subdirectory a run's team knowledge is staged
// under. It is named here because the snapshot's scratch walk has to skip it,
// and the walk now lives beside the capture that runs it.
const KnowledgeDir = "knowledge"

// scratchExcludes are the top-level _tfac entries that come back by a route of
// their own and so never ride in a snapshot: entity-memory rebuilds from
// conversation_memory (and under a jail is not even a directory — it is the
// symlink standing in for the read-only mount, which the walk skips as
// non-regular regardless), knowledge is re-copied from the team knowledge
// store on every launch (carrying it would preserve a stale copy at the cost
// of the blob's size), and ci-logs is the extracted output of `exec gh
// actions download-logs`, which the agent re-runs to get byte-identical
// content back from GitHub. Everything else under _tfac (skill scratch,
// ad-hoc agent files, and the agent's own memory.md — which is not in the DB
// until termination ingests it) is non-recoverable and IS captured.
//
// ci-logs is the only one of the three the agent can notice missing:
// entity-memory and knowledge are both re-staged before it looks, while a full
// Actions log archive — routinely hundreds of MB to GBs of text, and the
// largest thing a park would ever compress — is re-fetched on demand rather
// than restored. So a cold rehydrate whose snapshot dropped a populated ci-logs
// plants a notice where the logs were, rather than handing back a tree that
// quietly lost them.
var scratchExcludes = map[string]bool{
	EntityMemoryDir: true,
	KnowledgeDir:    true,
	CILogsDir:       true,
}

// WalkScratch visits every regular file under wtPath/_tfac that a snapshot
// carries, in lexical order, skipping the scratchExcludes subtrees — the one
// definition of "the scratch a snapshot carries" that the archive and the
// fingerprint both read. A missing _tfac visits nothing.
//
// It reports whether a ci-logs subtree with something in it was skipped. An
// entry that vanishes between the listing and the visit is skipped: a
// checkpoint walks a tree a background process may still be changing, and an
// entry removed a moment later is the same torn capture as one removed a
// moment earlier. ctx is checked per entry.
//
// An entry this process may not read is skipped too, and named in unreadable
// — a directory it cannot list, or a file a visit could not open. A snapshot
// short of that file still carries the rest of the work; a walk that failed on
// it would carry none.
func WalkScratch(ctx context.Context, wtPath string, visit func(rel, path string, fi os.FileInfo) error) (omittedCILogs bool, unreadable []string, err error) {
	root := filepath.Join(wtPath, ScratchDir)
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return false, nil, nil
	}
	err = filepath.Walk(root, func(path string, fi os.FileInfo, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if walkErr != nil {
			if errors.Is(walkErr, fs.ErrNotExist) {
				return nil
			}
			if errors.Is(walkErr, fs.ErrPermission) {
				unreadable = append(unreadable, filepath.ToSlash(rel))
				return nil
			}
			return walkErr
		}
		if rel == "." {
			return nil
		}
		top := rel
		if i := strings.IndexRune(rel, filepath.Separator); i >= 0 {
			top = rel[:i]
		}
		if scratchExcludes[top] {
			if fi.IsDir() {
				if rel == CILogsDir && dirHasEntry(path) {
					omittedCILogs = true
				}
				return filepath.SkipDir
			}
			return nil
		}
		if !fi.Mode().IsRegular() {
			return nil // directories implied by their files; skip symlinks/etc.
		}
		err = visit(filepath.ToSlash(rel), path, fi)
		if errors.Is(err, fs.ErrPermission) {
			unreadable = append(unreadable, filepath.ToSlash(rel))
			return nil
		}
		return err
	})
	return omittedCILogs, unreadable, err
}

// dirHasEntry reports whether path holds at least one entry, reading only far
// enough to answer rather than listing a directory the walk is about to skip.
// A read failure answers false: this decides whether to explain an absence, and
// a directory that cannot be read is not evidence that anything was dropped.
func dirHasEntry(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(1)
	return err == nil && len(names) > 0
}

// ScratchHasCILogs reports whether wtPath's ci-logs directory holds anything,
// which is what the manifest's CILogsOmitted says of a tree the walk has not
// run over.
func ScratchHasCILogs(wtPath string) bool {
	return dirHasEntry(filepath.Join(wtPath, ScratchDir, CILogsDir))
}
