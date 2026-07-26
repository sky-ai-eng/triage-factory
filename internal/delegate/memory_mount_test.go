package delegate

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// sandboxingMode puts the process in the mode that actually jails agents, so the
// staging branch is live. Skips off Linux, where WillSandbox is false regardless.
func sandboxingMode(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox path (and therefore the memory mount) is Linux-only")
	}
	paths.SetForTest(t, t.TempDir())
	runmode.SetForTest(t, runmode.ModeMulti)
}

// TestEntityMemoryTarget_LocalRendersInTheTree: local mode owns the run tree, so
// the layout stays exactly where released behavior puts it, no mount is
// requested, and the repo-owned guard still applies — that tree may be a repo
// checkout.
func TestEntityMemoryTarget_LocalRendersInTheTree(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	runmode.SetForTest(t, runmode.ModeLocal)

	cwd := t.TempDir()
	owned := repoFiles{"_tfac/memory.md": true}
	var cfg runConfig

	dir, gotOwned := entityMemoryTarget(&cfg, "run-local", cwd, owned)

	if want := localMemoryRoot(cwd); dir != want {
		t.Errorf("target = %q, want the in-tree layout at %q", dir, want)
	}
	if cfg.memorySourcePath != "" {
		t.Errorf("memorySourcePath = %q, want empty (local mode mounts nothing)", cfg.memorySourcePath)
	}
	if !gotOwned.owns("memory.md") {
		t.Error("the repo-owned set must travel with an in-tree target")
	}
}

// TestBlueprintHandoff_WarmStepReadsItsPredecessorFromTheMount is the whole
// ticket, end to end and in order: step 1 writes the one path it is told about
// and terminates, its narrative is ingested, and step 2 — starting in the SAME
// tree, which by then belongs to the sandbox uid — gets that narrative rendered
// into the dir its launch will mount, numbered and named for the step that
// produced it. Before the mount this step read an empty directory.
//
// The tree is made unwritable to prove the "no write into a handed-off tree"
// half rather than assert it: a materializer that still reached inside would
// fail here, which is exactly what the old pre-launch pass did.
func TestBlueprintHandoff_WarmStepReadsItsPredecessorFromTheMount(t *testing.T) {
	sandboxingMode(t)
	s, database, runID, taskID := setupAdvanceFixture(t, "warm-handoff")
	makeRunBlueprintStep(t, database, runID, taskID)
	task := loadTask(t, s, taskID)
	blueprintRunID := "bpr-" + runID
	cwd := t.TempDir()

	// Step 1 writes the fixed path and terminates.
	writeAgentMemory(t, cwd, "step 1 chose approach X because Y")
	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, blueprintRunID, task,
		res(`{"outcome":"continue","summary":"did step work"}`), cwd, nil, "", "event", "")

	// The launch handed the tree to the sandbox uid; nothing TF-side can write
	// under it now, memory.md included.
	for _, dir := range []string{filepath.Join(cwd, "_tfac"), cwd} {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("chmod %s: %v", dir, err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}

	// Step 2's pre-launch pass, in the order runAgent performs it.
	const step2RunID = "step2-run"
	var cfg runConfig
	dir, gotOwned := entityMemoryTarget(&cfg, step2RunID, cwd, nil)
	t.Cleanup(func() { removeStagedMemory(dir) })

	if want := sandbox.TrustedMemorySourcePath(step2RunID); dir != want {
		t.Fatalf("target = %q, want this launch's own staging dir %q", dir, want)
	}
	if cfg.memorySourcePath != dir {
		t.Errorf("memorySourcePath = %q, want %q — without it the launch mounts nothing", cfg.memorySourcePath, dir)
	}
	if gotOwned != nil {
		t.Error("a staging dir is no repo; it must carry no repo-owned set")
	}

	materializePriorMemories(s.taskMemory, runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, dir, task.EntityID, blueprintRunID, gotOwned)

	// Step 1's handoff is in the tree step 2 will mount...
	assertMemoryFile(t, filepath.Join(dir, "this-run", "01-t.md"), "step 1 chose approach X because Y")
	// ...and the run tree took no write at all.
	if _, err := os.Lstat(filepath.Join(cwd, "_tfac", "entity-memory")); !os.IsNotExist(err) {
		t.Errorf("staging wrote into the run tree (err=%v); a handed-off tree cannot accept writes", err)
	}
}

