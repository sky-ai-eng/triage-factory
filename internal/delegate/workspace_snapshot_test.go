package delegate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
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

	const conversationID = "wt-warm"
	wtPath, owner, repo := setupTestWorktree(t, conversationID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, conversationID) })

	const sessionID = "sess-warm"
	writeSession(t, wtPath, sessionID, `{"type":"summary"}`)

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, sessionID, domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Written after the snapshot: a rehydrate rebuilds from the (older) blob and
	// would lose this, so its survival distinguishes warm reuse from a rebuild.
	marker := filepath.Join(wtPath, "_tfac", "notes", "warm-marker.txt")
	writeFile(t, marker, "warm")

	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtPath, BlueprintRunID: conversationID}
	got, prov, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{owner: owner, repo: repo}, nil)
	if err != nil {
		t.Fatalf("ensureWorkspace (warm): %v", err)
	}
	if prov != domain.WorkspaceProvenanceWarm {
		t.Errorf("warm provenance = %q, want warm — a reused tree must not be reported as a restore", prov)
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

	const conversationID = "wt-cold"
	wtPath, owner, repo := setupTestWorktree(t, conversationID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, conversationID) })

	// The agent commits work (advances the branch; rides in the bundle).
	writeFile(t, filepath.Join(wtPath, "agent.txt"), "committed by agent")
	gitT(t, wtPath, "add", "agent.txt")
	gitT(t, wtPath, "commit", "-m", "agent work")

	// ...then leaves an uncommitted edit (rides in the patch).
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\nuncommitted edit\n")

	// Ephemeral _tfac is snapshotted; entity-memory / ci-logs are excluded
	// (they re-materialize from the DB, or re-download from GitHub).
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes", "build.log"), "scratch note")
	writeFile(t, filepath.Join(wtPath, "_tfac", "entity-memory", "ns", "x.md"), "memory")
	writeFile(t, filepath.Join(wtPath, "_tfac", "ci-logs", "42", "build.log"), "ci log line")

	const sessionID = "sess-cold"
	sessPath := writeSession(t, wtPath, sessionID, `{"type":"summary","sid":"cold"}`)

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, sessionID, domain.ConversationRuntimeSDK); err != nil {
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

	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtPath, BlueprintRunID: conversationID}
	got, prov, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{owner: owner, repo: repo}, nil)
	if err != nil {
		t.Fatalf("ensureWorkspace (cold): %v", err)
	}
	if prov != domain.WorkspaceProvenanceRehydrated {
		t.Errorf("cold provenance = %q, want rehydrated", prov)
	}
	if got != wtPath {
		t.Errorf("rehydrated cwd = %q, want %q", got, wtPath)
	}

	assertFileContains(t, filepath.Join(got, "agent.txt"), "committed by agent") // bundle
	assertFileContains(t, filepath.Join(got, "README.md"), "uncommitted edit")   // patch
	assertFileContains(t, filepath.Join(got, "_tfac", "notes", "build.log"), "scratch note")
	assertMissing(t, filepath.Join(got, "_tfac", "entity-memory", "ns", "x.md"))
	assertMissing(t, filepath.Join(got, "_tfac", "ci-logs", "42", "build.log"))
	// The one exclusion the agent can notice: it gets an explanation in place
	// of the logs, not a directory that silently emptied.
	assertFileContains(t, filepath.Join(got, "_tfac", "ci-logs", ciLogsNoticeFile), "download-logs")

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

	const conversationID = "wt-has-transcript"
	// A non-git run-root keeps the focus on the session member (git delta is
	// exercised by the fuller round-trip test above). Rooting it at the
	// deterministic RunRoot(keyID) — where the cold path rebuilds — keeps
	// wtDir == conv.WorktreePath, so ensureWorkspace doesn't take the
	// persist-new-path branch (SetWorktreePathSystem is unwired in this spawner).
	wtPath := worktree.RunRoot(conversationID)
	t.Cleanup(func() { _ = os.RemoveAll(wtPath) })
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "scratch survived")
	const sessionID = "sess-present"
	writeSession(t, wtPath, sessionID, `{"type":"summary","sid":"present"}`)

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, sessionID, domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	if err := os.RemoveAll(wtPath); err != nil { // host loss: only the snapshot remains
		t.Fatalf("rm worktree: %v", err)
	}

	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtPath, BlueprintRunID: conversationID, SessionID: sessionID}
	got, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{}, nil)
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

	const conversationID = "wt-no-transcript"
	wtPath := worktree.RunRoot(conversationID)
	t.Cleanup(func() { _ = os.RemoveAll(wtPath) })
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "scratch survived")
	const sessionID = "sess-lost"
	// Deliberately NO writeSession: the conversation carries a session id but its
	// transcript is not on disk when the snapshot is taken.
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, sessionID, domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	if err := os.RemoveAll(wtPath); err != nil {
		t.Fatalf("rm worktree: %v", err)
	}

	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtPath, BlueprintRunID: conversationID, SessionID: sessionID}
	got, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{}, nil)
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

