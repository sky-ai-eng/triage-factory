package delegate

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestEnsureWorkspace_WarmPath_NoRehydrate is the warm-path acceptance: a run
// that parked keeps its worktree on disk, so ensureWorkspace returns it as-is
// and does NOT rebuild from the snapshot. A marker file written AFTER the
// snapshot survives — proof the warm copy was used, not a rehydrate that would
// have lost it.
func TestEnsureWorkspace_WarmPath_NoRehydrate(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-warm"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	const sessionID = "sess-warm"
	writeSession(t, wtPath, sessionID, `{"type":"summary"}`)

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, sessionID); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Written after the snapshot: a rehydrate rebuilds from the (older) blob and
	// would lose this, so its survival distinguishes warm reuse from a rebuild.
	marker := filepath.Join(wtPath, "_tfac", "ci-logs", "warm-marker.txt")
	writeFile(t, marker, "warm")

	run := &domain.Conversation{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, gitSeed{owner: owner, repo: repo})
	if err != nil {
		t.Fatalf("ensureWorkspace (warm): %v", err)
	}
	if got != wtPath {
		t.Errorf("warm cwd = %q, want the on-disk worktree %q", got, wtPath)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("post-snapshot marker missing — ensureWorkspace rehydrated instead of reusing the warm copy: %v", err)
	}
}

// TestEnsureWorkspace_ColdPath_RehydratesFromSnapshot is the cold-path
// acceptance: a parked run whose local worktree (and session JSONL) are then
// lost — simulating host loss / a /tmp wipe — resumes by rebuilding from the
// snapshot. The agent's committed work (carried in the git bundle), the
// uncommitted changes (the patch), the ephemeral _tfac (minus the
// re-materializable subdirs), and an intact `--resume` session must all be
// restored.
func TestEnsureWorkspace_ColdPath_RehydratesFromSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-cold"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	// The agent commits work (advances the branch; rides in the bundle).
	writeFile(t, filepath.Join(wtPath, "agent.txt"), "committed by agent")
	gitT(t, wtPath, "add", "agent.txt")
	gitT(t, wtPath, "commit", "-m", "agent work")

	// ...then leaves an uncommitted edit (rides in the patch).
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\nuncommitted edit\n")

	// Ephemeral _tfac is snapshotted; entity-memory / project-knowledge are
	// excluded (they re-materialize from the DB / project KB).
	writeFile(t, filepath.Join(wtPath, "_tfac", "ci-logs", "build.log"), "ci log line")
	writeFile(t, filepath.Join(wtPath, "_tfac", "entity-memory", "ns", "x.md"), "memory")
	writeFile(t, filepath.Join(wtPath, "_tfac", "project-knowledge", "kb.md"), "kb")

	const sessionID = "sess-cold"
	sessPath := writeSession(t, wtPath, sessionID, `{"type":"summary","sid":"cold"}`)

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, sessionID); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Simulate host loss / /tmp wipe: the worktree and session JSONL are gone,
	// and the bare no longer has the agent's local branch (a fresh clone only
	// carries the remote) — so the committed state can ONLY come back via the
	// bundle. The persistent bare itself survives (state-root, not /tmp).
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	if err := os.Remove(sessPath); err != nil {
		t.Fatalf("rm session: %v", err)
	}
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	gitT(t, bareDir, "worktree", "prune")
	gitT(t, bareDir, "branch", "-D", "feature")

	run := &domain.Conversation{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, gitSeed{owner: owner, repo: repo})
	if err != nil {
		t.Fatalf("ensureWorkspace (cold): %v", err)
	}
	if got != wtPath {
		t.Errorf("rehydrated cwd = %q, want %q", got, wtPath)
	}

	assertFileContains(t, filepath.Join(got, "agent.txt"), "committed by agent") // bundle
	assertFileContains(t, filepath.Join(got, "README.md"), "uncommitted edit")   // patch
	assertFileContains(t, filepath.Join(got, "_tfac", "ci-logs", "build.log"), "ci log line")
	assertMissing(t, filepath.Join(got, "_tfac", "entity-memory", "ns", "x.md"))
	assertMissing(t, filepath.Join(got, "_tfac", "project-knowledge", "kb.md"))

	// The session transcript lands under the rebuilt cwd's encoded project dir
	// so `claude --resume` reconnects.
	sessPath2, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(got), sessionID)
	if err != nil {
		t.Fatalf("ClaudeSessionPath: %v", err)
	}
	assertFileContains(t, sessPath2, `"sid":"cold"`)
}