// TestStagedEntityMemorySource_ResumeReusesWhatTheClaimStaged: a resume continues
// the same conversation, so it re-mounts the tree its original claim rendered
// rather than rendering a new one — and degrades to no mount when the dir is
// gone (a cold resume on another executor, or after the startup sweep).
func TestStagedEntityMemorySource_ResumeReusesWhatTheClaimStaged(t *testing.T) {
	sandboxingMode(t)

	const runID = "resumed-run"
	if got := stagedEntityMemorySource(runID); got != "" {
		t.Errorf("stagedEntityMemorySource with nothing on disk = %q, want empty", got)
	}

	dir := sandbox.TrustedMemorySourcePath(runID)
	if err := os.MkdirAll(filepath.Join(dir, "this-run"), 0o755); err != nil {
		t.Fatalf("stage: %v", err)
	}
	t.Cleanup(func() { removeStagedMemory(dir) })

	if got := stagedEntityMemorySource(runID); got != dir {
		t.Errorf("stagedEntityMemorySource = %q, want %q", got, dir)
	}

	removeStagedMemory(dir)
	if got := stagedEntityMemorySource(runID); got != "" {
		t.Errorf("after reclaim = %q, want empty (a swept dir means no mount, not a failed launch)", got)
	}
	// Reclaiming twice is how every terminal path behaves — the step's own defer
	// and the blueprint-wide sweep both run.
	removeStagedMemory(dir)
}

// TestProcessCompletion_RefusesThePredecessorsMemory is the attribution half of
// the warm-step fix, and it is not solved by the mount: the agent writes
// _tfac/memory.md INSIDE the tree, so at a warm step boundary the orchestrator
// cannot delete what the previous step left there. A step that terminates having
// written nothing must still record agent_content NULL — "this run terminated"
// and "this run wrote it" are different facts, and the factory's memory_missing
// derivation reads the second.
func TestProcessCompletion_RefusesThePredecessorsMemory(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "stale-memory")
	makeRunBlueprintStep(t, database, runID, taskID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()
	blueprintRunID := "bpr-" + runID

	// The previous step's file, still at the fixed write path because nothing in
	// a handed-off tree could remove it.
	writeAgentMemory(t, cwd, "the PREVIOUS step's narrative")
	inherited := fingerprintAgentMemoryFile(cwd)
	if inherited == nil {
		t.Fatal("fingerprint of an existing memory file = nil")
	}

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, blueprintRunID, task,
		res(`{"outcome":"continue","summary":"did step work"}`), cwd, inherited, "", "event", "")

	if got := memoryContentFor(t, s, task.EntityID, runID); got != "" {
		t.Errorf("agent_content = %q, want empty — this step wrote nothing, so its predecessor's narrative must not be adopted", got)
	}
}

// TestProcessCompletion_IngestsWhatThisStepWrote is the positive control for the
// test above: the same inherited file, overwritten by the agent, IS this run's
// work and must be ingested. Without this the fingerprint could pass by refusing
// everything.
func TestProcessCompletion_IngestsWhatThisStepWrote(t *testing.T) {
	s, database, runID, taskID := setupAdvanceFixture(t, "fresh-memory")
	makeRunBlueprintStep(t, database, runID, taskID)
	task := loadTask(t, s, taskID)
	cwd := t.TempDir()
	blueprintRunID := "bpr-" + runID

	writeAgentMemory(t, cwd, "the PREVIOUS step's narrative")
	inherited := fingerprintAgentMemoryFile(cwd)
	writeAgentMemory(t, cwd, "what THIS step did")

	s.processCompletion(context.Background(), runmode.LocalDefaultOrgID, runID, blueprintRunID, task,
		res(`{"outcome":"continue","summary":"did step work"}`), cwd, inherited, "", "event", "")

	if got := memoryContentFor(t, s, task.EntityID, runID); got != "what THIS step did" {
		t.Errorf("agent_content = %q, want this step's own narrative", got)
	}
}

