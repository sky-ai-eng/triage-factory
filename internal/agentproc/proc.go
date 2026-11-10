package agentproc

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// runProc is the started agent-runtime process the live driver and the
// one-shot runner drive, abstracting over the two launch shapes:
//
//   - the direct (local / non-sandbox) subprocess — *execProc below;
//   - the gVisor sandbox run — a sandbox.LaunchedRun, which the sandbox
//     package returns already satisfying this method set (a cap-broker
//     proxy: the runsc child runs in the broker, this end drives its
//     stdio).
//
// The contract mirrors the pre-split *exec.Cmd dance: Start, then
// Stdin/Stdout are valid; drive the NDJSON stream; Wait; then Stderr,
// OOMKilled and Actuals are final.
type runProc interface {
	Start() error
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() string
	Wait() error
	OOMKilled() bool
	Actuals() sandbox.RunActuals
}

// execProc adapts a plain *exec.Cmd (the direct, non-sandbox path) to
// runProc, wiring the same pipes + captured stderr the live driver and
// one-shot runner used to set up inline. It is byte-identical to that
// prior handling; the direct path holds no cgroup, so OOMKilled is always
// false and Actuals is always empty (local runs record no measured cost —
// there is no jail whose consumption could be attributed to the run).
type execProc struct {
	cmd    *exec.Cmd
	tree   *procTree
	stdin  io.WriteCloser
	stdout io.Reader
	stderr *syncBuffer
}

// newExecProc creates the stdin/stdout pipes and stderr capture for cmd
// before it starts (exec requires the pipes to precede Start). The stdin
// pipe is created for both the interactive and one-shot paths — the
// wrapper only reads stdin in streaming mode, so an unused pipe on the
// one-shot path is inert.
//
// tree is the handle configureProcessTree returned for cmd, or nil for a
// command that needs no process-tree teardown. Start attaches the started
// process to it and Wait (or a failed Start) releases it.
func newExecProc(cmd *exec.Cmd, tree *procTree) (*execProc, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		tree.close()
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		tree.close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr := newSyncBuffer()
	cmd.Stderr = stderr
	return &execProc{cmd: cmd, tree: tree, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

// Start launches the process and attaches it to its process tree. An attach
// failure is logged and not returned: the run is still correct, and cancel
// falls back to killing the direct child.
func (p *execProc) Start() error {
	if err := p.cmd.Start(); err != nil {
		p.tree.close()
		return err
	}
	if p.tree != nil {
		if err := p.tree.attach(p.cmd.Process); err != nil {
			agentprocLog.Warn("attach agent process to process tree failed", "pid", p.cmd.Process.Pid, "error", err)
		}
	}
	return nil
}

// Wait reaps the process, then releases the process tree. Releasing it ends
// any descendant the agent left running.
func (p *execProc) Wait() error {
	err := p.cmd.Wait()
	p.tree.close()
	return err
}

func (p *execProc) Stdin() io.WriteCloser { return p.stdin }
func (p *execProc) Stdout() io.Reader     { return p.stdout }
func (p *execProc) Stderr() string        { return p.stderr.String() }
func (p *execProc) OOMKilled() bool       { return false }

func (p *execProc) Actuals() sandbox.RunActuals { return sandbox.RunActuals{} }

// recordActualsTimeout bounds the teardown stamp. The write is one small
// UPDATE, and it runs off the run's own (possibly already cancelled)
// context, so a wedged database must not hold a finished run's teardown
// open — losing the accounting is the cheaper failure.
const recordActualsTimeout = 5 * time.Second

// recordSandboxActuals stamps what the launch consumed against the
// engagement that paid for it, once the runtime has exited and its cgroup
// read has been taken. Called from both the one-shot and the live path, so
// every disposition — completed, failed, cancelled, hibernated — records the
// cost it actually incurred.
//
// Silent no-op when there is nothing to record or nowhere to put it: a
// launch with no claim (the toolless system jobs), a caller that wired no
// recorder, or a run that measured nothing (the local/direct path, which
// holds no cgroup). Writing zeroes in those cases would turn "never
// measured" into a claim of a free run.
//
// Detached, bounded context: teardown routinely runs after the run's ctx is
// dead (a cancel is exactly when it happens), and the stamp is best-effort —
// a failure is logged and never surfaces to the run's disposition.
func recordSandboxActuals(ctx context.Context, opts RunOptions, proc runProc) {
	if opts.RecordSandboxActuals == nil || opts.ClaimID == "" {
		return
	}
	actuals := proc.Actuals()
	if actuals.PeakMemMB == nil && actuals.CPUUsec == nil {
		return
	}
	stampCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordActualsTimeout)
	defer cancel()
	if err := opts.RecordSandboxActuals(stampCtx, opts.OrgID, opts.ClaimID, actuals); err != nil {
		agentprocLog.Warn("record sandbox actuals for claim failed",
			"claim", opts.ClaimID, "conversation", opts.TraceID, "error", err)
	}
}