// TestEnsureWorkspace_ColdPath_TranscriptBearingSnapshotIsResumable: a snapshot
// that captured the session transcript rehydrates one back into place, so the
// resume guard (sessionTranscriptExists) sees the run as resumable and a
// --resume is safe. The positive half of the pair below.
func TestEnsureWorkspace_ColdPath_TranscriptBearingSnapshotIsResumable(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-has-transcript"
	// A non-git run-root keeps the focus on the session member (git delta is
	// exercised by the fuller round-trip test above). Rooting it at the
	// deterministic RunRoot(keyID) — where the cold path rebuilds — keeps
	// wtDir == run.WorktreePath, so ensureWorkspace doesn't take the
	// persist-new-path branch (SetWorktreePathSystem is unwired in this spawner).
	wtPath := worktree.RunRoot(runID)
	t.Cleanup(func() { _ = os.RemoveAll(wtPath) })
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "scratch survived")
	const sessionID = "sess-present"
	writeSession(t, wtPath, sessionID, `{"type":"summary","sid":"present"}`)

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, sessionID); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	if err := os.RemoveAll(wtPath); err != nil { // host loss: only the snapshot remains
		t.Fatalf("rm worktree: %v", err)
	}

	run := &domain.Conversation{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID, SessionID: sessionID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, gitSeed{})
	if err != nil {
		t.Fatalf("ensureWorkspace (cold): %v", err)
	}
	if !sessionTranscriptExists(got, sessionID) {
		t.Fatal("sessionTranscriptExists = false after rehydrating a transcript-bearing snapshot; the resume guard would wrongly fail a resumable run")
	}
}

// TestEnsureWorkspace_ColdPath_TranscriptlessSnapshotIsNotResumable: the exact
// shape behind the resume-fails-with-no-reason report — a run with a session id
// whose transcript was NOT captured (writeSnapshotTar skips the member and
// warns). The workspace rebuilds, but the resume guard must see it as
// unresumable so the delivery path fails with an actionable reason rather than
// handing the SDK a doomed --resume ("No conversation found").
func TestEnsureWorkspace_ColdPath_TranscriptlessSnapshotIsNotResumable(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-no-transcript"
	wtPath := worktree.RunRoot(runID)
	t.Cleanup(func() { _ = os.RemoveAll(wtPath) })
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "scratch survived")
	const sessionID = "sess-lost"
	// Deliberately NO writeSession: the run carries a session id but its
	// transcript is not on disk when the snapshot is taken.
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, sessionID); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}

	run := &domain.Conversation{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID, SessionID: sessionID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, gitSeed{})
	if err != nil {
		t.Fatalf("ensureWorkspace (cold): %v", err)
	}
	// The workspace itself rebuilt...
	assertFileContains(t, filepath.Join(got, "_tfac", "notes.txt"), "scratch survived")
	// ...but no transcript rode along, so the guard must report it unresumable.
	if sessionTranscriptExists(got, sessionID) {
		t.Fatal("sessionTranscriptExists = true for a transcript-less snapshot; the resume guard would not fire and the SDK would get a doomed --resume")
	}
}

