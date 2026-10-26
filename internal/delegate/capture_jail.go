package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// jailedCapture reports whether a snapshot's tree is read inside a capture
// jail rather than in this process.
//
// In multi mode the run tree belongs to the agent: its .git/config and tracked
// .gitattributes are the agent's to write, and the capture's `git add` and
// `git diff` consult them to decide whether to run an external clean, smudge,
// textconv or diff driver. Running those as this process, or as any host
// identity, runs the agent's program on the host kernel with whatever that
// identity can reach. So every multi capture runs in a one-shot gVisor jail
// the cap-broker builds, with no network, the tree mounted read-only, and
// nothing of any run in view but the files it reads. There is no host
// fallback: a capture whose jail cannot launch fails, and the snapshot with it.
//
// Local mode has no sandbox for the agent's own tree — the operator runs their
// own repositories on a machine they trust — so the capture runs in-process.
func jailedCapture() bool {
	return runmode.Current() == runmode.ModeMulti && runtime.GOOS == "linux"
}

// captureWorkspaceGit captures one checkout's git delta in this process, and
// the session transcript when the run has one. The local-mode capture; a
// multi-mode snapshot goes through captureTreeInJail.
func captureWorkspaceGit(ctx context.Context, wtPath, sessionID string) (worktree.CapturedState, error) {
	var state worktree.CapturedState
	delta, err := worktree.CaptureWorkspaceGit(ctx, wtPath)
	if err != nil {
		return worktree.CapturedState{}, err
	}
	state.Delta = delta
	state.SessionID = sessionID
	if data, ok := worktree.ReadClaudeSessionTranscript(wtPath, sessionID); ok {
		state.Transcript = data
	}
	return state, nil
}

// captureViaSandbox is the seam to the brokered capture jail. A package var so
// tests can pin routing without a real broker.
var captureViaSandbox = sandbox.CaptureRunTree

// jailCaptureTreeDir names where an unpacked capture puts the scratch: a
// directory shaped like a run tree, so everything that reads scratch off a tree
// reads it off this one unchanged.
const jailCaptureTreeDir = "tree"

// maxScratchFiles bounds how many scratch files one capture's stream may
// carry. The jail's stream is read as hostile, and a count is the cheapest
// thing to hold it to.
const maxScratchFiles = 1 << 20

// captureTreeInJail captures w's checkouts and scratch in a capture jail,
// unpacks what the jail streamed into a staging directory this process owns,
// and fills captured from it. The jail's output is read as hostile throughout:
// it ran git over files the agent wrote.
func captureTreeInJail(ctx context.Context, w snapshotWrite, captured *capturedSnapshot) error {
	stagingDir, err := prepareCaptureStaging()
	if err != nil {
		return fmt.Errorf("prepare capture staging: %w", err)
	}
	captured.cleanups = append(captured.cleanups, func() { _ = os.RemoveAll(stagingDir) })

	rels := make([]string, len(w.checkouts))
	for i, co := range w.checkouts {
		rels[i] = co.rel
	}
	raw, err := captureViaSandbox(ctx, sandbox.CaptureParams{Root: w.wtPath, Checkouts: rels, StagingDir: stagingDir})
	if err != nil {
		return err
	}
	var man worktree.CaptureManifest
	if err := json.Unmarshal(raw, &man); err != nil {
		return fmt.Errorf("decode capture manifest: %w", err)
	}
	if err := checkCaptureManifest(ctx, man, rels); err != nil {
		return err
	}

	treeDir := filepath.Join(stagingDir, jailCaptureTreeDir)
	got, err := unpackCaptureStream(filepath.Join(stagingDir, sandbox.CaptureStreamFile), stagingDir, treeDir, man)
	if err != nil {
		return err
	}
	for i, co := range w.checkouts {
		cc := man.Checkouts[i]
		state := worktree.CapturedState{Delta: cc.Delta}
		if cc.Delta != nil {
			state.BundlePath, state.PatchPath = got.bundles[i], got.patches[i]
		}
		captured.checkouts = append(captured.checkouts, capturedCheckout{snapshotCheckout: co, state: state})
	}
	captured.scratch = &capturedScratch{root: treeDir, digest: man.ScratchDigest, ciLogsOmitted: man.CILogsOmitted, unreadable: man.Unreadable}
	return nil
}

// checkCaptureManifest requires the manifest to answer exactly the question
// asked: one entry per requested checkout, in order, each saying only what the
// stream can then be held to.
func checkCaptureManifest(ctx context.Context, man worktree.CaptureManifest, rels []string) error {
	if len(man.Checkouts) != len(rels) {
		return fmt.Errorf("capture manifest names %d checkouts, asked for %d", len(man.Checkouts), len(rels))
	}
	for i, cc := range man.Checkouts {
		if cc.Rel != rels[i] {
			return fmt.Errorf("capture manifest entry %d is %q, asked for %q", i, cc.Rel, rels[i])
		}
		if cc.Delta == nil && (cc.HasBundle || cc.HasPatch) {
			return fmt.Errorf("capture manifest gives %s members but no git state", cc.Rel)
		}
		if cc.Delta != nil && (len(cc.Delta.Bundle) != 0 || len(cc.Delta.Patch) != 0) {
			return fmt.Errorf("capture manifest carries buffered members for %s", cc.Rel)
		}
		// The delta is stored and restored later, possibly on another executor,
		// where a malformed HEAD or branch name fails the restore of work the
		// snapshot had already replaced the last good one with. It is held to
		// the rule the restore applies, here.
		if cc.Delta != nil {
			if err := worktree.ValidateCapturedDelta(ctx, cc.Delta); err != nil {
				return fmt.Errorf("capture manifest for %s: %w", cc.Rel, err)
			}
		}
	}
	return nil
}