// TestSnapshotWorkspace_StoresZstd: the stored blob is zstd-compressed — the
// session transcript alone makes uncompressed storage pathological, so the
// staged tar is wrapped in zstd before Put. Asserts on
// the raw stored bytes: the storage seam must carry the compressed form, not
// just hand back something the reader can parse.
func TestSnapshotWorkspace_StoresZstd(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	// A non-git run-root is enough: format is decided by the writer wrapper,
	// not by which members ride in the tar.
	wtPath := t.TempDir()
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "scratch note")

	const conversationID = "wt-zstd"
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, "", domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, conversationID))
	if err != nil {
		t.Fatalf("get snapshot blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	magic := make([]byte, len(zstdMagic))
	if _, err := io.ReadFull(rc, magic); err != nil {
		t.Fatalf("read blob magic: %v", err)
	}
	if !bytes.Equal(magic, zstdMagic) {
		t.Errorf("stored blob starts with % x, want zstd magic % x", magic, zstdMagic)
	}
}

// TestEnsureWorkspace_ColdPath_TruncatedZstdErrors: a stored blob whose zstd
// checksum is truncated must fail the rehydrate
// rather than silently rebuild onto corrupt state. The tar reader stops at the
// archive's end-of-archive marker before the zstd footer, so the integrity
// check only fires because rehydrate drains the reader to EOF; this guards that
// drain. A non-git run-root keeps the focus on the integrity gate, which runs
// before any worktree mutation.
func TestEnsureWorkspace_ColdPath_TruncatedZstdErrors(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const conversationID = "wt-corrupt"
	worktree.RemoveRunRoot(conversationID)
	t.Cleanup(func() { worktree.RemoveRunRoot(conversationID) })
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "_tfac", "notes", "x.log"), "scratch bytes the zstd frame checksum covers")
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", src, "", domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	// Remove a byte from the frame checksum. The tar itself remains complete,
	// making the decoder drain after tar EOF the integrity gate under test.
	key := snapshotKey(runmode.LocalDefaultOrgID, conversationID)
	rc, err := s.Storage().Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get snapshot blob: %v", err)
	}
	blob, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read snapshot blob: %v", err)
	}
	if len(blob) < 5 {
		t.Fatalf("snapshot blob too small to corrupt: %d bytes", len(blob))
	}
	blob = blob[:len(blob)-1]
	if err := s.Storage().Put(context.Background(), key, bytes.NewReader(blob)); err != nil {
		t.Fatalf("put corrupted blob: %v", err)
	}

	// Cold path: the warm worktree is absent, so the resume can only come from
	// the (now corrupt) blob — which must surface as an error.
	wtDir := worktree.RunRoot(conversationID)
	conv := &domain.Conversation{ID: conversationID, WorktreePath: filepath.Join(t.TempDir(), "gone"), BlueprintRunID: conversationID}
	if _, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{}, nil); err == nil {
		t.Fatal("ensureWorkspace accepted a truncated zstd checksum; want an integrity error")
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree was mutated before integrity validation: stat error = %v", err)
	}
}

func TestEnsureWorkspace_ColdPath_LegacyGzipSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const conversationID = "wt-legacy-gzip"
	worktree.RemoveRunRoot(conversationID)
	t.Cleanup(func() { worktree.RemoveRunRoot(conversationID) })
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "_tfac", "notes.txt"), "from an old gzip snapshot")
	var blob bytes.Buffer
	gzw := gzip.NewWriter(&blob)
	if err := writeSnapshotTar(gzw, worktree.CapturedState{}, src); err != nil {
		t.Fatalf("write legacy snapshot tar: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("close legacy gzip: %v", err)
	}
	if err := s.Storage().Put(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, conversationID), bytes.NewReader(blob.Bytes())); err != nil {
		t.Fatalf("put legacy snapshot: %v", err)
	}

	wtDir := worktree.RunRoot(conversationID)
	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtDir, BlueprintRunID: conversationID}
	got, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{}, nil)
	if err != nil {
		t.Fatalf("rehydrate legacy gzip snapshot: %v", err)
	}
	assertFileContains(t, filepath.Join(got, "_tfac", "notes.txt"), "from an old gzip snapshot")
}