// TestSnapshotWorkspace_StoresGzip: the stored blob is gzip-compressed — the
// two fat members (session transcript, ci-logs) make uncompressed storage
// pathological, so the staged tar is wrapped in gzip before Put. Asserts on
// the raw stored bytes: the storage seam must carry the compressed form, not
// just hand back something the reader can parse.
func TestSnapshotWorkspace_StoresGzip(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	// A non-git run-root is enough: format is decided by the writer wrapper,
	// not by which members ride in the tar.
	wtPath := t.TempDir()
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "scratch note")

	const runID = "wt-gzip"
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, ""); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, runID))
	if err != nil {
		t.Fatalf("get snapshot blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	magic := make([]byte, 2)
	if _, err := io.ReadFull(rc, magic); err != nil {
		t.Fatalf("read blob magic: %v", err)
	}
	if magic[0] != 0x1f || magic[1] != 0x8b {
		t.Errorf("stored blob starts with %#02x %#02x, want the gzip magic 0x1f 0x8b", magic[0], magic[1])
	}
}

// TestEnsureWorkspace_ColdPath_CorruptGzipChecksumErrors: a stored blob whose
// gzip CRC-32 trailer no longer matches its contents must fail the rehydrate
// rather than silently rebuild onto corrupt state. The tar reader stops at the
// archive's end-of-archive marker before the gzip footer, so the integrity
// check only fires because rehydrate drains the reader to EOF; this guards that
// drain. A non-git run-root keeps the focus on the integrity gate, which runs
// before any worktree mutation.
func TestEnsureWorkspace_ColdPath_CorruptGzipChecksumErrors(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-corrupt"
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "_tfac", "ci-logs", "x.log"), "log bytes the gzip trailer checksums over")
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, src, ""); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Flip a byte in the gzip footer's CRC-32 (the last 8 bytes are CRC-32 +
	// ISIZE) so the decompressed bytes no longer match the stored checksum.
	key := snapshotKey(runmode.LocalDefaultOrgID, runID)
	rc, err := s.Storage().Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get snapshot blob: %v", err)
	}
	blob, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read snapshot blob: %v", err)
	}
	if len(blob) < 8 {
		t.Fatalf("snapshot blob too small to corrupt: %d bytes", len(blob))
	}
	blob[len(blob)-8] ^= 0xff
	if err := s.Storage().Put(context.Background(), key, bytes.NewReader(blob)); err != nil {
		t.Fatalf("put corrupted blob: %v", err)
	}

	// Cold path: the warm worktree is absent, so the resume can only come from
	// the (now corrupt) blob — which must surface as an error.
	run := &domain.Conversation{ID: runID, WorktreePath: filepath.Join(t.TempDir(), "gone"), BlueprintRunID: runID}
	if _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, gitSeed{}); err == nil {
		t.Fatal("ensureWorkspace accepted a snapshot with a corrupted gzip checksum; want an integrity error")
	}
}

