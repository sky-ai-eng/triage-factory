// One-time move of the workspace snapshot blobs onto the task key.
//
// A workspace — the run tree, the snapshot blob, and the workspace_snapshots
// row — is keyed by the task now, so a second delegation on a task continues in
// the first one's tree instead of cloning fresh. The row was re-keyed by a
// migration, which can rewrite a column; the blob is an object in a store no
// migration can reach, so it is moved here, at boot, beside the worktree sweep.
// Local mode only: multi is unreleased, so it has no deployed blobs to move.

package delegate

import (
	"context"
	"path"
	"sort"
	"strings"
)

// RekeyWorkspaceBlobsToTask renames each task's newest blueprint run's snapshot
// blob to the task key and drops the other runs' blobs.
//
// Which run wins is the same choice the migration made — the task's most
// recently started run — so the row and the blob describe one workspace. A
// loser's blob is deleted rather than kept: its lifecycle row is gone, and a
// blob with no row is exactly the "written, pointing at nothing" reading the
// record exists to rule out.
//
// Idempotent in both directions. A blob already at a task key names no
// blueprint run, so it is left alone; a run whose blob moved on an earlier boot
// is not there to move again; and a move interrupted between the copy and the
// delete finishes on the next boot, where the destination already exists and
// only the source is dropped.
//
// Best-effort throughout: every failure is logged and the sweep carries on. A
// blob it could not move costs one cold resume that rebuilds from nothing,
// where failing boot over it would cost the whole install.
func (s *Spawner) RekeyWorkspaceBlobsToTask(ctx context.Context, orgID string) {
	blobs := s.Storage()
	if blobs == nil || s.blueprints == nil {
		return
	}
	objects, err := blobs.List(ctx, orgID+"/")
	if err != nil {
		delegateLog.Warn("list workspace snapshot blobs failed; run-keyed blobs stay where they are and a resume will rebuild from nothing", "error", err)
		return
	}

	// The run-keyed blobs, grouped by the task their run belongs to. A key that
	// resolves to no blueprint run is already a task key (or names a run that
	// has since been purged), and neither is this sweep's to touch.
	type candidate struct {
		key       string // the blob's own storage key
		runID     string
		startedAt int64
	}
	byTask := map[string][]candidate{}
	for _, obj := range objects {
		keyID, ok := snapshotKeyID(orgID, obj.Key)
		if !ok {
			continue
		}
		br, err := s.blueprints.GetRunSystem(ctx, orgID, keyID)
		if err != nil {
			delegateLog.Warn("resolve the blueprint run behind a workspace snapshot blob failed; leaving it at its old key",
				"key", obj.Key, "error", err)
			continue
		}
		if br == nil || br.TaskID == "" {
			continue
		}
		byTask[br.TaskID] = append(byTask[br.TaskID], candidate{key: obj.Key, runID: br.ID, startedAt: br.StartedAt.UnixNano()})
	}

	movedCount, droppedCount := 0, 0
	for taskID, runs := range byTask {
		// Newest first, ties broken on the run id, so this sweep and the
		// migration's data step pick the same survivor.
		sort.Slice(runs, func(i, j int) bool {
			if runs[i].startedAt != runs[j].startedAt {
				return runs[i].startedAt > runs[j].startedAt
			}
			return runs[i].runID > runs[j].runID
		})
		dest := snapshotKey(orgID, taskID)
		for i, run := range runs {
			if i == 0 {
				if s.moveWorkspaceBlob(ctx, run.key, dest) {
					movedCount++
				}
				continue
			}
			if err := blobs.Delete(ctx, run.key); err != nil {
				delegateLog.Warn("drop a superseded run's workspace snapshot blob failed", "key", run.key, "error", err)
				continue
			}
			droppedCount++
		}
	}
	if movedCount > 0 || droppedCount > 0 {
		delegateLog.Info("workspace snapshot blobs re-keyed by task", "moved", movedCount, "superseded_dropped", droppedCount)
	}
}

// moveWorkspaceBlob copies src to dest and deletes src, reporting whether the
// blob ended up at dest. A dest that already holds a blob is the finished half
// of an interrupted earlier move, so the copy is skipped and only the source is
// dropped — overwriting would put the older copy back over the newer one.
func (s *Spawner) moveWorkspaceBlob(ctx context.Context, src, dest string) bool {
	if src == dest {
		return false
	}
	blobs := s.Storage()
	exists, err := blobs.Exists(ctx, dest)
	if err != nil {
		delegateLog.Warn("check the task-keyed workspace snapshot before moving failed; leaving the blob at its old key",
			"src", src, "dest", dest, "error", err)
		return false
	}
	if !exists {
		rc, err := blobs.Get(ctx, src)
		if err != nil {
			delegateLog.Warn("read a run-keyed workspace snapshot failed; leaving it where it is", "src", src, "error", err)
			return false
		}
		putErr := blobs.Put(ctx, dest, rc)
		_ = rc.Close()
		if putErr != nil {
			delegateLog.Warn("write a workspace snapshot at its task key failed; leaving the blob at its old key",
				"src", src, "dest", dest, "error", putErr)
			return false
		}
	}
	if err := blobs.Delete(ctx, src); err != nil {
		// The blob is at the task key, which is what a resume reads, so the
		// leftover is wasted disk rather than a wrong answer — and the next
		// boot takes the dest-exists arm above and drops it.
		delegateLog.Warn("drop a run-keyed workspace snapshot after moving it to the task key failed", "src", src, "error", err)
	}
	return true
}

// snapshotKeyID is snapshotKey read backwards: it pulls the key id out of
// "<orgID>/<keyID>/workspace.tar" and reports whether the key had that shape.
// Anything else under the org prefix belongs to another consumer of the blob
// store and is none of this sweep's business.
func snapshotKeyID(orgID, key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, orgID+"/")
	if !ok {
		return "", false
	}
	keyID, leaf := path.Split(rest)
	keyID = strings.TrimSuffix(keyID, "/")
	if keyID == "" || strings.Contains(keyID, "/") || leaf != snapshotBlobLeaf {
		return "", false
	}
	return keyID, true
}
