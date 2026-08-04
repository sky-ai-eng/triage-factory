package agentproc

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// deadRun is a LaunchedRun that never was: it stands in for a jail whose
// runtime exited before the in-jail tool host could dial back, which is what
// a runsc create failure looks like from this side. Only Stderr and the
// lifecycle no-ops are reached on that path.
type deadRun struct{ stderr string }

func (deadRun) Start() error                { return nil }
func (deadRun) Stdin() io.WriteCloser       { return nil }
func (deadRun) Stdout() io.Reader           { return nil }
func (r deadRun) Stderr() string            { return r.stderr }
func (deadRun) Wait() error                 { return errors.New("exit status 128") }
func (deadRun) OOMKilled() bool             { return false }
func (deadRun) Actuals() sandbox.RunActuals { return sandbox.RunActuals{} }
func (deadRun) Pid() int                    { return 0 }
func (deadRun) Close() error                { return nil }

// Compile-time proof the stand-in still satisfies what the jail holds.
var _ sandbox.LaunchedRun = deadRun{}

// TestToolHostJail_AcceptReportsJailStderr pins the diagnosis this error is
// supposed to give. The jail's stderr is the only place runsc's own create
// failure is written, so an Accept that reports a dead jail without it says
// nothing a reader can act on — which is exactly how it rendered on every
// brokered launch before the broker relayed its tail.
func TestToolHostJail_AcceptReportsJailStderr(t *testing.T) {
	const runscErr = `creating container: container with id "tf-conv-0" already exists`

	ln, err := net.Listen("unix", filepath.Join(t.TempDir(), "tools.sock"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	// The watcher's post-conditions, already satisfied: the runtime exited
	// and nothing ever dialed in.
	waitDone := make(chan struct{})
	close(waitDone)
	jail := &ToolHostJail{
		listener: ln,
		run:      deadRun{stderr: runscErr},
		waitDone: waitDone,
		waitErr:  errors.New("exit status 128"),
	}

	_, err = jail.Accept(context.Background(), 5*time.Second)
	if err == nil {
		t.Fatal("Accept returned a connection for a jail that already exited")
	}
	if !strings.Contains(err.Error(), "exited before connecting") {
		t.Errorf("error = %v, want the fail-fast exited-before-connecting message", err)
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v, want it to carry the jail's stderr; without it the failure is unattributable", err)
	}
}