// TestSnapshotWorkspace_OmitsCILogs is the exclusion acceptance: an extracted
// GitHub Actions log archive is re-downloadable, so it fails the snapshot's own
// "non-recoverable state only" admission test and must contribute nothing to
// the blob — while the scratch files that exist nowhere else still ride along.
// The size bound is the point of the exclusion: megabytes of logs in the tree,
// kilobytes of blob out.
func TestSnapshotWorkspace_OmitsCILogs(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	// A non-git run-root: the git members would only add noise to the size
	// bound, and the exclusion is decided by the scratch walk either way.
	wtPath := t.TempDir()
	logs := strings.Repeat("2026-08-20T12:00:00.0000000Z ##[group]Run go test ./...\n", 60_000)
	writeFile(t, filepath.Join(wtPath, "_tfac", "ci-logs", "42", "1_build.txt"), logs)
	writeFile(t, filepath.Join(wtPath, "_tfac", "ci-logs", "42", "2_test.txt"), logs)
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes", "keep.txt"), "the agent's own intermediate")

	const conversationID = "wt-cilogs"
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, "", domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	members := snapshotMembers(t, s.Storage(), snapshotKey(runmode.LocalDefaultOrgID, conversationID))
	for name := range members {
		if strings.HasPrefix(name, snapScratchPrefix+worktree.CILogsDir+"/") {
			t.Errorf("snapshot carries %q; ci-logs is re-downloadable and must not ride in the blob", name)
		}
	}
	if !members[snapScratchPrefix+"notes/keep.txt"] {
		t.Errorf("snapshot dropped the agent's own scratch (members: %v); only the re-fetchable subtrees are excluded", members)
	}

	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, conversationID))
	if err != nil {
		t.Fatalf("get snapshot blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	size, err := io.Copy(io.Discard, rc)
	if err != nil {
		t.Fatalf("size snapshot blob: %v", err)
	}
	if size > 8<<10 {
		t.Errorf("blob = %d bytes for a workspace whose only bulk is %d bytes of CI logs; the archive phase is still paying for them", size, 2*len(logs))
	}
}

// TestEnsureWorkspace_ColdPath_NoCILogsNoticeWithoutOmittedLogs: the notice
// explains a specific absence, so a workspace that never downloaded any logs
// must not get one. Otherwise every cold rehydrate would invent a ci-logs
// directory and tell the agent about logs that never existed.
func TestEnsureWorkspace_ColdPath_NoCILogsNoticeWithoutOmittedLogs(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const conversationID = "wt-no-cilogs"
	worktree.RemoveRunRoot(conversationID)
	t.Cleanup(func() { worktree.RemoveRunRoot(conversationID) })
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "_tfac", "notes.txt"), "scratch note")
	// An empty ci-logs directory is nothing dropped, so it is nothing to
	// explain — the walk must distinguish it from a populated one.
	if err := os.MkdirAll(filepath.Join(src, "_tfac", "ci-logs"), 0o755); err != nil {
		t.Fatalf("mkdir ci-logs: %v", err)
	}
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", src, "", domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	wtDir := worktree.RunRoot(conversationID)
	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtDir, BlueprintRunID: conversationID}
	got, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{}, nil)
	if err != nil {
		t.Fatalf("ensureWorkspace (cold): %v", err)
	}
	assertFileContains(t, filepath.Join(got, "_tfac", "notes.txt"), "scratch note")
	if _, err := os.Stat(filepath.Join(got, "_tfac", worktree.CILogsDir)); !os.IsNotExist(err) {
		t.Errorf("rehydrate created _tfac/ci-logs for a workspace that never had logs in it (stat error = %v)", err)
	}
}

