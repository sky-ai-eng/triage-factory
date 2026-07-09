package agentproc

import (
	"context"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"
)

// TestLiveRun_CleanupRunsOncePerExitPath pins the load-bearing teardown
// invariant the sandbox-mode flip relies on: readLoop runs the sandbox
// cleanup EXACTLY once — after cmd.Wait — on every way a run can end (a clean
// exit, a crash, an external cancel). A zero-run would leak the
// netns/veth/subnet slot; a double-run would re-Shutdown the proxies and
// re-Close the agenthost daemon (a latent bug even though sandbox.Close is
// itself idempotent). cleanup is a no-op for direct/local runs, so this is
// the unit-level guard on the multi-mode teardown ownership.
//
// readLoop needs a real *exec.Cmd to Wait on, so each case spawns a trivial
// POSIX subprocess (no SDK / runsc) and injects a counting cleanup via the
// struct rather than the sandbox bring-up.
func TestLiveRun_CleanupRunsOncePerExitPath(t *testing.T) {
	// A well-formed terminal result so the clean-exit case exercises the
	// "valid conclusion then process exits 0" path, not just a bare exit.
	resultLine := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1,"num_turns":1,"total_cost_usd":0,"stop_reason":"end_turn","result":"ok"}`

	cases := []struct {
		name   string
		build  func(runCtx context.Context) *exec.Cmd
		cancel bool // external cancel mid-run (the SIGKILL / ctx-cancel path)
	}{
		{
			name: "clean_exit",
			build: func(runCtx context.Context) *exec.Cmd {
				return exec.CommandContext(runCtx, "printf", "%s\n", resultLine)
			},
		},
		{
			name: "crash",
			build: func(runCtx context.Context) *exec.Cmd {
				return exec.CommandContext(runCtx, "sh", "-c", "exit 1")
			},
		},
		{
			name: "cancel",
			build: func(runCtx context.Context) *exec.Cmd {
				return exec.CommandContext(runCtx, "sleep", "30")
			},
			cancel: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			runCtx, cancel := context.WithCancel(context.Background())
			defer cancel()

			cmd := c.build(runCtx)
			proc, err := newExecProc(cmd)
			if err != nil {
				t.Fatalf("newExecProc: %v", err)
			}

			var calls int32
			l := &LiveRun{
				cancel:  cancel,
				cleanup: func() { atomic.AddInt32(&calls, 1) },
				done:    make(chan struct{}),
				ready:   make(chan struct{}),
			}

			if err := proc.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			go l.readLoop(runCtx, proc, NoopSink{}, nil, nil, "t")

			if c.cancel {
				// Let the reader settle into its blocking read, then cancel:
				// exec.CommandContext SIGKILLs the process, stdout EOFs, and
				// readLoop runs through cmd.Wait to cleanup exactly as a crash
				// would.
				time.Sleep(50 * time.Millisecond)
				cancel()
			}

			select {
			case <-l.done:
			case <-time.After(15 * time.Second):
				t.Fatal("readLoop did not finish")
			}

			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("cleanup ran %d times, want exactly 1", got)
			}

			// The once-guard holds against a defensive second teardown call
			// (a future second site, or a belt-and-suspenders Close path):
			// runCleanup is idempotent.
			l.runCleanup()
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("cleanup ran %d times after a second runCleanup, want 1 (once-guard regressed)", got)
			}
		})
	}
}

// TestLiveRun_RunCleanupNilSafe pins that a LiveRun with no cleanup — the
// direct/local path and the bare-struct unit fixtures — tolerates runCleanup
// without panicking, so readLoop's unconditional call is safe everywhere.
func TestLiveRun_RunCleanupNilSafe(t *testing.T) {
	l := &LiveRun{}
	l.runCleanup() // must not panic
}
