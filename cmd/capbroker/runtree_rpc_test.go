//go:build linux

package capbroker

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

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

// TestIPCRoundTrip_CaptureRunDelta_LargeDelta pins the one response that
// outgrows the request frame cap: a delta bigger than maxFrameSize (it
// embeds a git bundle + binary patch) must cross the wire intact under
// responseFrameSize rather than being rejected by the 1 MiB request rail.
func TestIPCRoundTrip_CaptureRunDelta_LargeDelta(t *testing.T) {
	big := bytes.Repeat([]byte{0xAB}, 3*maxFrameSize) // 3 MiB > the request cap
	var gotWorktree string
	client := serveTestBroker(t, &fakeOps{
		captureRunDeltaFn: func(ctx context.Context, worktree string) ([]byte, error) {
			gotWorktree = worktree
			return big, nil
		},
	})
	delta, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-3")
	if err != nil {
		t.Fatalf("CaptureRunDelta: %v", err)
	}
	if gotWorktree != "/tmp/tf-runs/run-3" {
		t.Errorf("broker saw worktree %q", gotWorktree)
	}
	if !bytes.Equal(delta, big) {
		t.Errorf("delta corrupted in transit: got %d bytes, want %d identical bytes", len(delta), len(big))
	}
}

// TestIPCRoundTrip_CaptureRunDelta_EmptyMeansNoDelta pins the "not a git
// worktree" shape: a nil/empty broker result arrives as empty bytes, not
// an error — the caller's null-delta handling depends on it.
func TestIPCRoundTrip_CaptureRunDelta_EmptyMeansNoDelta(t *testing.T) {
	client := serveTestBroker(t, &fakeOps{
		captureRunDeltaFn: func(ctx context.Context, worktree string) ([]byte, error) {
			return nil, nil
		},
	})
	delta, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-4")
	if err != nil {
		t.Fatalf("CaptureRunDelta: %v", err)
	}
	if len(delta) != 0 {
		t.Errorf("delta = %d bytes, want empty", len(delta))
	}
}

// TestIPCRoundTrip_CaptureRunDelta_OpaqueBytes pins the §4.2 exception in
// docs/sandbox-security-architecture.md: the capture delta transits the
// broker as opaque bytes it never interprets. A payload that is deliberately
// NOT valid JSON must still round-trip intact — proof that the broker
// base64-forwards the bytes without parsing them. If the wire type were ever
// changed to one the broker's encoder inspects (e.g. json.RawMessage, which
// validates JSON syntax on marshal), this non-JSON payload would fail to
// cross and this test would catch the regression against that invariant.
func TestIPCRoundTrip_CaptureRunDelta_OpaqueBytes(t *testing.T) {
	// Not valid JSON at any offset: a lone brace, raw NULs, invalid UTF-8.
	opaque := []byte{'{', 0x00, 0xff, 0xfe, '"', ':'}
	client := serveTestBroker(t, &fakeOps{
		captureRunDeltaFn: func(ctx context.Context, worktree string) ([]byte, error) {
			return opaque, nil
		},
	})
	delta, err := client.CaptureRunDelta(context.Background(), "/tmp/tf-runs/run-5")
	if err != nil {
		t.Fatalf("CaptureRunDelta: %v", err)
	}
	if !bytes.Equal(delta, opaque) {
		t.Errorf("opaque payload not preserved: got %v, want %v", delta, opaque)
	}
}
