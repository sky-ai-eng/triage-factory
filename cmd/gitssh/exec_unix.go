//go:build unix

package gitssh

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// execSSH replaces this process with the real ssh, so git keeps talking to a
// single process with the stdio, exit status and signal behavior it would have
// had if it had execed ssh itself. Returns only on failure to exec.
func execSSH(args []string, stderr io.Writer) int {
	path, ok := lookSSH(stderr)
	if !ok {
		return 127
	}
	argv := append([]string{"ssh"}, args...)
	if err := syscall.Exec(path, argv, os.Environ()); err != nil {
		fmt.Fprintf(stderr, "triagefactory git-ssh: exec ssh: %v\n", err)
		return 127
	}
	return 0 // unreachable: a successful Exec never returns
}
