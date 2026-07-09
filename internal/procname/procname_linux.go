//go:build linux

package procname

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// maxTaskCommLen is Linux's TASK_COMM_LEN (16 bytes including the
// terminating NUL). PR_SET_NAME silently truncates past this; truncating
// here too makes the behavior explicit rather than relying on the
// kernel's own silent cutoff.
const maxTaskCommLen = 16

func setTitle(name string) {
	b := []byte(name)
	if len(b) > maxTaskCommLen-1 {
		b = b[:maxTaskCommLen-1]
	}
	b = append(b, 0) // NUL-terminate for PR_SET_NAME's C-string argument

	// unix.Prctl takes arg2 as a plain uintptr — the compiler's "pointer
	// stays alive across a syscall" exception only covers a conversion
	// inlined directly in the raw syscall's own argument list, one frame
	// further down than this call, so KeepAlive pins b explicitly rather
	// than relying on that not applying transitively through the
	// intermediate Prctl call.
	_ = unix.Prctl(unix.PR_SET_NAME, uintptr(unsafe.Pointer(&b[0])), 0, 0, 0)
	runtime.KeepAlive(b)
}