// TestRehydrate_RestoredTreeCarriesTheMemorySymlink is the cold-resume half. A
// rehydrated tree is the last moment it is orchestrator-owned, so the link must
// be planted there or the resumed run's mount lands somewhere nothing looks.
//
// It also pins the ORDERING: the scratch install is a wholesale rename onto
// _tfac, which fails outright if the link's parent already exists — so planting
// must come after, and a tree whose snapshot carried a real scratch dir must
// still come back with a symlink, not a directory.
func TestRehydrate_RestoredTreeCarriesTheMemorySymlink(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the sandbox path (and therefore the memory mount) is Linux-only")
	}
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	// Capture side runs in local mode: multi resolves the blob store to an object
	// store this test has no business standing up, and takes its git delta through
	// a dropped-privilege child. The blob is the same either way. The mode flips
	// to the one that actually jails agents before the rehydrate, which is what
	// the planting gate under test reads.
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newStorageSpawner(t)

	const runID = "wt-memlink"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	// Scratch content that DOES ride the snapshot, so the rehydrate takes the
	// rename branch — the one that collides with an already-planted link.
	writeFile(t, filepath.Join(wtPath, "_tfac", "ci-logs", "build.log"), "ci log line")
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, ""); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Host loss: the tree is gone and comes back from bare + delta.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	gitT(t, bareDir, "worktree", "prune")

	runmode.SetForTest(t, runmode.ModeMulti)
	run := &domain.Conversation{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, owner, repo, "")
	if err != nil {
		t.Fatalf("ensureWorkspace (cold): %v", err)
	}

	assertFileContains(t, filepath.Join(got, "_tfac", "ci-logs", "build.log"), "ci log line")
	link := filepath.Join(got, "_tfac", "entity-memory")
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat %s: %v", link, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s came back as %v, want the symlink standing in for the mount", link, fi.Mode())
	}
	if target, _ := os.Readlink(link); target != sandbox.TrustedMemoryDestination {
		t.Errorf("%s -> %q, want %q", link, target, sandbox.TrustedMemoryDestination)
	}
}

// TestMemoryFingerprint_TracksBothSizeAndMtime pins what "the agent never
// touched it" means. Size alone would miss a same-length rewrite; mtime alone
// would miss a filesystem that rounds it. Both must be consulted, and a nil
// fingerprint (no inherited file, or a resume) must never claim a match — that
// would refuse every run's own memory.
func TestMemoryFingerprint_TracksBothSizeAndMtime(t *testing.T) {
	cwd := t.TempDir()

	var absent *memoryFingerprint
	if absent.matches(cwd) {
		t.Error("a nil fingerprint claimed a match; every run's own memory would be refused")
	}

	writeAgentMemory(t, cwd, "the previous step's narrative")
	fp := fingerprintAgentMemoryFile(cwd)
	if fp == nil {
		t.Fatal("fingerprint of an existing file = nil")
	}
	if !fp.matches(cwd) {
		t.Error("an untouched file did not match its own fingerprint")
	}

	// Same length, rewritten: only the timestamp distinguishes them, so this is
	// the case size alone would get wrong. Stamped explicitly rather than raced
	// against the clock's resolution.
	path := agentMemoryFilePath(cwd)
	writeFileT(t, path, "the CURRENT step's narrativ!")
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime().Add(time.Second)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if fp.matches(cwd) {
		t.Error("a rewritten file matched the inherited fingerprint; this step's work would be dropped")
	}

	// Restored timestamp, different length: the case mtime alone would get wrong.
	writeFileT(t, path, "much longer content than the file this run inherited")
	if err := os.Chtimes(path, fp.modTime, fp.modTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if fp.matches(cwd) {
		t.Error("a resized file matched the inherited fingerprint")
	}

	// A file that is gone is not the inherited one either.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if fp.matches(cwd) {
		t.Error("a missing file matched the inherited fingerprint")
	}
	if fingerprintAgentMemoryFile(cwd) != nil {
		t.Error("fingerprint of a missing file is not nil")
	}
}

// memoryContentFor returns the ingested agent_content for one run's memory row,
// or "" when the row carries none (the NULL case).
func memoryContentFor(t *testing.T, s *Spawner, entityID, runID string) string {
	t.Helper()
	mems, err := s.taskMemory.GetMemoriesForEntitySystem(context.Background(), runmode.LocalDefaultOrgID, entityID, runmode.LocalDefaultTeamID)
	if err != nil {
		t.Fatalf("GetMemoriesForEntitySystem: %v", err)
	}
	for _, m := range mems {
		if m.RunID == runID {
			return m.Content
		}
	}
	return ""
}
