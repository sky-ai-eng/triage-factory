// Package procname sets this process's kernel-visible name (the
// prctl(PR_SET_NAME) proctitle, i.e. /proc/<pid>/comm) so a security
// reviewer's `ps` shows the cap-broker/orchestrator split at a glance —
// `tf-cap-broker` vs. `tf-orchestrator` — instead of both processes
// sharing the same binary basename.
//
// This is explicitly NOT a privilege operation — renaming a thread's comm
// requires no capability — so, unlike the actual capability drop (which
// must happen at exec, never in-process, since capabilities are
// per-thread and the Go runtime spawns threads before main() runs), it
// is fine to call from within a running Go program.
package procname

// SetTitle sets /proc/<pid>/comm to name (truncated to the kernel's
// 15-usable-byte TASK_COMM_LEN limit) on Linux; a no-op everywhere else.
// Call it once, early in main(), before any other goroutine's identity
// matters for this purpose.
func SetTitle(name string) {
	setTitle(name)
}