type unpackedCapture struct {
	bundles, patches map[int]string
}

// unpackCaptureStream reads the jail's stream into stagingDir: each checkout's
// bundle and patch as files, and every scratch file under treeDir/_tfac. A
// member this process did not ask for, one named outside the places members
// go, one the manifest did not announce, or a stream cut short is a failed
// capture, never a partial one.
func unpackCaptureStream(streamPath, stagingDir, treeDir string, man worktree.CaptureManifest) (unpackedCapture, error) {
	f, err := os.OpenFile(streamPath, os.O_RDWR, 0)
	if err != nil {
		return unpackedCapture{}, fmt.Errorf("open capture stream: %w", err)
	}
	defer func() { _ = f.Close() }()

	index := map[string]struct {
		i      int
		bundle bool
	}{}
	for i, cc := range man.Checkouts {
		if cc.HasBundle {
			index[worktree.CaptureBundleMember(cc.Rel)] = struct {
				i      int
				bundle bool
			}{i, true}
		}
		if cc.HasPatch {
			index[worktree.CapturePatchMember(cc.Rel)] = struct {
				i      int
				bundle bool
			}{i, false}
		}
	}
	out := unpackedCapture{bundles: map[int]string{}, patches: map[int]string{}}
	scratchFiles := 0
	sr := sandbox.NewCaptureStreamReader(newPunchingReader(f))
	for {
		name, body, err := sr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return unpackedCapture{}, err
		}
		switch {
		case strings.HasPrefix(name, worktree.CaptureScratchPrefix):
			rel := strings.TrimPrefix(name, worktree.CaptureScratchPrefix)
			if !validScratchRel(rel) {
				return unpackedCapture{}, fmt.Errorf("capture stream names a scratch member outside the scratch: %q", name)
			}
			// A subtree that comes back by its own route is never in a
			// snapshot. One in the stream was not put there by the walk, and
			// carrying ci-logs would also restore without the notice that
			// says logs were left out.
			if worktree.ScratchExcluded(rel) {
				return unpackedCapture{}, fmt.Errorf("capture stream carries a scratch member from an excluded directory: %q", name)
			}
			if scratchFiles++; scratchFiles > maxScratchFiles {
				return unpackedCapture{}, fmt.Errorf("capture stream carries more than %d scratch files", maxScratchFiles)
			}
			dest := filepath.Join(treeDir, worktree.ScratchDir, filepath.FromSlash(rel))
			if err := writeStaged(dest, body); err != nil {
				return unpackedCapture{}, err
			}
		default:
			want, ok := index[name]
			if !ok {
				return unpackedCapture{}, fmt.Errorf("capture stream carries a member nothing asked for: %q", name)
			}
			dest := filepath.Join(stagingDir, "co"+strconv.Itoa(want.i)+map[bool]string{true: ".bundle", false: ".patch"}[want.bundle])
			if err := writeStaged(dest, body); err != nil {
				return unpackedCapture{}, err
			}
			target := out.patches
			if want.bundle {
				target = out.bundles
			}
			if _, dup := target[want.i]; dup {
				return unpackedCapture{}, fmt.Errorf("capture stream carries %q twice", name)
			}
			target[want.i] = dest
		}
	}
	if scratchFiles != man.ScratchFiles {
		return unpackedCapture{}, fmt.Errorf("capture stream carries %d scratch files, the manifest announced %d", scratchFiles, man.ScratchFiles)
	}
	for i, cc := range man.Checkouts {
		if cc.HasBundle && out.bundles[i] == "" || cc.HasPatch && out.patches[i] == "" {
			return unpackedCapture{}, fmt.Errorf("capture stream is missing a member the manifest announced for %s", cc.Rel)
		}
	}
	return out, nil
}

// validScratchRel is the one shape a scratch member's name may take: a plain
// relative path that stays inside the scratch, in the cleaned form the walk
// writes.
func validScratchRel(rel string) bool {
	return rel != "" && path.Clean(rel) == rel && !path.IsAbs(rel) && rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") && !strings.ContainsRune(rel, 0)
}

// writeStaged writes body to a new file at dest, creating its parents. It
// never replaces an existing file, so a stream that names a member twice, or
// names one under another's path, fails rather than overwriting.
func writeStaged(dest string, body io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return fmt.Errorf("stage capture member: %w", err)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("capture stream carries a member twice: %s", filepath.Base(dest))
		}
		return fmt.Errorf("stage capture member: %w", err)
	}
	if _, err := io.Copy(f, body); err != nil {
		_ = f.Close()
		return fmt.Errorf("stage capture member: %w", err)
	}
	return f.Close()
}

// prepareCaptureStaging makes the directory a capture's stream is written
// through: owner-only, holding the one pre-created owner-only file the broker
// validates and hands the jail as its only writable output. It is in the runs
// base, as the archive staged from it is (stageSnapshotArchive), and for the
// same reason: the stream is the tree's whole delta.
func prepareCaptureStaging() (string, error) {
	base, err := worktree.EnsureRunsBase()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, ".capture-*")
	if err != nil {
		return "", err
	}
	clean := true
	defer func() {
		if clean {
			_ = os.RemoveAll(dir)
		}
	}()
	member := filepath.Join(dir, sandbox.CaptureStreamFile)
	f, err := os.OpenFile(member, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(member, 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	clean = false
	return dir, nil
}
