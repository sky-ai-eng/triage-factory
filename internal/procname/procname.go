// Package procname sets this process's kernel-visible name
// (/proc/<pid>/comm, what `ps` shows) so a security reviewer's `ps` shows
// the cap-broker/orchestrator split at a glance — `tf-cap-broker` vs.
// `tf-orchestrator` — instead of both processes sharing the same binary
// basename. Code reads the name too: the boot-time sidecar reap finds
// orphaned sidecars by it, and the compose smoke test finds the broker by
// it.
//
// This is explicitly NOT a privilege operation — renaming a process's comm
// requires no capability — so, unlike the actual capability drop (which
// must happen at exec, never in-process, since capabilities are
// per-thread and the Go runtime spawns threads before main() runs), it
// is fine to call from within a running Go program.
package procname

// SetTitle sets /proc/<pid>/comm to name (truncated to the kernel's
// 15-usable-byte TASK_COMM_LEN limit) on Linux; a no-op everywhere else.
// It names the process whichever goroutine, and so whichever OS thread,
// calls it. Call it once, early in main().
func SetTitle(name string) {
	setTitle(name)
}
