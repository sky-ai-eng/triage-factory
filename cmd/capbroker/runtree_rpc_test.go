//go:build linux

package capbroker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// withStubCaptureRunDeltaTo replaces the broker's real (uid-drop, netns)
// capture with a stand-in that writes directly to the caller-supplied
// *os.File — exercising the RPC/socket-passthrough wiring without needing
// root.
func withStubCaptureRunDeltaTo(t *testing.T, fn func(ctx context.Context, worktree string, stdout *os.File) (string, error)) {
	t.Helper()
	orig := captureRunDeltaTo
	captureRunDeltaTo = fn
	t.Cleanup(func() { captureRunDeltaTo = orig })
}

// withTempCaptureSocketDir redirects the per-capture stdout socket
// directory off the root-only production /run/tf into a writable temp dir.
// Deliberately NOT t.TempDir(): that embeds the (sometimes long) test
// function name in the path, and a unix socket's sun_path is capped at
// ~108 bytes on Linux — a long test name plus the capture-<hex>.sock
// filename can push bind() past that limit. os.MkdirTemp keeps the path
// short and fixed-length regardless of the test name.
func withTempCaptureSocketDir(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "tfcap")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	orig := captureSocketDir
	captureSocketDir = dir
	t.Cleanup(func() { captureSocketDir = orig })
}

// TestIPCRoundTrip_ChownRunTree pins the wire shape of the run-tree
// hand-off: root and subpath arrive broker-side verbatim.
func TestIPCRoundTrip_ChownRunTree(t *testing.T) {
	var gotRoot, gotSubpath string
	client := serveTestBroker(t, &fakeOps{
		chownRunTreeFn: func(ctx context.Context, root, subpath string) error {
			gotRoot, gotSubpath = root, subpath
			return nil
		},
	})
	if err := client.ChownRunTree(context.Background(), "/tmp/tf-runs/run-1", "owner/repo"); err != nil {
		t.Fatalf("ChownRunTree: %v", err)
	}
	if gotRoot != "/tmp/tf-runs/run-1" || gotSubpath != "owner/repo" {
		t.Errorf("broker saw root=%q subpath=%q", gotRoot, gotSubpath)
	}
}

// TestIPCRoundTrip_RemoveRunTree pins the removal RPC, including error
// propagation — a broker-side validation rejection must surface verbatim
// to the caller, not be silently swallowed into a "removed" no-op.
func TestIPCRoundTrip_RemoveRunTree(t *testing.T) {
	var gotPath string
	client := serveTestBroker(t, &fakeOps{
		removeRunTreeFn: func(ctx context.Context, path string) error {
			gotPath = path
			return nil
		},
	})
	if err := client.RemoveRunTree(context.Background(), "/tmp/tf-runs/run-2"); err != nil {
		t.Fatalf("RemoveRunTree: %v", err)
	}
	if gotPath != "/tmp/tf-runs/run-2" {
		t.Errorf("broker saw path %q", gotPath)
	}

	rejecting := serveTestBroker(t, &fakeOps{
		removeRunTreeFn: func(ctx context.Context, path string) error {
			return errors.New("sandbox: remove run tree: /etc is owned by uid 0, not a run-tree owner")
		},
	})
	err := rejecting.RemoveRunTree(context.Background(), "/etc")
	if err == nil {
		t.Fatal("expected the broker-side rejection to propagate")
	}
	if err.Error() != "sandbox: remove run tree: /etc is owned by uid 0, not a run-tree owner" {
		t.Errorf("error = %q, want the broker's message verbatim", err.Error())
	}
}