// TestEnsureWorkspace_ColdPath_PreExclusionSnapshotRestoresItsCILogs: blobs
// written before the exclusion are durable state that carries its logs as
// ordinary scratch members. Those must still restore verbatim — and must not
// acquire a notice claiming they were dropped.
func TestEnsureWorkspace_ColdPath_PreExclusionSnapshotRestoresItsCILogs(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const conversationID = "wt-preexclusion"
	worktree.RemoveRunRoot(conversationID)
	t.Cleanup(func() { worktree.RemoveRunRoot(conversationID) })

	// Hand-built, because the writer under test no longer produces this shape:
	// scratch members under ci-logs, and a manifest with no omission recorded.
	var blob bytes.Buffer
	zw, err := zstd.NewWriter(&blob)
	if err != nil {
		t.Fatalf("open zstd: %v", err)
	}
	tw := tar.NewWriter(zw)
	if err := writeTarBytes(tw, snapScratchPrefix+"ci-logs/42/1_build.txt", []byte("logs an older build captured")); err != nil {
		t.Fatalf("write legacy ci-logs member: %v", err)
	}
	manifest, err := json.Marshal(snapshotManifest{})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := writeTarBytes(tw, snapManifest, manifest); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zstd: %v", err)
	}
	if err := s.Storage().Put(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, conversationID), bytes.NewReader(blob.Bytes())); err != nil {
		t.Fatalf("put pre-exclusion snapshot: %v", err)
	}

	wtDir := worktree.RunRoot(conversationID)
	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtDir, BlueprintRunID: conversationID}
	got, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{}, nil)
	if err != nil {
		t.Fatalf("rehydrate pre-exclusion snapshot: %v", err)
	}
	assertFileContains(t, filepath.Join(got, "_tfac", "ci-logs", "42", "1_build.txt"), "logs an older build captured")
	if _, err := os.Stat(filepath.Join(got, "_tfac", worktree.CILogsDir, ciLogsNoticeFile)); !os.IsNotExist(err) {
		t.Errorf("rehydrate planted the not-restored notice beside logs it did restore (stat error = %v)", err)
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
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes", "test.log"),
		strings.Repeat("=== RUN   TestSomething\n--- PASS: TestSomething (0.01s)\n", 2000))

	const conversationID = "wt-fat"
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, sessionID, domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, conversationID))
	if err != nil {
		t.Fatalf("get snapshot blob: %v", err)
	}
	defer func() { _ = rc.Close() }()
	compressedSize, err := io.Copy(io.Discard, rc)
	if err != nil {
		t.Fatalf("size snapshot blob: %v", err)
	}

	// The same workspace through the tar writer alone = the plain equivalent.
	// The transcript now rides in as bytes (captured agent-side), so pass the
	// same JSONL the on-disk session holds.
	var plain bytes.Buffer
	if err := writeSnapshotTar(&plain, worktree.CapturedState{SessionID: sessionID, Transcript: []byte(jsonl.String())}, wtPath); err != nil {
		t.Fatalf("writeSnapshotTar (plain): %v", err)
	}
	if compressedSize >= int64(plain.Len())/2 {
		t.Errorf("compressed blob = %d bytes vs plain tar = %d bytes; compression had no real effect", compressedSize, plain.Len())
	}
}

func TestWriteSnapshotTar_StreamsStagedCaptureMembers(t *testing.T) {
	staging := t.TempDir()
	bundlePath := filepath.Join(staging, worktree.CaptureBundleFile)
	patchPath := filepath.Join(staging, worktree.CapturePatchFile)
	transcriptPath := filepath.Join(staging, worktree.CaptureTranscriptFile)
	writeFile(t, bundlePath, "bundle-bytes")
	writeFile(t, patchPath, "patch-bytes")
	writeFile(t, transcriptPath, "transcript-bytes")

	captured := worktree.CapturedState{
		Delta:          &worktree.GitDelta{Branch: "aa/work", Head: "abc123"},
		SessionID:      "sess-staged",
		BundlePath:     bundlePath,
		PatchPath:      patchPath,
		TranscriptPath: transcriptPath,
	}
	var blob bytes.Buffer
	if err := writeSnapshotTar(&blob, captured, t.TempDir()); err != nil {
		t.Fatalf("writeSnapshotTar: %v", err)
	}

	want := map[string]string{
		snapBundle:  "bundle-bytes",
		snapPatch:   "patch-bytes",
		snapSession: "transcript-bytes",
	}
	tr := tar.NewReader(bytes.NewReader(blob.Bytes()))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		member, ok := want[hdr.Name]
		if !ok {
			continue
		}
		got, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != member {
			t.Errorf("%s = %q, want %q", hdr.Name, got, member)
		}
		delete(want, hdr.Name)
	}
	if len(want) != 0 {
		t.Errorf("snapshot omitted staged members: %v", want)
	}
}

