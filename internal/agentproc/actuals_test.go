package agentproc

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// fakeProc is a runProc with no subprocess behind it: a scripted stdout, a
// scripted exit, and scripted exit-time cgroup actuals. It lets the teardown
// stamp be driven without a jail (which needs multi mode, Linux, runsc and
// capabilities).
type fakeProc struct {
	stdout  string
	waitErr error
	actuals sandbox.RunActuals
}

func (p *fakeProc) Start() error                { return nil }
func (p *fakeProc) Stdin() io.WriteCloser       { return nopWriteCloser{} }
func (p *fakeProc) Stdout() io.Reader           { return strings.NewReader(p.stdout) }
func (p *fakeProc) Stderr() string              { return "" }
func (p *fakeProc) Wait() error                 { return p.waitErr }
func (p *fakeProc) OOMKilled() bool             { return false }
func (p *fakeProc) Actuals() sandbox.RunActuals { return p.actuals }

// measured is the actuals a real sandboxed run would hand back.
func measured() sandbox.RunActuals {
	peak, cpu := 640, int64(3_100_000)
	return sandbox.RunActuals{PeakMemMB: &peak, CPUUsec: &cpu}
}

type stampCall struct {
	orgID, claimID string
	actuals        sandbox.RunActuals
}

// TestRecordSandboxActuals_StampsTheEngagement pins the write: what the run
// consumed lands against the claim that paid for it, under the org the run
// belongs to.
func TestRecordSandboxActuals_StampsTheEngagement(t *testing.T) {
	var got []stampCall
	opts := RunOptions{
		OrgID:   "org-1",
		ClaimID: "claim-1",
		RecordSandboxActuals: func(_ context.Context, orgID, claimID string, a sandbox.RunActuals) error {
			got = append(got, stampCall{orgID, claimID, a})
			return nil
		},
	}
	recordSandboxActuals(context.Background(), opts, &fakeProc{actuals: measured()})

	if len(got) != 1 {
		t.Fatalf("recorder called %d times, want 1", len(got))
	}
	if got[0].orgID != "org-1" || got[0].claimID != "claim-1" {
		t.Errorf("stamped (org=%q, claim=%q), want (org-1, claim-1)", got[0].orgID, got[0].claimID)
	}
	if got[0].actuals.PeakMemMB == nil || *got[0].actuals.PeakMemMB != 640 {
		t.Errorf("peak = %v, want 640", got[0].actuals.PeakMemMB)
	}
	if got[0].actuals.CPUUsec == nil || *got[0].actuals.CPUUsec != 3_100_000 {
		t.Errorf("cpu = %v, want 3100000", got[0].actuals.CPUUsec)
	}
}

// TestRecordSandboxActuals_SkipsWhenNothingToRecord covers the three no-op
// shapes. The unmeasured case is the load-bearing one: a local run holds no
// cgroup, and writing its empty actuals would record a free run rather than
// leaving the columns NULL.
func TestRecordSandboxActuals_SkipsWhenNothingToRecord(t *testing.T) {
	cases := map[string]struct {
		opts RunOptions
		proc *fakeProc
	}{
		"no claim to attribute (system job)": {
			opts: RunOptions{OrgID: "org-1"},
			proc: &fakeProc{actuals: measured()},
		},
		"nothing measured (local/direct path)": {
			opts: RunOptions{OrgID: "org-1", ClaimID: "claim-1"},
			proc: &fakeProc{},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			called := false
			opts := c.opts
			opts.RecordSandboxActuals = func(context.Context, string, string, sandbox.RunActuals) error {
				called = true
				return nil
			}
			recordSandboxActuals(context.Background(), opts, c.proc)
			if called {
				t.Error("recorder ran; want no write at all")
			}
		})
	}

	// No recorder wired at all: must not panic on the nil func.
	recordSandboxActuals(context.Background(), RunOptions{ClaimID: "claim-1"}, &fakeProc{actuals: measured()})
}

// TestRecordSandboxActuals_DetachesFromACancelledRun pins that the stamp
// still happens for a cancelled run — teardown is exactly when the run's own
// context is already dead, and a cancelled run consumed real resources.
func TestRecordSandboxActuals_DetachesFromACancelledRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stampCtxErr error
	called := false
	recordSandboxActuals(ctx, RunOptions{
		OrgID:   "org-1",
		ClaimID: "claim-1",
		RecordSandboxActuals: func(c context.Context, _, _ string, _ sandbox.RunActuals) error {
			called, stampCtxErr = true, c.Err()
			return nil
		},
	}, &fakeProc{actuals: measured()})

	if !called {
		t.Fatal("recorder never ran for a cancelled run")
	}
	if stampCtxErr != nil {
		t.Errorf("stamp context carried %v; want a live detached context", stampCtxErr)
	}
}

// TestRecordSandboxActuals_WriteFailureIsSwallowed pins the best-effort
// contract: accounting must never become a way for a run to fail.
func TestRecordSandboxActuals_WriteFailureIsSwallowed(t *testing.T) {
	recordSandboxActuals(context.Background(), RunOptions{
		OrgID:   "org-1",
		ClaimID: "claim-1",
		RecordSandboxActuals: func(context.Context, string, string, sandbox.RunActuals) error {
			return errors.New("database is on fire")
		},
	}, &fakeProc{actuals: measured()})
	// Reaching here without a panic is the assertion — the helper returns
	// normally and the caller's run proceeds.
}

// TestLiveRun_StampsActualsAtTeardown drives the live reader loop with a
// fake process to prove the interactive path (every delegated run) stamps
// once the runtime has exited, not just the one-shot path.
func TestLiveRun_StampsActualsAtTeardown(t *testing.T) {
	stamped := make(chan stampCall, 1)
	opts := RunOptions{
		OrgID:   "org-1",
		ClaimID: "claim-live",
		TraceID: "run-live",
		RecordSandboxActuals: func(_ context.Context, orgID, claimID string, a sandbox.RunActuals) error {
			stamped <- stampCall{orgID, claimID, a}
			return nil
		},
	}
	l := &LiveRun{
		stdin:  nopWriteCloser{},
		cancel: func() {},
		done:   make(chan struct{}),
		ready:  make(chan struct{}),
	}
	go l.readLoop(context.Background(), opts, &fakeProc{actuals: measured()}, NoopSink{}, nil)

	select {
	case <-l.done:
	case <-time.After(5 * time.Second):
		t.Fatal("readLoop did not finish")
	}
	select {
	case got := <-stamped:
		if got.claimID != "claim-live" || got.actuals.PeakMemMB == nil || got.actuals.CPUUsec == nil {
			t.Errorf("live teardown stamped %+v, want claim-live with both actuals", got)
		}
	default:
		t.Error("live teardown recorded no actuals")
	}
}
