package runsidecar

import (
	"os"
	"testing"
	"time"
)

// TestRun_ExitsCleanlyOnStdinEOF pins the Phase 1 idle contract: with no
// credential/proxy logic yet, the sidecar's only job is to block until its
// stdio channel closes (the orchestrator-side teardown path) and then exit
// with no error — never hang, never error on a graceful close.
func TestRun_ExitsCleanlyOnStdinEOF(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = origStdin })

	if err := w.Close(); err != nil { // immediate EOF on the read side
		t.Fatalf("close write end: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- run([]string{"--container-id", "test-sc"}) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run() = %v, want nil on a clean stdin EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("run() did not return after stdin EOF")
	}
}

// TestRun_RejectsUnknownFlag pins that flag.ContinueOnError surfaces a
// parse failure rather than the subcommand silently ignoring it.
func TestRun_RejectsUnknownFlag(t *testing.T) {
	if err := run([]string{"--not-a-real-flag"}); err == nil {
		t.Error("run() with an unknown flag = nil, want an error")
	}
}
