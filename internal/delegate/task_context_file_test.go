package delegate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWriteTaskContextFile_LandsInTheStagedTarget is the task context's whole
// retention, and it has to land where entityMemoryTarget put the prior
// memories: nothing is pinned through a compaction, so the row carrying these
// bytes is summarized like any other, and this file is the only way back to the
// PR number the summary dropped.
//
// The directory matters as much as the name. On a warm, handed-off step the run
// tree belongs to the sandbox identity and TF may not write into it; the staged
// memory dir is the one per-launch location it still owns, which is why the file
// sits one level inside the memory tree rather than beside it in _tfac/.
func TestWriteTaskContextFile_LandsInTheStagedTarget(t *testing.T) {
	staged := filepath.Join(t.TempDir(), "staged-memory")
	const rendered = "<task_context>\nPull request: #18\nTitle: fix the flaky check\n</task_context>"

	writeTaskContextFile(staged, rendered, nil)

	got, err := os.ReadFile(filepath.Join(staged, taskContextFileName))
	if err != nil {
		t.Fatalf("the task context was not retained: %v", err)
	}
	if strings.TrimRight(string(got), "\n") != rendered {
		t.Errorf("retained file = %q, want the rendered block verbatim", got)
	}
}

// TestWriteTaskContextFile_SkipsWhatItMustNot covers the three cases where
// writing is the wrong answer. A run with no task context has nothing to
// retain; a repo that tracks the path owns it, and a file the agent would
// commit is worth more than a re-read; and a launch that staged nowhere has
// nowhere to put it.
func TestWriteTaskContextFile_SkipsWhatItMustNot(t *testing.T) {
	t.Run("no task context", func(t *testing.T) {
		root := t.TempDir()
		writeTaskContextFile(root, "   \n", nil)
		if _, err := os.Stat(filepath.Join(root, taskContextFileName)); !os.IsNotExist(err) {
			t.Error("an empty task context still wrote a file")
		}
	})

	t.Run("repo owns the path", func(t *testing.T) {
		root := t.TempDir()
		owned := repoFiles{filepath.Join(scratchDirName, entityMemoryDirName, taskContextFileName): true}
		writeTaskContextFile(root, "<task_context>\nx\n</task_context>", owned)
		if _, err := os.Stat(filepath.Join(root, taskContextFileName)); !os.IsNotExist(err) {
			t.Error("a repo-tracked path was overwritten")
		}
	})

	t.Run("no staged target", func(t *testing.T) {
		writeTaskContextFile("", "<task_context>\nx\n</task_context>", nil)
	})
}

// TestWriteTaskContextFile_OverwritesThePriorStep keeps a blueprint's later step
// from re-reading the step before it. The steps of one run share a tree, so the
// file is per launch, not per conversation — a stale one would be a summary's
// authority on a pull request that has moved since.
func TestWriteTaskContextFile_OverwritesThePriorStep(t *testing.T) {
	root := t.TempDir()
	writeTaskContextFile(root, "<task_context>\nstep 0\n</task_context>", nil)
	writeTaskContextFile(root, "<task_context>\nstep 1\n</task_context>", nil)

	got, err := os.ReadFile(filepath.Join(root, taskContextFileName))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(got), "step 0") {
		t.Errorf("the earlier step's task context survived: %q", got)
	}
}
