package delegate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestEnsureWorkspace_WarmTreeIsReused: a parked run root that survived on disk
// is returned as-is. A file written after the snapshot survives, which a
// rebuild from the older blob would have lost.
func TestEnsureWorkspace_WarmTreeIsReused(t *testing.T) {
	f := newSnapshotFixture(t, "task-warm")
	f.addCheckout(t, "acme/app", "default")
	f.snapshot(t, "", domain.ConversationRuntimeSDK)
	marker := filepath.Join(f.root, worktree.ScratchDir, "warm-marker.txt")
	writeFile(t, marker, "warm")

	got, prov, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), f.restorer(), nil)
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	if prov != domain.WorkspaceProvenanceWarm || got != f.root {
		t.Errorf("ensureWorkspace = (%q, %q), want the warm root %q", got, prov, f.root)
	}
	assertFileContains(t, marker, "warm")
	if len(f.ledger.recordedRows()) != 0 {
		t.Errorf("a warm tree recorded checkouts %v; nothing was rebuilt", f.ledger.recordedRows())
	}
}

// TestEnsureWorkspace_ColdRoundTrip is the restore acceptance: a run root with
// a PR checkout and checkouts of two other repos — each with an unpushed
// commit, an uncommitted edit and an untracked file — plus scratch and a
// session transcript, rebuilt after the root is gone. Every checkout comes
// back exactly, as a self-contained clone when the run trees are, the PR
// checkout with its push tracking, and each is recorded as the restoring
// conversation's. A snapshot of the rebuilt tree carries all three again.
func TestEnsureWorkspace_ColdRoundTrip(t *testing.T) {
	for _, selfContained := range []bool{false, true} {
		name := "linked"
		if selfContained {
			name = "self-contained"
		}
		t.Run(name, func(t *testing.T) {
			f := newSnapshotFixture(t, "task-cold")
			runmode.SetLocalSandboxForTest(t, selfContained)

			pr := f.addCheckout(t, "acme/app", "pr-7")
			lib := f.addCheckout(t, "acme/lib", "default")
			docs := f.addCheckout(t, "acme/docs", "ref-main")
			gitT(t, lib, "checkout", "-q", "-b", "agent-work")
			heads := map[string]string{}
			for _, co := range []string{pr, lib, docs} {
				dirtyCheckout(t, co)
				heads[co] = strings.TrimSpace(gitOut(t, co, "rev-parse", "HEAD"))
			}
			prBranch := worktree.CurrentBranch(pr)

			writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes", "build.log"), "scratch note")
			writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "entity-memory", "ns", "x.md"), "memory")
			writeFile(t, filepath.Join(f.root, worktree.ScratchDir, worktree.CILogsDir, "42", "build.log"), "ci log line")
			const sessionID = "sess-cold"
			sessPath := writeSession(t, f.root, sessionID, `{"type":"summary","sid":"cold"}`)

			f.snapshot(t, sessionID, domain.ConversationRuntimeSDK)
			f.loseRoot(t)
			if err := os.Remove(sessPath); err != nil {
				t.Fatalf("rm session: %v", err)
			}

			conv := f.conv(sessionID)
			got, prov, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, f.restorer(), nil)
			if err != nil {
				t.Fatalf("ensureWorkspace: %v", err)
			}
			if prov != domain.WorkspaceProvenanceRehydrated || got != f.root {
				t.Fatalf("ensureWorkspace = (%q, %q), want %q rehydrated", got, prov, f.root)
			}
			if worktree.IsGitWorktree(got) {
				t.Error("the rebuilt run root is a git checkout; it is a plain folder")
			}
			for _, co := range []string{pr, lib, docs} {
				assertFileContains(t, filepath.Join(co, "committed.txt"), "unpushed commit")
				assertFileContains(t, filepath.Join(co, "README.md"), "uncommitted edit")
				assertFileContains(t, filepath.Join(co, "untracked.txt"), "untracked file")
				if head := strings.TrimSpace(gitOut(t, co, "rev-parse", "HEAD")); head != heads[co] {
					t.Errorf("%s HEAD = %s, want %s", co, head, heads[co])
				}
				if fi, err := os.Lstat(filepath.Join(co, ".git")); err != nil || fi.IsDir() != selfContained {
					t.Errorf("%s/.git is a directory = %v (err %v), want %v", co, fi != nil && fi.IsDir(), err, selfContained)
				}
			}
			if b := worktree.CurrentBranch(pr); b != prBranch {
				t.Errorf("PR checkout branch = %q, want %q", b, prBranch)
			}
			if target := worktree.PushTargetBranch(pr); target != "feature" {
				t.Errorf("PR checkout push target = %q, want the PR head branch", target)
			}
			if b := worktree.CurrentBranch(lib); b != "agent-work" {
				t.Errorf("acme/lib branch = %q, want agent-work", b)
			}
			if err := exec.Command("git", "-C", docs, "symbolic-ref", "-q", "HEAD").Run(); err == nil {
				t.Error("acme/docs came back on a branch; it was detached")
			}

			assertFileContains(t, filepath.Join(got, worktree.ScratchDir, "notes", "build.log"), "scratch note")
			assertMissing(t, filepath.Join(got, worktree.ScratchDir, "entity-memory", "ns", "x.md"))
			assertMissing(t, filepath.Join(got, worktree.ScratchDir, worktree.CILogsDir, "42", "build.log"))
			assertFileContains(t, filepath.Join(got, worktree.ScratchDir, worktree.CILogsDir, ciLogsNoticeFile), "download-logs")
			if !sessionTranscriptExists(got, sessionID) {
				t.Error("the session transcript did not come back; a --resume would fail")
			}

			recorded := map[string]domain.ConversationWorktree{}
			for _, w := range f.ledger.recordedRows() {
				recorded[w.Path] = w
			}
			for _, co := range []string{pr, lib, docs} {
				if w, ok := recorded[co]; !ok || w.ConversationID != conv.ID {
					t.Errorf("no row recorded for %s as %s's (recorded: %v)", co, conv.ID, f.ledger.recordedRows())
				}
			}

			f.snapshot(t, sessionID, domain.ConversationRuntimeSDK)
			members := snapshotMembers(t, f.s.Storage(), snapshotKey(runmode.LocalDefaultOrgID, f.key))
			for _, rel := range []string{"acme/app/pr-7", "acme/lib/default", "acme/docs/ref-main"} {
				if !members[snapCheckoutsPrefix+rel+"/bundle"] || !members[snapCheckoutsPrefix+rel+"/patch"] {
					t.Errorf("the snapshot of the rebuilt tree lost %s's members (members: %v)", rel, members)
				}
			}
		})
	}
}

