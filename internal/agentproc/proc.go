package agentproc

import (
	"fmt"
	"io"
	"os/exec"
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
// Stdin/Stdout are valid; drive the NDJSON stream; Wait; then Stderr and
// OOMKilled are final.
type runProc interface {
	Start() error
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Stderr() string
	Wait() error
	OOMKilled() bool
}

// execProc adapts a plain *exec.Cmd (the direct, non-sandbox path) to
// runProc, wiring the same pipes + captured stderr the live driver and
// one-shot runner used to set up inline. It is byte-identical to that
// prior handling; the direct path holds no cgroup, so OOMKilled is always
// false.
type execProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.Reader
	stderr *syncBuffer
}

// newExecProc creates the stdin/stdout pipes and stderr capture for cmd
// before it starts (exec requires the pipes to precede Start). The stdin
// pipe is created for both the interactive and one-shot paths — the
// wrapper only reads stdin in streaming mode, so an unused pipe on the
// one-shot path is inert.
func newExecProc(cmd *exec.Cmd) (*execProc, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr := newSyncBuffer()
	cmd.Stderr = stderr
	return &execProc{cmd: cmd, stdin: stdin, stdout: stdout, stderr: stderr}, nil
}

func (p *execProc) Start() error          { return p.cmd.Start() }
func (p *execProc) Stdin() io.WriteCloser { return p.stdin }
func (p *execProc) Stdout() io.Reader     { return p.stdout }
func (p *execProc) Stderr() string        { return p.stderr.String() }
func (p *execProc) Wait() error           { return p.cmd.Wait() }
func (p *execProc) OOMKilled() bool       { return false }