func TestCapturedBytesSize_RejectsStagedSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "member")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := capturedBytesSize(nil, link); err == nil {
		t.Fatal("capturedBytesSize accepted a staged symlink")
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

	const conversationID = "wt-detached"
	wtPath, owner, repo := setupTestWorktree(t, conversationID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, conversationID) })

	// Agent commits, then detaches HEAD at that commit.
	writeFile(t, filepath.Join(wtPath, "agent.txt"), "committed by agent")
	gitT(t, wtPath, "add", "agent.txt")
	gitT(t, wtPath, "commit", "-m", "agent work")
	gitT(t, wtPath, "checkout", "--detach", "HEAD")
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\ndetached edit\n")
	headSHA := strings.TrimSpace(gitOut(t, wtPath, "rev-parse", "HEAD"))

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, "", domain.ConversationRuntimeSDK); err != nil {
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

	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtPath, BlueprintRunID: conversationID}
	got, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{owner: owner, repo: repo}, nil)
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

	const conversationID = "wt-nopush"
	wtPath, owner, repo := setupTestWorktree(t, conversationID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, conversationID) })

	// No commits — only an uncommitted edit on the never-pushed "feature" branch.
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\nwork in progress\n")
	headSHA := strings.TrimSpace(gitOut(t, wtPath, "rev-parse", "HEAD"))

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, "", domain.ConversationRuntimeSDK); err != nil {
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

	conv := &domain.Conversation{ID: conversationID, WorktreePath: wtPath, BlueprintRunID: conversationID}
	got, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{owner: owner, repo: repo}, nil)
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

	conv := &domain.Conversation{ID: "wt-missing", WorktreePath: filepath.Join(t.TempDir(), "gone")}
	if _, _, err := s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, gitSeed{owner: "o", repo: "r"}, nil); err == nil {
		t.Fatal("ensureWorkspace should error when neither the worktree nor a snapshot exists")
	}
}

