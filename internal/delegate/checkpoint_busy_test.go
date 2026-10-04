package delegate

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestCheckpoint_ASlowUploadIsOneBusySkip: every batch boundary that comes
// while one upload is still running finds the engagement busy, and they are
// all the same skip. One slow upload counts once, however many boundaries
// the agent reaches behind it; a checkpoint that finds the engagement busy
// after that upload has finished is a skip of its own.
func TestCheckpoint_ASlowUploadIsOneBusySkip(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-busy-once")
	held := &heldPutStorage{Storage: f.s.Storage(), entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.s.SetStorage(held)
	c := f.start()
	defer c.stop()

	f.due(c)
	c.afterToolBatch(context.Background(), 5)
	waitFor(t, held.entered, "the first checkpoint's upload")

	for i := 0; i < 4; i++ {
		f.due(c)
		c.afterToolBatch(context.Background(), float64(9+i))
	}
	if got := f.outcomes(checkpointSkippedBusy); got != 1 {
		t.Fatalf("busy skips behind one slow upload = %d, want 1", got)
	}

	close(held.release)
	c.wg.Wait()

	// The upload is done; a checkpoint that now finds every host slot taken
	// is a new skip.
	for range checkpointHostSlots {
		if !f.s.acquireCheckpointSlot() {
			t.Fatal("could not take a host slot")
		}
	}
	defer func() {
		for range checkpointHostSlots {
			f.s.releaseCheckpointSlot()
		}
	}()
	writeFile(t, filepath.Join(f.tree, worktree.ScratchDir, "notes.txt"), "changed since")
	f.due(c)
	c.afterToolBatch(context.Background(), 20)
	f.due(c)
	c.afterToolBatch(context.Background(), 21)
	if got := f.outcomes(checkpointSkippedBusy); got != 2 {
		t.Errorf("busy skips = %d, want 2: the one behind the upload, and one for the slots that came after", got)
	}
}