// TestEnsureWorkspace_FailedCheckoutLeavesNothing: one checkout that cannot be
// rebuilt fails the whole restore, and nothing of the attempt is left behind —
// no root, no checkout, no recorded row — for a later claim to take for a warm
// tree.
func TestEnsureWorkspace_FailedCheckoutLeavesNothing(t *testing.T) {
	f := newSnapshotFixture(t, "task-fail")
	f.addCheckout(t, "acme/lib", "default")
	f.addCheckout(t, "acme/app", "pr-7")
	f.snapshot(t, "", domain.ConversationRuntimeNative)
	f.loseRoot(t)

	restorer := f.restorer()
	restorer.pr = func(context.Context, string, string, int) (*ghclient.PRView, error) {
		return nil, errors.New("pull request read refused")
	}
	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), restorer, failingFreshBuilder(t)); err == nil {
		t.Fatal("ensureWorkspace succeeded with a checkout that could not be rebuilt")
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Errorf("the run root survived a failed restore (stat err %v)", err)
	}
	if rows := f.ledger.recordedRows(); len(rows) != 0 {
		t.Errorf("a failed restore recorded %v", rows)
	}
}

// TestEnsureWorkspace_RefusedState covers the state an older binary left: a
// blob without the layout version, a blob in the old compression, a warm root
// that is itself a git checkout, and no blob at all. None is converted. A
// native conversation is built a fresh workspace; an SDK resume is refused as
// expired.
func TestEnsureWorkspace_RefusedState(t *testing.T) {
	stage := map[string]func(t *testing.T, f *snapshotFixture){
		"unversioned blob": func(t *testing.T, f *snapshotFixture) {
			putTarBlob(t, f, true, func(tw *tar.Writer) {
				man, _ := json.Marshal(snapshotManifest{SessionID: "sess-old"})
				_ = writeTarBytes(tw, snapManifest, man)
				_ = writeTarBytes(tw, snapScratchPrefix+"notes.txt", []byte("old layout"))
			})
			f.loseRoot(t)
		},
		"gzip blob": func(t *testing.T, f *snapshotFixture) {
			putTarBlob(t, f, false, func(tw *tar.Writer) {
				_ = writeTarBytes(tw, snapScratchPrefix+"notes.txt", []byte("old layout"))
			})
			f.loseRoot(t)
		},
		"warm root that is a checkout": func(t *testing.T, f *snapshotFixture) {
			gitT(t, f.root, "init", "-q")
		},
		"no blob": func(t *testing.T, f *snapshotFixture) {
			f.loseRoot(t)
		},
	}
	for name, setup := range stage {
		t.Run(name, func(t *testing.T) {
			for _, runtime := range []string{domain.ConversationRuntimeNative, domain.ConversationRuntimeSDK} {
				f := newSnapshotFixture(t, "task-refused-"+runtime)
				setup(t, f)
				conv := f.conv("")
				conv.Runtime = runtime
				built := false
				fresh := func(context.Context) (string, error) {
					built = true
					if _, err := os.Stat(filepath.Join(f.root, ".git")); !os.IsNotExist(err) {
						t.Errorf("the fresh build found the older tree still in place (stat err %v)", err)
					}
					return worktree.MakeRunRoot(f.key)
				}
				got, prov, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, conv, f.restorer(), fresh)
				if runtime == domain.ConversationRuntimeSDK {
					if !errors.Is(err, ErrWorkspaceExpired) {
						t.Errorf("SDK resume: err = %v, want ErrWorkspaceExpired", err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("native: ensureWorkspace: %v", err)
				}
				if !built || prov != domain.WorkspaceProvenanceFresh || got != f.root {
					t.Errorf("native: ensureWorkspace = (%q, %q, built=%v), want a fresh %q", got, prov, built, f.root)
				}
				if _, err := os.Stat(filepath.Join(got, worktree.ScratchDir, "notes.txt")); !os.IsNotExist(err) {
					t.Errorf("native: the older blob's scratch was restored (stat err %v)", err)
				}
			}
		})
	}
}

// TestSnapshotWorkspace_BlobFormat: the stored blob is zstd, its first member
// is the manifest with this layout's version and the checkouts it carries —
// which is what lets the layout probe answer from one short read — and
// extracted CI logs, which are one download away, contribute nothing to it.
func TestSnapshotWorkspace_BlobFormat(t *testing.T) {
	f := newSnapshotFixture(t, "task-format")
	f.addCheckout(t, "acme/app", "default")
	logs := strings.Repeat("2026-08-20T12:00:00.0000000Z ##[group]Run go test ./...\n", 60_000)
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, worktree.CILogsDir, "42", "1_build.txt"), logs)
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes", "keep.txt"), "the agent's own intermediate")
	f.snapshot(t, "", domain.ConversationRuntimeNative)

	key := snapshotKey(runmode.LocalDefaultOrgID, f.key)
	rc, err := f.s.Storage().Get(context.Background(), key)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	blob, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.HasPrefix(blob, zstdMagic) {
		t.Errorf("blob starts with % x, want zstd", blob[:4])
	}
	if len(blob) > 16<<10 {
		t.Errorf("blob = %d bytes for a tree whose only bulk is %d bytes of CI logs", len(blob), len(logs))
	}

	zr, err := zstd.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != snapManifest {
		t.Fatalf("first member = %v (err %v), want the manifest", hdr, err)
	}
	var man snapshotManifest
	if err := json.NewDecoder(tr).Decode(&man); err != nil {
		t.Fatal(err)
	}
	want := manifestCheckout{RepoID: "acme/app", Slug: "default", Path: "acme/app/default"}
	if man.LayoutVersion != snapshotLayoutVersion || len(man.Checkouts) != 1 ||
		man.Checkouts[0].RepoID != want.RepoID || man.Checkouts[0].Slug != want.Slug || man.Checkouts[0].Path != want.Path || man.Checkouts[0].Head == "" {
		t.Errorf("manifest = %+v, want layout %d carrying %+v", man, snapshotLayoutVersion, want)
	}
	if !man.CILogsOmitted {
		t.Error("manifest does not record the omitted CI logs; a restore could not explain their absence")
	}
	members := snapshotMembers(t, f.s.Storage(), key)
	for name := range members {
		if strings.HasPrefix(name, snapScratchPrefix+worktree.CILogsDir+"/") {
			t.Errorf("blob carries %q", name)
		}
	}
	if !members[snapScratchPrefix+"notes/keep.txt"] {
		t.Errorf("blob dropped the agent's own scratch (members: %v)", members)
	}

	if p, err := f.s.snapshotLayoutAt(context.Background(), runmode.LocalDefaultOrgID, f.key); err != nil || p != snapshotRestorable {
		t.Errorf("snapshotLayoutAt = %v, %v; want restorable", p, err)
	}
	if p, err := f.s.snapshotLayoutAt(context.Background(), runmode.LocalDefaultOrgID, "no-such-key"); err != nil || p != snapshotAbsent {
		t.Errorf("snapshotLayoutAt(missing) = %v, %v; want absent", p, err)
	}
}

