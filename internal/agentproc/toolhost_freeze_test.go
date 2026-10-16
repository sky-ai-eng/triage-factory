package agentproc

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

type freezableRun struct {
	deadRun
	calls []string
}

func (r *freezableRun) Pause(context.Context) error  { r.calls = append(r.calls, "pause"); return nil }
func (r *freezableRun) Resume(context.Context) error { r.calls = append(r.calls, "resume"); return nil }

var _ sandbox.Freezer = (*freezableRun)(nil)

func TestToolHostJail_FreezeAndThaw(t *testing.T) {
	run := &freezableRun{}
	jail := &ToolHostJail{run: run, waitDone: make(chan struct{})}
	if err := jail.Freeze(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := jail.Thaw(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 2 || run.calls[0] != "pause" || run.calls[1] != "resume" {
		t.Errorf("calls = %v", run.calls)
	}
}

// A jail whose launcher cannot freeze is an error, never a quiet success: a
// capture that went ahead on it would read a moving tree and call it still.
func TestToolHostJail_FreezeWithoutAFreezerFails(t *testing.T) {
	jail := &ToolHostJail{run: deadRun{}, waitDone: make(chan struct{})}
	if err := jail.Freeze(context.Background()); err == nil {
		t.Error("Freeze on a launcher that cannot freeze succeeded")
	}
}

// An exited jail is as still as a frozen one.
func TestToolHostJail_ExitedJailNeedsNoFreeze(t *testing.T) {
	done := make(chan struct{})
	close(done)
	jail := &ToolHostJail{run: deadRun{}, waitDone: done}
	if err := jail.Freeze(context.Background()); err != nil {
		t.Errorf("Freeze of an exited jail: %v", err)
	}
	if err := jail.Thaw(context.Background()); err != nil {
		t.Errorf("Thaw of an exited jail: %v", err)
	}
}

// Stop returns once the runtime has been reaped, and not before.
func TestToolHostJail_StopWaitsForTheRuntime(t *testing.T) {
	done := make(chan struct{})
	jail := &ToolHostJail{run: deadRun{}, waitDone: done}
	returned := make(chan error, 1)
	go func() { returned <- jail.Stop() }()
	select {
	case <-returned:
		t.Fatal("Stop returned while the runtime was still alive")
	case <-time.After(100 * time.Millisecond):
	}
	close(done)
	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("Stop = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after the runtime exited")
	}
	var nilJail *ToolHostJail
	if err := nilJail.Stop(); err != nil {
		t.Errorf("Stop on nil = %v", err)
	}
}