// TestSnapshotWorkspace_CompressionShrinksTranscriptHeavyBlob: a JSONL-heavy
// workspace — the dominant real-world shape, where the transcript carries
// every tool call and result verbatim — must store measurably smaller than
// its plain-tar equivalent. Loose bound only (half), not a pinned ratio.
func TestSnapshotWorkspace_CompressionShrinksTranscriptHeavyBlob(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	wtPath := t.TempDir()
	const sessionID = "sess-fat"
	var jsonl strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&jsonl, `{"type":"tool_result","seq":%d,"content":"$ go test ./...\nok  \tgithub.com/sky-ai-eng/triage-factory/internal/delegate\t1.2s\n"}%s`, i, "\n")
	}
	writeSession(t, wtPath, sessionID, jsonl.String())
	writeFile(t, filepath.Join(wtPath, "_tfac", "ci-logs", "test.log"),
		strings.Repeat("=== RUN   TestSomething\n--- PASS: TestSomething (0.01s)\n", 2000))

	const runID = "wt-fat"
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, sessionID); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, runID))
	if err != nil {
		t.Fatalf("get snapshot blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	gzSize, err := io.Copy(io.Discard, rc)
	if err != nil {
		t.Fatalf("size snapshot blob: %v", err)
	}

	// The same workspace through the tar writer alone = the plain equivalent.
	// The transcript now rides in as bytes (captured agent-side), so pass the
	// same JSONL the on-disk session holds.
	var plain bytes.Buffer
	if err := writeSnapshotTar(&plain, nil, wtPath, sessionID, []byte(jsonl.String())); err != nil {
		t.Fatalf("writeSnapshotTar (plain): %v", err)
	}
	if gzSize >= int64(plain.Len())/2 {
		t.Errorf("gzip blob = %d bytes vs plain tar = %d bytes; compression had no real effect", gzSize, plain.Len())
	}
}

// TestEnsureWorkspace_ColdPath_DetachedHead: a worktree snapshotted while HEAD
// is detached (e.g. the agent ran `git checkout <sha>`) rehydrates to the same
// detached commit — the manifest carries the HEAD SHA, so an empty branch no
// longer strands the run.
func TestEnsureWorkspace_ColdPath_DetachedHead(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-detached"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	// Agent commits, then detaches HEAD at that commit.
	writeFile(t, filepath.Join(wtPath, "agent.txt"), "committed by agent")
	gitT(t, wtPath, "add", "agent.txt")
	gitT(t, wtPath, "commit", "-m", "agent work")
	gitT(t, wtPath, "checkout", "--detach", "HEAD")
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\ndetached edit\n")
	headSHA := strings.TrimSpace(gitOut(t, wtPath, "rev-parse", "HEAD"))

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, ""); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Lose the worktree and drop the branch so the commit returns via the bundle.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	gitT(t, bareDir, "worktree", "prune")
	gitT(t, bareDir, "branch", "-D", "feature")

	run := &domain.Conversation{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, gitSeed{owner: owner, repo: repo})
	if err != nil {
		t.Fatalf("ensureWorkspace (detached): %v", err)
	}
	assertFileContains(t, filepath.Join(got, "agent.txt"), "committed by agent")
	assertFileContains(t, filepath.Join(got, "README.md"), "detached edit")
	if gotSHA := strings.TrimSpace(gitOut(t, got, "rev-parse", "HEAD")); gotSHA != headSHA {
		t.Errorf("rehydrated HEAD = %s, want %s", gotSHA, headSHA)
	}
	if err := exec.Command("git", "-C", got, "symbolic-ref", "-q", "HEAD").Run(); err == nil {
		t.Error("rehydrated HEAD is on a branch; want detached")
	}
}

// TestEnsureWorkspace_ColdPath_NeverPushedBranchNoCommits: a run on a local
// branch that was never pushed and has no local-only commits (only uncommitted
// changes) still rehydrates — the manifest's HEAD SHA recreates the branch even
// though the bundle is empty and there's no refs/remotes/origin/<branch>.
func TestEnsureWorkspace_ColdPath_NeverPushedBranchNoCommits(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const runID = "wt-nopush"
	wtPath, owner, repo := setupTestWorktree(t, runID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, runID) })

	// No commits — only an uncommitted edit on the never-pushed "feature" branch.
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\nwork in progress\n")
	headSHA := strings.TrimSpace(gitOut(t, wtPath, "rev-parse", "HEAD"))

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, runID, wtPath, ""); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Simulate a fresh host: worktree gone and the bare lacks the never-pushed
	// branch (only origin/* survives a fresh clone), but it still has origin/main
	// — which is where HEAD points, since "feature" had no commits.
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}
	bareDir, err := worktree.RepoDir(owner, repo)
	if err != nil {
		t.Fatalf("RepoDir: %v", err)
	}
	gitT(t, bareDir, "worktree", "prune")
	gitT(t, bareDir, "branch", "-D", "feature")

	run := &domain.Conversation{ID: runID, WorktreePath: wtPath, BlueprintRunID: runID}
	got, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, gitSeed{owner: owner, repo: repo})
	if err != nil {
		t.Fatalf("ensureWorkspace (never-pushed branch): %v", err)
	}
	assertFileContains(t, filepath.Join(got, "README.md"), "work in progress")
	if gotBranch := strings.TrimSpace(gitOut(t, got, "rev-parse", "--abbrev-ref", "HEAD")); gotBranch != "feature" {
		t.Errorf("rehydrated branch = %q, want feature", gotBranch)
	}
	if gotSHA := strings.TrimSpace(gitOut(t, got, "rev-parse", "HEAD")); gotSHA != headSHA {
		t.Errorf("rehydrated HEAD = %s, want %s", gotSHA, headSHA)
	}
}