// TestEnsureWorkspace_NoCILogsNoticeWithoutOmittedLogs: the notice explains a
// specific absence, so a tree whose ci-logs held nothing gets none.
func TestEnsureWorkspace_NoCILogsNoticeWithoutOmittedLogs(t *testing.T) {
	f := newSnapshotFixture(t, "task-no-cilogs")
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes.txt"), "scratch note")
	if err := os.MkdirAll(filepath.Join(f.root, worktree.ScratchDir, worktree.CILogsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	f.snapshot(t, "", domain.ConversationRuntimeSDK)
	f.loseRoot(t)

	got, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), f.restorer(), nil)
	if err != nil {
		t.Fatalf("ensureWorkspace: %v", err)
	}
	assertFileContains(t, filepath.Join(got, worktree.ScratchDir, "notes.txt"), "scratch note")
	assertMissing(t, filepath.Join(got, worktree.ScratchDir, worktree.CILogsDir))
}

// TestEnsureWorkspace_TranscriptGuard: a snapshot that captured the session
// transcript restores one the resume guard sees; one taken while the
// transcript was missing rebuilds the tree but leaves the guard to report the
// run unresumable, rather than hand the SDK a --resume it cannot honor.
func TestEnsureWorkspace_TranscriptGuard(t *testing.T) {
	for _, withTranscript := range []bool{true, false} {
		f := newSnapshotFixture(t, "task-transcript")
		const sessionID = "sess-guard"
		writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes.txt"), "scratch survived")
		if withTranscript {
			writeSession(t, f.root, sessionID, `{"type":"summary"}`)
		}
		f.snapshot(t, sessionID, domain.ConversationRuntimeSDK)
		f.loseRoot(t)

		got, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(sessionID), f.restorer(), nil)
		if err != nil {
			t.Fatalf("ensureWorkspace: %v", err)
		}
		assertFileContains(t, filepath.Join(got, worktree.ScratchDir, "notes.txt"), "scratch survived")
		if sessionTranscriptExists(got, sessionID) != withTranscript {
			t.Errorf("transcript captured = %v, but the guard reads %v", withTranscript, !withTranscript)
		}
	}
}