// TestIPCRoundTrip_CaptureRunDelta_StreamsByteIdentical is the v2 successor
// to the pre-v2 TestIPCRoundTrip_CaptureRunDelta_OpaqueBytes: it pins both
// "never interpreted" and "never re-encoded" at once by round-tripping a
// deliberately non-JSON payload (a lone brace, raw NULs, invalid UTF-8)
// byte-identical through the real fd-passthrough plumbing — a stubbed
// capture, but a real broker, a real dial, and a real accepted socket. Since
// this data no longer rides a JSON response field at all (it streams over a
// raw socket), there is no encoder left to accidentally validate or mangle
// it — this test is the structural proof of that.
func TestIPCRoundTrip_CaptureRunDelta_StreamsByteIdentical(t *testing.T) {
	withTempCaptureSocketDir(t)
	opaque := []byte{'{', 0x00, 0xff, 0xfe, '"', ':'}
	var gotWorktree string
	withStubCaptureRunDeltaTo(t, func(ctx context.Context, worktree string, stdout *os.File) (string, error) {
		gotWorktree = worktree
		_, err := stdout.Write(opaque)
		_ = stdout.Close()
		return "", err
	})
	client := serveTestBroker(t, &fakeOps{})

	delta, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-5")
	if err != nil {
		t.Fatalf("CaptureRunDelta: %v", err)
	}
	if gotWorktree != "/tmp/tf-runs/run-5" {
		t.Errorf("broker saw worktree %q", gotWorktree)
	}
	if !bytes.Equal(delta, opaque) {
		t.Errorf("payload not preserved byte-identical: got %v, want %v", delta, opaque)
	}
}

// TestIPCRoundTrip_CaptureRunDelta_LargeDelta pins that a delta bigger than
// maxFrameSize (it can embed a git bundle + binary patch) still crosses
// intact — via the streamed socket, not an RPC frame. Unlike the pre-v2
// shape there is no larger response-frame cap to fall back on; the point of
// this change is that this data never rides an RPC frame at all.
func TestIPCRoundTrip_CaptureRunDelta_LargeDelta(t *testing.T) {
	withTempCaptureSocketDir(t)
	big := bytes.Repeat([]byte{0xAB}, 3*maxFrameSize) // 3 MiB > the RPC frame cap
	withStubCaptureRunDeltaTo(t, func(ctx context.Context, worktree string, stdout *os.File) (string, error) {
		_, err := stdout.Write(big)
		_ = stdout.Close()
		return "", err
	})
	client := serveTestBroker(t, &fakeOps{})

	delta, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-3")
	if err != nil {
		t.Fatalf("CaptureRunDelta: %v", err)
	}
	if !bytes.Equal(delta, big) {
		t.Errorf("delta corrupted in transit: got %d bytes, want %d identical bytes", len(delta), len(big))
	}
}

// TestIPCRoundTrip_CaptureRunDelta_EmptyMeansNoDelta pins the "not a git
// worktree" shape: a capture that writes nothing arrives as empty bytes,
// not an error — the caller's null-delta handling depends on it.
func TestIPCRoundTrip_CaptureRunDelta_EmptyMeansNoDelta(t *testing.T) {
	withTempCaptureSocketDir(t)
	withStubCaptureRunDeltaTo(t, func(ctx context.Context, worktree string, stdout *os.File) (string, error) {
		_ = stdout.Close()
		return "", nil
	})
	client := serveTestBroker(t, &fakeOps{})

	delta, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-4")
	if err != nil {
		t.Fatalf("CaptureRunDelta: %v", err)
	}
	if len(delta) != 0 {
		t.Errorf("delta = %d bytes, want empty", len(delta))
	}
}

// TestIPCRoundTrip_CaptureRunDelta_OverCapFailsCleanly pins the loss
// contract: a stream exceeding sandbox.CaptureMaxBytes fails the capture
// cleanly (the park degrades to snapshot-less) instead of buffering
// arbitrarily. Shrinks the cap so the test doesn't need to push real
// hundreds of MiB to exercise it.
func TestIPCRoundTrip_CaptureRunDelta_OverCapFailsCleanly(t *testing.T) {
	withTempCaptureSocketDir(t)
	origCap := sandbox.CaptureMaxBytes
	sandbox.CaptureMaxBytes = 1024
	t.Cleanup(func() { sandbox.CaptureMaxBytes = origCap })

	withStubCaptureRunDeltaTo(t, func(ctx context.Context, worktree string, stdout *os.File) (string, error) {
		defer stdout.Close()
		_, err := stdout.Write(bytes.Repeat([]byte{0x01}, 4096)) // well past the shrunk cap
		return "", err
	})
	client := serveTestBroker(t, &fakeOps{})

	if _, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-cap"); err == nil {
		t.Fatal("expected an over-cap error, got nil")
	}
}

