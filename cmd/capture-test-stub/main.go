// cmd/capture-test-stub is the capture jail's program for the sandbox
// integration tests: the same `snapshot-capture` body the real binary runs,
// built as a pure-Go static binary.
//
// The real binary is glibc-linked on most dev systems, and bind-mounting it
// into an alpine (musl) rootfs makes it fail to exec; the production image
// builds it static. The tests need the capture body, not the whole server, so
// they build this instead — the jail runs exactly the code that ships, behind
// the argv the broker pins.
//
// Compiled with CGO_ENABLED=0, like cmd/exec-test-stub.
package main

import (
	"fmt"
	"os"

	"github.com/sky-ai-eng/triage-factory/cmd/snapshotcapture"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "snapshot-capture" {
		fmt.Fprintln(os.Stderr, "capture-test-stub: only snapshot-capture is supported")
		os.Exit(2)
	}
	snapshotcapture.Handle(os.Args[2:])
}