// TestEnsureWorkspace_TruncatedZstdErrors: a blob whose zstd checksum is cut
// short fails the rehydrate before the run root is touched. The tar ends before
// the zstd footer, so only the drain after the last member catches it.
func TestEnsureWorkspace_TruncatedZstdErrors(t *testing.T) {
	f := newSnapshotFixture(t, "task-corrupt")
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "x.log"), "scratch bytes the zstd frame checksum covers")
	f.snapshot(t, "", domain.ConversationRuntimeSDK)
	f.loseRoot(t)

	key := snapshotKey(runmode.LocalDefaultOrgID, f.key)
	rc, err := f.s.Storage().Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := io.ReadAll(rc)
	_ = rc.Close()
	if err := f.s.Storage().Put(context.Background(), key, bytes.NewReader(blob[:len(blob)-1])); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := f.s.ensureWorkspace(context.Background(), runmode.LocalDefaultOrgID, f.conv(""), f.restorer(), nil); err == nil {
		t.Fatal("ensureWorkspace accepted a truncated zstd checksum")
	}
	if _, err := os.Stat(f.root); !os.IsNotExist(err) {
		t.Errorf("the run root was written before the integrity check (stat err %v)", err)
	}
}

// TestWriteSnapshotTar_StreamsStagedCaptureMembers: members the capture child
// staged on disk are streamed into the tar under the checkout's member names.
func TestWriteSnapshotTar_StreamsStagedCaptureMembers(t *testing.T) {
	staging := t.TempDir()
	bundlePath := filepath.Join(staging, worktree.CaptureBundleFile)
	patchPath := filepath.Join(staging, worktree.CapturePatchFile)
	transcriptPath := filepath.Join(staging, worktree.CaptureTranscriptFile)
	writeFile(t, bundlePath, "bundle-bytes")
	writeFile(t, patchPath, "patch-bytes")
	writeFile(t, transcriptPath, "transcript-bytes")

	captured := &capturedSnapshot{
		state: worktree.CapturedState{SessionID: "sess-staged", TranscriptPath: transcriptPath},
		checkouts: []capturedCheckout{{
			snapshotCheckout: snapshotCheckout{repoID: "acme/app", slug: "pr-7", rel: "acme/app/pr-7"},
			state: worktree.CapturedState{
				Delta:      &worktree.GitDelta{Branch: "aa/work", Head: "abc123"},
				BundlePath: bundlePath, PatchPath: patchPath,
			},
		}},
	}
	var blob bytes.Buffer
	if err := writeSnapshotTar(context.Background(), &blob, captured, t.TempDir(), snapshotManifest{}); err != nil {
		t.Fatalf("writeSnapshotTar: %v", err)
	}
	want := map[string]string{
		snapCheckoutsPrefix + "acme/app/pr-7/bundle": "bundle-bytes",
		snapCheckoutsPrefix + "acme/app/pr-7/patch":  "patch-bytes",
		snapSession: "transcript-bytes",
	}
	tr := tar.NewReader(&blob)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if body, ok := want[hdr.Name]; ok {
			if got, _ := io.ReadAll(tr); string(got) != body {
				t.Errorf("%s = %q, want %q", hdr.Name, got, body)
			}
			delete(want, hdr.Name)
		}
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

// TestFailRun_LeavesTheWorkspaceSnapshotToItsOwner: a failure does not delete
// the task's workspace blob. The blueprint's teardown owns that key, and a
// delete from the conversation's failure would take the workspace out from
// under work the failure has not ended.
func TestFailRun_LeavesTheWorkspaceSnapshotToItsOwner(t *testing.T) {
	isolateRunNamespace(t)
	s, database, conversationID, taskID := setupAdvanceFixture(t, "failrun-discard")
	wireBlobStore(t, s)

	ctx := context.Background()
	key := snapshotKey(runmode.LocalDefaultOrgID, taskIDForConversation(t, database, conversationID))
	if err := s.Storage().Put(ctx, key, strings.NewReader("snapshot")); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	s.failConversation(runmode.LocalDefaultOrgID, conversationID, taskID, holderClaimFor(t, s, runmode.LocalDefaultOrgID, conversationID), "event", "boom", domain.ConversationFailureUnclassified)

	if ok, _ := s.Storage().Exists(ctx, key); !ok {
		t.Error("failConversation deleted the task's workspace snapshot; terminateBlueprint owns that blob")
	}
}

// TestSnapshotWorkspace_PhaseSpans pins the span family a slow park is read
// through: the punctual workspace.snapshot root split into capture / archive /
// put children, each carrying the sizes that explain its own duration and the
// runtime whose snapshot it was.
func TestSnapshotWorkspace_PhaseSpans(t *testing.T) {
	read := recordSpans(t)
	f := newSnapshotFixture(t, "task-spans")
	co := f.addCheckout(t, "acme/app", "default")
	dirtyCheckout(t, co)
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes", "build.log"), strings.Repeat("scratch note\n", 200))
	const sessionID = "sess-spans"
	writeSession(t, f.root, sessionID, `{"type":"summary","sid":"spans"}`)
	f.snapshot(t, sessionID, domain.ConversationRuntimeSDK)

	spans := read()
	roots := spansNamed(spans, "workspace.snapshot")
	if len(roots) != 1 {
		t.Fatalf("workspace.snapshot spans = %d, want 1", len(roots))
	}
	root := roots[0]
	total := spanAttr(t, root, "size_bytes").AsInt64()
	if total <= 0 {
		t.Errorf("root size_bytes = %d, want > 0", total)
	}
	for _, name := range []string{"workspace.snapshot.capture", "workspace.snapshot.archive", "workspace.snapshot.put"} {
		phases := spansNamed(spans, name)
		if len(phases) != 1 {
			t.Fatalf("%s spans = %d, want 1", name, len(phases))
		}
		if phases[0].Parent().SpanID() != root.SpanContext().SpanID() {
			t.Errorf("%s does not parent to the workspace.snapshot span", name)
		}
		if got := spanAttr(t, phases[0], "runtime").AsString(); got != domain.ConversationRuntimeSDK {
			t.Errorf("%s runtime = %q, want %q", name, got, domain.ConversationRuntimeSDK)
		}
	}
	capture := spansNamed(spans, "workspace.snapshot.capture")[0]
	for _, key := range []string{"snapshot.bundle_bytes", "snapshot.patch_bytes", "snapshot.transcript_bytes"} {
		if got := spanAttr(t, capture, key).AsInt64(); got <= 0 {
			t.Errorf("capture %s = %d, want > 0 for a tree carrying that member", key, got)
		}
	}
	if got := spanAttr(t, capture, "count").AsInt64(); got != 1 {
		t.Errorf("capture count = %d, want the one checkout", got)
	}
	archive := spansNamed(spans, "workspace.snapshot.archive")[0]
	raw := spanAttr(t, archive, "snapshot.raw_bytes").AsInt64()
	compressed := spanAttr(t, archive, "size_bytes").AsInt64()
	if raw <= compressed || compressed != total {
		t.Errorf("archive raw=%d compressed=%d root=%d; want raw > compressed == root", raw, compressed, total)
	}
	if got := spanAttr(t, spansNamed(spans, "workspace.snapshot.put")[0], "size_bytes").AsInt64(); got != compressed {
		t.Errorf("put size_bytes = %d, want the staged blob's %d", got, compressed)
	}
}

// TestSnapshotWorkspace_PhaseSpans_NoCheckoutsNoSession: a native conversation
// with no checkouts has no delta and no transcript, and those sizes report an
// explicit zero rather than vanishing.
func TestSnapshotWorkspace_PhaseSpans_NoCheckoutsNoSession(t *testing.T) {
	read := recordSpans(t)
	f := newSnapshotFixture(t, "task-spans-native")
	writeFile(t, filepath.Join(f.root, worktree.ScratchDir, "notes.txt"), "scratch note")
	f.snapshot(t, "", domain.ConversationRuntimeNative)

	capture := spansNamed(read(), "workspace.snapshot.capture")
	if len(capture) != 1 {
		t.Fatalf("workspace.snapshot.capture spans = %d, want 1", len(capture))
	}
	for _, key := range []string{"snapshot.bundle_bytes", "snapshot.patch_bytes", "snapshot.transcript_bytes", "count"} {
		if got := spanAttr(t, capture[0], key).AsInt64(); got != 0 {
			t.Errorf("capture %s = %d, want an explicit 0", key, got)
		}
	}
}

// --- fixture ---------------------------------------------------------------

// snapshotFixture is a task's run root with real checkouts beneath it, on a
// spawner with a blob store and an in-memory checkout ledger: what a snapshot
// reads and a restore rebuilds, with no database.
type snapshotFixture struct {
	s      *Spawner
	ledger *checkoutLedger
	key    string
	root   string
	// upstreams maps "owner/repo" to the origin its checkouts were cloned from.
	upstreams map[string]string
}

// fixtureConversation is the conversation every fixture checkout belongs to.
const fixtureConversation = "conv-1"

func newSnapshotFixture(t *testing.T, key string) *snapshotFixture {
	t.Helper()
	isolateRunNamespace(t)
	setupGitTestEnv(t)
	s := newStorageSpawner(t)
	root, err := worktree.MakeRunRoot(key)
	if err != nil {
		t.Fatalf("MakeRunRoot: %v", err)
	}
	t.Cleanup(func() { worktree.RemoveRunRoot(key) })
	return &snapshotFixture{s: s, ledger: s.conversationWorktrees.(*checkoutLedger), key: key, root: root, upstreams: map[string]string{}}
}

// addCheckout builds a checkout of repoID under the root the way the run
// would: slug pr-7 as setup builds a PR run's checkout, any other slug as
// `workspace add` does. It records the checkout's row and returns its path.
func (f *snapshotFixture) addCheckout(t *testing.T, repoID, slug string) string {
	t.Helper()
	owner, repo := parseOwnerRepo(repoID)
	origin := f.upstream(t, repoID)
	var (
		path string
		err  error
	)
	switch slug {
	case "pr-7":
		path, err = worktree.CreateForPRInRoot(context.Background(), owner, repo, origin, origin, "feature", 7, fixtureConversation, f.root)
	case "default":
		path, err = worktree.CreateForCheckoutInRoot(context.Background(), owner, repo, origin, "", f.key, f.root)
	default:
		path, err = worktree.CreateForCheckoutInRoot(context.Background(), owner, repo, origin, strings.TrimPrefix(slug, "ref-"), f.key, f.root)
	}
	if err != nil {
		t.Fatalf("build %s %s: %v", repoID, slug, err)
	}
	f.ledger.add(domain.ConversationWorktree{ConversationID: fixtureConversation, RepoID: repoID, Path: path, Ref: slug})
	return path
}

// upstream is repoID's origin: one commit on main, and pull request #7 whose
// head is the branch feature.
func (f *snapshotFixture) upstream(t *testing.T, repoID string) string {
	t.Helper()
	if origin, ok := f.upstreams[repoID]; ok {
		return origin
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitT(t, "", "init", "-q", "--bare", "-b", "main", origin)
	seed := filepath.Join(t.TempDir(), "seed")
	gitT(t, "", "init", "-q", "-b", "main", seed)
	writeFile(t, filepath.Join(seed, "README.md"), "hello\n")
	gitT(t, seed, "add", "README.md")
	gitT(t, seed, "commit", "-q", "-m", "init")
	gitT(t, seed, "push", "-q", origin, "main")
	writeFile(t, filepath.Join(seed, "feature.txt"), "the pull request's change\n")
	gitT(t, seed, "add", "feature.txt")
	gitT(t, seed, "commit", "-q", "-m", "feature")
	gitT(t, seed, "push", "-q", origin, "HEAD:refs/heads/feature", "HEAD:refs/pull/7/head")
	f.upstreams[repoID] = origin
	return origin
}

// restorer rebuilds the fixture's checkouts from their local origins, reading
// pull request #7 as one whose head is feature on the same origin.
func (f *snapshotFixture) restorer() checkoutRestorer {
	return checkoutRestorer{
		seed: func(_ context.Context, owner, repo string) gitSeed {
			return gitSeed{owner: owner, repo: repo, cloneURL: f.upstreams[owner+"/"+repo]}
		},
		pr: func(_ context.Context, owner, repo string, number int) (*ghclient.PRView, error) {
			origin := f.upstreams[owner+"/"+repo]
			return &ghclient.PRView{Number: number, HeadRef: "feature", BaseRef: "main", CloneURL: origin, SSHURL: origin}, nil
		},
	}
}

// snapshot writes the fixture tree's snapshot under its task key.
func (f *snapshotFixture) snapshot(t *testing.T, sessionID, runtime string) {
	t.Helper()
	if err := f.s.snapshotWorkspace(context.Background(), runmode.LocalDefaultOrgID, fixtureConversation, f.key, "", f.root, sessionID, runtime); err != nil {
		t.Fatalf("snapshotWorkspace: %v", err)
	}
}

// loseRoot removes the run root, as a host loss or a /tmp wipe does. The bares
// survive, as they do on a host that keeps its state root.
func (f *snapshotFixture) loseRoot(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(f.root); err != nil {
		t.Fatalf("remove run root: %v", err)
	}
}

// conv is the claimed conversation a resume rebuilds the fixture's tree for.
// Its recorded path is the root itself, so a rebuild does not re-stamp it.
func (f *snapshotFixture) conv(sessionID string) *domain.Conversation {
	return &domain.Conversation{
		ID: fixtureConversation, ClaimID: "claim-1", TaskID: f.key, WorktreePath: f.root,
		SessionID: sessionID, Runtime: domain.ConversationRuntimeSDK,
	}
}

// dirtyCheckout leaves a checkout the three kinds of work a restore has to
// bring back: an unpushed commit, an uncommitted edit and an untracked file.
func dirtyCheckout(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "committed.txt"), "unpushed commit\n")
	gitT(t, dir, "add", "committed.txt")
	gitT(t, dir, "commit", "-q", "-m", "agent work")
	writeFile(t, filepath.Join(dir, "README.md"), "hello\nuncommitted edit\n")
	writeFile(t, filepath.Join(dir, "untracked.txt"), "untracked file\n")
}

