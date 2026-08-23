package agentproc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestRunInteractive_RefusesMultiMode mirrors TestRun_RefusesMultiMode for
// the streaming-input entry point: multi mode must fail closed with
// errSDKLoopInMultiMode before spawning anything, never fall through to an
// unsandboxed direct spawn on the host.
func TestRunInteractive_RefusesMultiMode(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	lr, err := RunInteractive(context.Background(), RunOptions{}, nil, nil)
	if !errors.Is(err, errSDKLoopInMultiMode) {
		t.Fatalf("RunInteractive() in multi mode err = %v, want errSDKLoopInMultiMode", err)
	}
	if lr != nil {
		t.Errorf("RunInteractive() in multi mode returned a non-nil LiveRun, want nil (nothing spawned)")
	}
}

// nopWriteCloser stands in for the wrapper's stdin pipe in unit tests
// that don't spawn a subprocess.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// TestLiveRunClose_BoundedWhenReaderWedged guards the close-after-SIGKILL
// path: if the reader goroutine is stuck (e.g. blocked in a Sink or
// PermissionHandler), `done` never closes, but Close must still return —
// within drainTimeout+killTimeout — rather than hang the caller forever.
func TestLiveRunClose_BoundedWhenReaderWedged(t *testing.T) {
	canceled := make(chan struct{})
	lr := &LiveRun{
		stdin:        nopWriteCloser{},
		cancel:       func() { close(canceled) },
		done:         make(chan struct{}), // never closed → reader "wedged"
		ready:        make(chan struct{}),
		drainTimeout: 20 * time.Millisecond,
		killTimeout:  20 * time.Millisecond,
	}

	start := time.Now()
	err := lr.Close()
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error when the reader never finishes")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Close took %v; expected it to return promptly under the bounds", elapsed)
	}
	// Drain timeout must have escalated to SIGKILL (cancel).
	select {
	case <-canceled:
	default:
		t.Error("expected Close to cancel (SIGKILL) on drain timeout")
	}
}

// TestLiveRunClose_ReturnsErrWhenDrained pins the happy path: when the
// reader finishes during the drain window, Close returns the terminal
// error (here nil) without escalating to SIGKILL.
func TestLiveRunClose_ReturnsErrWhenDrained(t *testing.T) {
	canceled := false
	lr := &LiveRun{
		stdin:        nopWriteCloser{},
		cancel:       func() { canceled = true },
		done:         make(chan struct{}),
		ready:        make(chan struct{}),
		drainTimeout: time.Second,
		killTimeout:  time.Second,
	}
	// Reader finishes promptly after the end write.
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(lr.done)
	}()

	if err := lr.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil", err)
	}
	if canceled {
		t.Error("Close should not SIGKILL when the run drains gracefully")
	}
}

// TestConsumeStreamInteractive_InterruptMarksResult pins the reader-side
// interrupt labeling: the wrapper's control/interrupted ack marks the NEXT
// result as Interrupted (the SDK has no native subtype for interrupts — the
// turn is wire-labeled is_error/error_during_execution, shape-identical to a
// real runtime error), and only that one — a later result is unmarked. The
// merged accounting result keeps the flag sticky (MergeResult), recording
// that an interrupt happened somewhere in the conversation.
func TestConsumeStreamInteractive_InterruptMarksResult(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"control","subtype":"interrupted"}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"num_turns":1}`,
		`{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"ok"}`,
	}, "\n") + "\n"

	var got []*Result
	l := &LiveRun{ready: make(chan struct{}), done: make(chan struct{})}
	merged, err := l.consumeStreamInteractive(strings.NewReader(input), NoopSink{}, NewStreamState(), nil, func(r *Result) { got = append(got, r) }, "t")
	if err != nil {
		t.Fatalf("consumeStreamInteractive: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 per-turn results, got %d", len(got))
	}
	if !got[0].Interrupted {
		t.Error("the result after control/interrupted must be marked Interrupted")
	}
	if got[1].Interrupted {
		t.Error("a later result must not inherit the interrupt mark")
	}
	if merged == nil || !merged.Interrupted {
		t.Error("merged accounting result should keep Interrupted sticky")
	}
}
