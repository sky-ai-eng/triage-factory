//go:build linux

package procinfo

import (
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// maxTaskCommLen is Linux's TASK_COMM_LEN (16 bytes including the
// terminating NUL). The kernel silently truncates past this; truncating
// here too makes the behavior explicit rather than relying on the
// kernel's own silent cutoff.
const maxTaskCommLen = 16

// setTitle names the thread-group leader, which is what /proc/<pid>/comm and
// `ps` report. A name is a per-thread attribute, and the Go scheduler can run
// the caller on any OS thread, the main goroutine included, so it writes
// /proc/self/comm: the kernel applies a write there to the leader whichever
// thread in the group makes it. PR_SET_NAME, which names only the calling
// thread, is the fallback when /proc/self/comm cannot be opened.
func setTitle(name string) {
	b := []byte(name)
	if len(b) > maxTaskCommLen-1 {
		b = b[:maxTaskCommLen-1]
	}

	if f, err := os.OpenFile("/proc/self/comm", os.O_WRONLY, 0); err == nil {
		_, werr := f.Write(b)
		cerr := f.Close()
		if werr == nil && cerr == nil {
			return
		}
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