// TestEnsureWorkspace_ColdPath_NoSnapshotErrors: with the worktree gone and no
// snapshot ever written, ensureWorkspace surfaces a clear error rather than
// silently handing back a dead path.
func TestEnsureWorkspace_ColdPath_NoSnapshotErrors(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	run := &domain.Conversation{ID: "wt-missing", WorktreePath: filepath.Join(t.TempDir(), "gone")}
	if _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, run, gitSeed{owner: "o", repo: "r"}); err == nil {
		t.Fatal("ensureWorkspace should error when neither the worktree nor a snapshot exists")
	}
}

// TestFailRun_DiscardsWorkspaceSnapshot: a parked run that then fails (e.g. an
// open run whose resume errors mid-execution) drops its snapshot rather than
// orphaning the blob. failRun is the single failure chokepoint covering the
// resume goroutine's failure exits.
func TestFailRun_DiscardsWorkspaceSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, _, runID, taskID := setupAdvanceFixture(t, "failrun-discard")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)

	ctx := context.Background()
	key := snapshotKey(runmode.LocalDefaultOrgID, runID)
	if err := blobs.Put(ctx, key, strings.NewReader("snapshot")); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// triggerType "event" so failRun routes through the admin-pool System
	// methods (no synthetic-claims tx needed in the fixture).
	s.failRun(runmode.LocalDefaultOrgID, runID, taskID, "", "event", "", "boom", domain.RunFailureUnclassified)

	if ok, _ := blobs.Exists(ctx, key); ok {
		t.Error("failRun did not discard the workspace snapshot — blob orphaned on failure")
	}
}

// --- helpers ---------------------------------------------------------------

// newStorageSpawner builds a bare Spawner with only the blob store wired —
// enough for the snapshot/rehydrate path, which touches no DB on the same host
// (the rebuilt cwd equals the stored worktree_path, so no SetWorktreePath).
func newStorageSpawner(t *testing.T) *Spawner {
	t.Helper()
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	s.SetStorage(blobs)
	return s
}

// setupTestWorktree stands up a real origin bare + a delegated worktree on a
// "feature" branch via the production worktree path, so the snapshot/rehydrate
// code exercises actual git plumbing. Returns the worktree path and its
// owner/repo.
func setupTestWorktree(t *testing.T, runID string) (wtPath, owner, repo string) {
	t.Helper()
	owner, repo = "o", "r"

	origin := filepath.Join(t.TempDir(), "origin.git")
	gitT(t, "", "init", "--bare", origin)
	seed := filepath.Join(t.TempDir(), "seed")
	gitT(t, "", "clone", origin, seed)
	gitT(t, seed, "checkout", "-b", "main")
	writeFile(t, filepath.Join(seed, "README.md"), "hello\n")
	gitT(t, seed, "add", "README.md")
	gitT(t, seed, "commit", "-m", "init")
	gitT(t, seed, "push", "origin", "main")

	wt, err := worktree.CreateForBranch(context.Background(), owner, repo, origin, "main", "feature", runID)
	if err != nil {
		t.Fatalf("CreateForBranch: %v", err)
	}
	return wt, owner, repo
}

// setupGitTestEnv isolates HOME (so ~/.claude session writes + git config land
// in a throwaway dir) and pins a git identity so commits don't depend on the
// developer's global config.
func setupGitTestEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_NAME", "Test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %q: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// gitOut runs git and returns stdout (for rev-parse and friends).
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s in %q: %v", strings.Join(args, " "), dir, err)
	}
	return string(out)
}

// writeSession writes a fake Claude session transcript for wtPath's cwd and
// returns its path.
func writeSession(t *testing.T, wtPath, sessionID, body string) string {
	t.Helper()
	p, err := worktree.ClaudeSessionPath(worktree.ResolveClaudeProjectCwd(wtPath), sessionID)
	if err != nil {
		t.Fatalf("ClaudeSessionPath: %v", err)
	}
	writeFile(t, p, body)
	return p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Errorf("%s = %q, want it to contain %q", path, string(data), want)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists or errored unexpectedly (%v); it should have been excluded from the snapshot", path, err)
	}
}
