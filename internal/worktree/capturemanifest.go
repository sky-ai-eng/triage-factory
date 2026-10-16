package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
)

// CaptureManifest is what one capture jail says about the tree it read, as the
// small JSON message on its stdout. Everything large is in the stream beside
// it, under the member names below. The orchestrator reads it as hostile: the
// jail ran git over files the agent wrote.
type CaptureManifest struct {
	Checkouts []CheckoutCapture `json:"checkouts"`
	// ScratchDigest is a digest of what the scratch walk saw (ScratchDigest),
	// which is all a checkpoint needs of the scratch to tell whether it moved.
	ScratchDigest string `json:"scratch_digest"`
	// ScratchFiles is how many scratch members the stream carries. The stream
	// ends where the file ends, so a stream cut at a member boundary reads as a
	// complete one; the count is what the reader holds it to.
	ScratchFiles int `json:"scratch_files"`
	// CILogsOmitted is the walk's answer to whether a populated ci-logs
	// subtree was left out.
	CILogsOmitted bool `json:"ci_logs_omitted,omitempty"`
	// Unreadable names the scratch entries the walk could not read.
	Unreadable []string `json:"unreadable,omitempty"`
}

// CheckoutCapture is one checkout's capture, in the order the capture was
// asked for them.
type CheckoutCapture struct {
	// Rel is the checkout's path under the run root, as it was named.
	Rel string `json:"rel"`
	// Delta is the checkout's git state, nil when it is no longer a git
	// worktree. The members are in the stream, not on it.
	Delta *GitDelta `json:"delta"`
	// HasBundle and HasPatch say which of the delta's members the stream
	// carries.
	HasBundle bool `json:"has_bundle,omitempty"`
	HasPatch  bool `json:"has_patch,omitempty"`
}

// The members of a capture's stream. Each is named by what it is and where it
// goes in a snapshot, so a member the orchestrator did not ask for has no
// place to land.
const (
	// CaptureScratchPrefix starts every scratch member: scratch/<path under
	// _tfac>.
	CaptureScratchPrefix = "scratch/"
	captureCheckoutsPart = "checkouts/"
)

// CaptureBundleMember and CapturePatchMember name a checkout's members.
func CaptureBundleMember(rel string) string { return captureCheckoutsPart + rel + "/bundle" }
func CapturePatchMember(rel string) string  { return captureCheckoutsPart + rel + "/patch" }

// ScratchDigest digests what a snapshot of wtPath's scratch would carry: each
// file's path, size, modification time and status-change time, in walk order.
// It is the cheap half of telling whether a tree moved, on purpose — the
// scratch is the unbounded part of a workspace, and hashing it would cost what
// compressing it costs. visit, when set, sees each file the walk does.
func ScratchDigest(ctx context.Context, wtPath string, visit func(rel, path string, fi os.FileInfo) error) (digest string, omittedCILogs bool, unreadable []string, err error) {
	h := sha256.New()
	omittedCILogs, unreadable, err = WalkScratch(ctx, wtPath, func(rel, path string, fi os.FileInfo) error {
		for _, p := range []string{"file", rel, strconv.FormatInt(fi.Size(), 10), strconv.FormatInt(fi.ModTime().UnixNano(), 10), scratchChangeStamp(fi)} {
			fmt.Fprintf(h, "%d:%s;", len(p), p)
		}
		if visit != nil {
			return visit(rel, path, fi)
		}
		return nil
	})
	if err != nil {
		return "", false, nil, err
	}
	return hex.EncodeToString(h.Sum(nil)), omittedCILogs, unreadable, nil
}