// TestIPCRoundTrip_CaptureRunDelta_WaitsForStreamAfterRPCSuccess pins the
// both-signals-required ordering: a successful RPC response alone must not
// be enough to return bytes — the client also has to see the stream's EOF.
// Shrinks captureTimeout so the test doesn't wait the full 5-minute
// production budget. The stub reports RPC success immediately but holds
// stdout open (via a goroutine gated on release) past that shrunk budget,
// so a client that returned on RPC success alone would return well under
// captureTimeout — this asserts on elapsed time to catch that.
func TestIPCRoundTrip_CaptureRunDelta_WaitsForStreamAfterRPCSuccess(t *testing.T) {
	withTempCaptureSocketDir(t)
	origTimeout := captureTimeout
	captureTimeout = 200 * time.Millisecond
	t.Cleanup(func() { captureTimeout = origTimeout })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	withStubCaptureRunDeltaTo(t, func(ctx context.Context, worktree string, stdout *os.File) (string, error) {
		go func() {
			<-release
			_ = stdout.Close()
		}()
		return "", nil
	})
	client := serveTestBroker(t, &fakeOps{})

	start := time.Now()
	_, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-wait")
	if err == nil {
		t.Fatal("expected a timeout waiting for the stream after RPC success, got nil")
	}
	if elapsed := time.Since(start); elapsed < captureTimeout {
		t.Errorf("returned in %s, under the %s stream-wait budget — it did not actually wait for the stream", elapsed, captureTimeout)
	}
}

// TestIPCRoundTrip_CaptureRunDelta_ClosesStreamPromptlyOnRPCFailure pins
// that an RPC failure closes an already-accepted stream connection right
// away rather than leaving it to read (and buffer, toward
// sandbox.CaptureMaxBytes) a result nobody will use. The stub never closes
// its end of the stream itself — it blocks reading from it in the
// background, so this test's success signal (the read unblocking) can only
// come from the CLIENT side closing its accepted conn. A generous
// captureTimeout (unmodified from production) with a much shorter assertion
// window proves this happens promptly, not merely "eventually, on its own
// timeout".
func TestIPCRoundTrip_CaptureRunDelta_ClosesStreamPromptlyOnRPCFailure(t *testing.T) {
	withTempCaptureSocketDir(t)

	peerClosed := make(chan struct{})
	withStubCaptureRunDeltaTo(t, func(ctx context.Context, worktree string, stdout *os.File) (string, error) {
		t.Cleanup(func() { _ = stdout.Close() })
		go func() {
			buf := make([]byte, 16)
			_, _ = stdout.Read(buf) // unblocks once the client closes its end
			close(peerClosed)
		}()
		return "", errors.New("simulated child failure")
	})
	client := serveTestBroker(t, &fakeOps{})

	if _, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-fail"); err == nil {
		t.Fatal("expected the simulated RPC failure to propagate")
	}

	select {
	case <-peerClosed:
	case <-time.After(1 * time.Second):
		t.Fatal("client did not close the accepted stream promptly after an RPC failure")
	}
}

// TestIPCRoundTrip_CaptureRunDelta_ErrorHasNoEmptyStderrSuffix pins that a
// capture failure with no stderr output doesn't surface an error string
// with an always-appended, now-empty "(stderr: )" suffix.
func TestIPCRoundTrip_CaptureRunDelta_ErrorHasNoEmptyStderrSuffix(t *testing.T) {
	withTempCaptureSocketDir(t)
	withStubCaptureRunDeltaTo(t, func(ctx context.Context, worktree string, stdout *os.File) (string, error) {
		_ = stdout.Close()
		return "", errors.New("simulated capture failure")
	})
	client := serveTestBroker(t, &fakeOps{})

	_, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-errfmt")
	if err == nil {
		t.Fatal("expected the simulated failure to propagate")
	}
	if strings.Contains(err.Error(), "(stderr: )") {
		t.Errorf("error = %q, want no empty-stderr suffix when stderrTail is empty", err.Error())
	}
}
