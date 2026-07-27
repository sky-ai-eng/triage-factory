package delegate

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// sealedBundleSink is the sidecar half of the supervision channel: it records
// every sealed blob the relay pushes down.
type sealedBundleSink struct {
	mu  sync.Mutex
	got [][]byte
}

func (s *sealedBundleSink) Handle(_ context.Context, kind sidecarproto.Kind, body json.RawMessage) (any, error) {
	if kind != sidecarproto.KindSealedBundle {
		return nil, errors.New("unexpected kind " + string(kind))
	}
	var b sidecarproto.SealedBundleBody
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, b.Sealed)
	return nil, nil
}

func (s *sealedBundleSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.got)
}

func (s *sealedBundleSink) last() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.got) == 0 {
		return nil
	}
	return s.got[len(s.got)-1]
}

// TestNudgeCredentialRelay pins the on-demand half of the credential refresh
// relay, the part a `workspace add` depends on: a nudge pushes the current
// sealed bundle to the sidecar immediately rather than at the next 30s tick,
// reports the outcome to its caller, and stays the single writer of bundles on
// the supervision channel (a nudge that finds nothing new pushes nothing).
func TestNudgeCredentialRelay(t *testing.T) {
	sink := &sealedBundleSink{}
	hostSide, sidecarSide := net.Pipe()
	conn := sidecarproto.New(hostSide, nil)
	peer := sidecarproto.New(sidecarSide, sink)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = peer.Close()
	})

	var mu sync.Mutex
	sealed := []byte("bundle-v1")
	getter := func(context.Context) (int64, []byte, bool, error) {
		mu.Lock()
		defer mu.Unlock()
		return 7, sealed, true, nil
	}

	es := &executorSandbox{stopRelay: make(chan struct{}), credNudge: make(chan credRelayNudge)}
	s := &Spawner{}
	go s.relayCredentialRefreshes("run-1", getter, 7, conn, es.credNudge, es.stopRelay)
	t.Cleanup(func() { close(es.stopRelay) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The tick is 30s away; the nudge must land well inside this test.
	if err := es.nudgeCredentialRelay(ctx); err != nil {
		t.Fatalf("nudge: %v", err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("sidecar received %d bundles after one nudge, want 1", got)
	}
	if string(sink.last()) != "bundle-v1" {
		t.Errorf("sidecar received %q, want bundle-v1", sink.last())
	}

	// Nothing re-sealed since: a second nudge is a no-op, not a re-push.
	if err := es.nudgeCredentialRelay(ctx); err != nil {
		t.Fatalf("second nudge: %v", err)
	}
	if got := sink.count(); got != 1 {
		t.Fatalf("sidecar received %d bundles after an unchanged nudge, want 1", got)
	}

	// The brain re-seals (the `workspace add` case) — the next nudge carries it.
	mu.Lock()
	sealed = []byte("bundle-v2")
	mu.Unlock()
	if err := es.nudgeCredentialRelay(ctx); err != nil {
		t.Fatalf("nudge after re-seal: %v", err)
	}
	if got := sink.count(); got != 2 {
		t.Fatalf("sidecar received %d bundles after a re-seal, want 2", got)
	}
	if string(sink.last()) != "bundle-v2" {
		t.Errorf("sidecar received %q, want bundle-v2", sink.last())
	}
}

// TestNudgeCredentialRelay_CancelStopsTheWork pins the nudge's ctx contract:
// the relay runs the DB read and the sidecar Call under the CALLER's context,
// so cancelling a nudge actually stops that work instead of leaving it running
// for the relay's own step budget while the caller walks away.
func TestNudgeCredentialRelay_CancelStopsTheWork(t *testing.T) {
	hostSide, sidecarSide := net.Pipe()
	conn := sidecarproto.New(hostSide, nil)
	peer := sidecarproto.New(sidecarSide, nil)
	t.Cleanup(func() {
		_ = conn.Close()
		_ = peer.Close()
	})

	// The getter blocks until its ctx dies, then reports which ctx that was.
	gotCancelled := make(chan error, 1)
	getter := func(ctx context.Context) (int64, []byte, bool, error) {
		<-ctx.Done()
		gotCancelled <- ctx.Err()
		return 0, nil, false, ctx.Err()
	}

	es := &executorSandbox{stopRelay: make(chan struct{}), credNudge: make(chan credRelayNudge)}
	s := &Spawner{}
	go s.relayCredentialRefreshes("run-1", getter, 7, conn, es.credNudge, es.stopRelay)
	t.Cleanup(func() { close(es.stopRelay) })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := es.nudgeCredentialRelay(ctx); err == nil {
		t.Fatal("nudge with an expiring ctx returned nil, want the deadline surfaced")
	}
	// credRelayStepTimeout is 20s: without ctx propagation the read would still
	// be running here rather than already cancelled.
	select {
	case err := <-gotCancelled:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("relay work ended with %v, want the caller's DeadlineExceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("relay work outlived the cancelled nudge; its ctx is not the caller's")
	}
}

// TestNudgeCredentialRelay_AfterStopDoesNotBlock pins that a nudge racing
// sandbox teardown reports the relay is gone instead of hanging until its
// caller's deadline — the materialization then fails with a clear error rather
// than sitting out the whole wait window.
func TestNudgeCredentialRelay_AfterStopDoesNotBlock(t *testing.T) {
	es := &executorSandbox{stopRelay: make(chan struct{}), credNudge: make(chan credRelayNudge)}
	close(es.stopRelay)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if err := es.nudgeCredentialRelay(ctx); err == nil {
		t.Fatal("nudge on a stopped relay returned nil, want an error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("nudge on a stopped relay took %s; it must fail fast", elapsed)
	}
}

// TestNudgeCredentialRelay_UnwiredIsNoOp pins the nil-safe shape the curator
// and local paths rely on: no nudge channel means no relay to nudge.
func TestNudgeCredentialRelay_UnwiredIsNoOp(t *testing.T) {
	if err := (*executorSandbox)(nil).nudgeCredentialRelay(context.Background()); err != nil {
		t.Fatalf("nil sandbox nudge = %v, want nil", err)
	}
	if err := (&executorSandbox{}).nudgeCredentialRelay(context.Background()); err != nil {
		t.Fatalf("unwired sandbox nudge = %v, want nil", err)
	}
}
