// Package git implements `exec git sign`, the commit signing program a run's
// git calls. It is not an agent verb: the run's git config names the ssh-sign
// shim in the hooks dir as gpg.ssh.program, the shim hands it git's signing
// call, and git is its only intended caller. It is documented to no one, and
// absent from the exec help index.
//
// Git calls its signing program as
//
//	<program> -Y sign -n git -f <public key file> -U <buffer file>
//
// and reads the armored signature back from <buffer file>.sig. The commit is
// checked and signed by the run's daemon (agenthost.Client.SignCommit), which
// holds or reaches the key; this process only moves bytes between git's files
// and that call.
package git

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/commitsigner"
)

// maxPayloadBytes bounds the commit object read from git's buffer file: the
// whole object crosses the agenthost socket in one frame. A commit message
// this large is not one an agent writes.
const maxPayloadBytes = 8 << 20

// Handle runs `exec git <args>` and returns the process exit status. host is
// built only once the arguments are known to be git's signing call.
//
// Every failure prints one line to stderr and exits 1. Git then aborts the
// commit with "error: gpg failed to sign the data" and the agent reads that
// line, so a failure that is not a refusal still says what to do next.
func Handle(ctx context.Context, buildHost func() agenthost.Client, args []string, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "sign" {
		_, _ = fmt.Fprintln(stderr, usageLine)
		return 1
	}
	buffer, ok := signBuffer(args[1:])
	if !ok {
		_, _ = fmt.Fprintln(stderr, usageLine)
		return 1
	}
	payload, err := readPayload(buffer)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "triagefactory: could not read the commit git asked to sign: %v. Nothing was committed.\n", err)
		return 1
	}

	host := buildHost()
	defer func() { _ = host.Close() }()
	res, err := sign(ctx, host, payload)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, commitsigner.UnansweredLine())
		return 1
	}
	if res.Signature == "" {
		line := res.Refusal
		if line == "" {
			line = commitsigner.UnansweredLine()
		}
		_, _ = fmt.Fprintln(stderr, line)
		return 1
	}
	if err := os.WriteFile(buffer+".sig", []byte(res.Signature), 0o600); err != nil {
		_, _ = fmt.Fprintf(stderr, "triagefactory: could not write the signature for git: %v. Nothing was committed.\n", err)
		return 1
	}
	return 0
}

// usageLine answers anything that is not git's signing call.
const usageLine = "triagefactory: exec git takes only git's commit signing call, " +
	"sign -Y sign -n git [-f <key file>] [-U] <buffer file>, and is run by git, not by hand."

// signBuffer reads git's signing arguments, -Y sign -n git, then any of -f
// <file> and -U, then the buffer file last, and returns the buffer. -f and -U
// name the key git would sign with; the run's daemon signs with the signer's
// own, so they are accepted and ignored. Anything else is refused: a call git
// does not make is not one this program answers.
func signBuffer(args []string) (string, bool) {
	if len(args) < 5 || args[0] != "-Y" || args[1] != "sign" || args[2] != "-n" || args[3] != "git" {
		return "", false
	}
	rest := args[4 : len(args)-1]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "-U":
		case "-f":
			if i+1 == len(rest) {
				return "", false
			}
			i++
		default:
			return "", false
		}
	}
	buffer := args[len(args)-1]
	if buffer == "" {
		return "", false
	}
	return buffer, true
}

func readPayload(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	payload, err := io.ReadAll(io.LimitReader(f, maxPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxPayloadBytes {
		return nil, fmt.Errorf("the commit is larger than %d bytes", maxPayloadBytes)
	}
	return payload, nil
}

// sign asks the host for the signature and stops waiting at
// commitsigner.RequestTimeout, whether or not the call honors its context: in
// a local run with the sandbox off the call reads the OS keychain in this
// process, which can block on a prompt no one will answer.
func sign(ctx context.Context, host agenthost.Client, payload []byte) (agenthost.SignCommitResult, error) {
	ctx, cancel := context.WithTimeout(ctx, commitsigner.RequestTimeout)
	defer cancel()
	type answer struct {
		res agenthost.SignCommitResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := host.SignCommit(ctx, payload)
		done <- answer{res, err}
	}()
	select {
	case a := <-done:
		return a.res, a.err
	case <-ctx.Done():
		return agenthost.SignCommitResult{}, ctx.Err()
	}
}