// TestFailRun_DiscardsWorkspaceSnapshot: a parked run that then fails (e.g. an
// open run whose resume errors mid-execution) drops its snapshot rather than
// orphaning the blob. failConversation is the single failure chokepoint covering the
// resume goroutine's failure exits.
func TestFailRun_DiscardsWorkspaceSnapshot(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	s, _, conversationID, taskID := setupAdvanceFixture(t, "failrun-discard")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)

	ctx := context.Background()
	key := snapshotKey(runmode.LocalDefaultOrgID, conversationID)
	if err := blobs.Put(ctx, key, strings.NewReader("snapshot")); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// triggerType "event" so failConversation routes through the admin-pool System
	// methods (no synthetic-claims tx needed in the fixture).
	s.failConversation(runmode.LocalDefaultOrgID, conversationID, taskID, "", "event", "", "boom", domain.ConversationFailureUnclassified)

	if ok, _ := blobs.Exists(ctx, key); ok {
		t.Error("failConversation did not discard the workspace snapshot — blob orphaned on failure")
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
func setupTestWorktree(t *testing.T, conversationID string) (wtPath, owner, repo string) {
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

	wt, err := worktree.CreateForBranch(context.Background(), owner, repo, origin, "main", "feature", conversationID)
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

// TestSnapshotWorkspace_PhaseSpans pins the span family a slow park is read
// through: the punctual workspace.snapshot root split into capture / archive /
// put children, each carrying the sizes that explain its own duration, all
// stamped with the runtime whose snapshot this was. The worktree carries one
// member of every kind so every size attribute has something real to measure.
func TestSnapshotWorkspace_PhaseSpans(t *testing.T) {
	read := recordSpans(t)
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	const conversationID = "wt-spans"
	wtPath, _, _ := setupTestWorktree(t, conversationID)
	t.Cleanup(func() { _ = worktree.RemoveAt(wtPath, conversationID) })

	writeFile(t, filepath.Join(wtPath, "agent.txt"), "committed by agent")
	gitT(t, wtPath, "add", "agent.txt")
	gitT(t, wtPath, "commit", "-m", "agent work")
	writeFile(t, filepath.Join(wtPath, "README.md"), "hello\nuncommitted edit\n")
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes", "build.log"), strings.Repeat("scratch note\n", 200))
	const sessionID = "sess-spans"
	writeSession(t, wtPath, sessionID, `{"type":"summary","sid":"spans"}`)

	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, sessionID, domain.ConversationRuntimeSDK); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	spans := read()
	roots := spansNamed(spans, "workspace.snapshot")
	if len(roots) != 1 {
		t.Fatalf("workspace.snapshot spans = %d, want 1", len(roots))
	}
	root := roots[0]
	if got := spanAttr(t, root, "runtime").AsString(); got != domain.ConversationRuntimeSDK {
		t.Errorf("runtime = %q, want %q", got, domain.ConversationRuntimeSDK)
	}
	total := spanAttr(t, root, "size_bytes").AsInt64()
	if total <= 0 {
		t.Errorf("root size_bytes = %d, want > 0", total)
	}

	// The phases are CHILDREN of the punctual root — one trace per snapshot,
	// since the whole operation is bounded work in one frame — and each is
	// self-describing: runtime rides on every phase because the dashboard
	// reads the sizes off the phase spans alone, where the parent's
	// attributes are out of reach.
	for _, name := range []string{"workspace.snapshot.capture", "workspace.snapshot.archive", "workspace.snapshot.put"} {
		phases := spansNamed(spans, name)
		if len(phases) != 1 {
			t.Fatalf("%s spans = %d, want 1", name, len(phases))
		}
		p := phases[0]
		if p.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s parents to %v, want the workspace.snapshot span", name, p.Parent().SpanID())
		}
		if got := spanAttr(t, p, "runtime").AsString(); got != domain.ConversationRuntimeSDK {
			t.Errorf("%s runtime = %q, want %q", name, got, domain.ConversationRuntimeSDK)
		}
	}

	capture := spansNamed(spans, "workspace.snapshot.capture")[0]
	for _, key := range []string{"snapshot.bundle_bytes", "snapshot.patch_bytes", "snapshot.transcript_bytes"} {
		if got := spanAttr(t, capture, key).AsInt64(); got <= 0 {
			t.Errorf("capture %s = %d, want > 0 for a worktree carrying that member", key, got)
		}
	}

	archive := spansNamed(spans, "workspace.snapshot.archive")[0]
	raw := spanAttr(t, archive, "snapshot.raw_bytes").AsInt64()
	gz := spanAttr(t, archive, "size_bytes").AsInt64()
	if raw <= 0 || gz <= 0 {
		t.Fatalf("archive raw_bytes=%d size_bytes=%d, want both > 0", raw, gz)
	}
	if raw <= gz {
		t.Errorf("raw_bytes (%d) <= size_bytes (%d); the members are compressible text plus tar padding, so raw in must exceed compressed out — this pair is what a codec change proves itself against", raw, gz)
	}
	if gz != total {
		t.Errorf("archive size_bytes = %d, root size_bytes = %d; both name the one staged blob", gz, total)
	}

	put := spansNamed(spans, "workspace.snapshot.put")[0]
	if got := spanAttr(t, put, "size_bytes").AsInt64(); got != gz {
		t.Errorf("put size_bytes = %d, want the staged blob's %d", got, gz)
	}
}

// TestSnapshotWorkspace_PhaseSpans_NonGitNoSession is the other end of the
// coverage matrix: a native conversation never snapshots a transcript and a
// non-git run-root has no delta, so those sizes report zero rather than
// vanishing — a dashboard reading transcript sizes filters on runtime, and
// the explicit zero keeps a native row from reading as a failed capture.
func TestSnapshotWorkspace_PhaseSpans_NonGitNoSession(t *testing.T) {
	read := recordSpans(t)
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s := newStorageSpawner(t)

	wtPath := t.TempDir()
	writeFile(t, filepath.Join(wtPath, "_tfac", "notes.txt"), "scratch note")

	const conversationID = "wt-spans-native"
	if err := s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, conversationID, conversationID, "", wtPath, "", domain.ConversationRuntimeNative); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}

	spans := read()
	capture := spansNamed(spans, "workspace.snapshot.capture")
	if len(capture) != 1 {
		t.Fatalf("workspace.snapshot.capture spans = %d, want 1", len(capture))
	}
	if got := spanAttr(t, capture[0], "runtime").AsString(); got != domain.ConversationRuntimeNative {
		t.Errorf("runtime = %q, want %q", got, domain.ConversationRuntimeNative)
	}
	for _, key := range []string{"snapshot.bundle_bytes", "snapshot.patch_bytes", "snapshot.transcript_bytes"} {
		if got := spanAttr(t, capture[0], key).AsInt64(); got != 0 {
			t.Errorf("capture %s = %d, want an explicit 0 for a member this snapshot doesn't carry", key, got)
		}
	}
}
