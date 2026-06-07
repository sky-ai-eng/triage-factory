package agentproc

import (
	"testing"
	"time"
)

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
