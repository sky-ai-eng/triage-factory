//go:build windows

package gitssh

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

// execSSH runs the real ssh as a child and returns its exit status, because
// Windows has no process-replacing exec: the child inherits this process's
// stdio and environment, so git still sees one transport with the stdio and
// exit status ssh would have given it. Signals are not forwarded. git stops a
// transport by closing its pipes or killing this process, and Windows has no
// signal to relay in the first place, so a Ctrl+C reaches both processes
// through the shared console instead.
func execSSH(args []string, stderr io.Writer) int {
	path, ok := lookSSH(stderr)
	if !ok {
		return 127
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "triagefactory git-ssh: exec ssh: %v\n", err)
		return 127
	}
	return 0
}
