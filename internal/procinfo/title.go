package procinfo

// SetTitle sets /proc/<pid>/comm to name (truncated to the kernel's
// 15-usable-byte TASK_COMM_LEN limit) on Linux; a no-op everywhere else.
// It names the process whichever goroutine, and so whichever OS thread,
// calls it. Call it once, early in main().
//
// The name is what `ps` shows, so the cap-broker and the orchestrator read
// as `tf-cap-broker` and `tf-orchestrator` instead of sharing the binary's
// basename. Code reads it too: the boot-time sidecar reap finds orphaned
// sidecars by it, and the compose smoke test finds the broker by it.
func SetTitle(name string) {
	setTitle(name)
}