// putTarBlob stores a hand-built blob under the fixture's key: zstd, or the
// gzip an older binary wrote.
func putTarBlob(t *testing.T, f *snapshotFixture, zstdBlob bool, members func(*tar.Writer)) {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	if zstdBlob {
		zw, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		w = zw
	} else {
		w = gzip.NewWriter(&buf)
	}
	tw := tar.NewWriter(w)
	members(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Storage().Put(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, f.key), &buf); err != nil {
		t.Fatal(err)
	}
}

// checkoutLedger is conversation_worktrees in memory: the rows a snapshot reads
// for the task, and the rows a restore records through the claim fence.
type checkoutLedger struct {
	db.ConversationWorktreeStore
	mu       sync.Mutex
	rows     []domain.ConversationWorktree
	recorded []domain.ConversationWorktree
}

func (l *checkoutLedger) add(w domain.ConversationWorktree) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rows = append(l.rows, w)
}

func (l *checkoutLedger) recordedRows() []domain.ConversationWorktree {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.ConversationWorktree(nil), l.recorded...)
}

func (l *checkoutLedger) ListForTaskSystem(context.Context, string, string) ([]domain.ConversationWorktree, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.ConversationWorktree(nil), l.rows...), nil
}

func (l *checkoutLedger) RecordForClaimSystem(_ context.Context, _, _ string, w domain.ConversationWorktree) (domain.ConversationWorktree, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recorded = append(l.recorded, w)
	return w, nil
}

// --- helpers ---------------------------------------------------------------

// newStorageSpawner builds a bare Spawner with a blob store and an in-memory
// checkout ledger — enough for the snapshot/rehydrate path, which on the same
// host touches no other table (the rebuilt root equals the stored
// worktree_path, so nothing re-stamps it).
func newStorageSpawner(t *testing.T) *Spawner {
	t.Helper()
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s := NewSpawner(nil, db.Stores{ConversationWorktrees: &checkoutLedger{}}, nil, nil, "")
	s.SetStorage(blobs)
	return s
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
