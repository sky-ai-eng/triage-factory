package domain

import "time"

// TaskMemory is a durable per-run narrative of what an agent tried on a task
// and why. Written to `./task_memory/<run_id>.md` in the worktree during the
// run, then ingested here before worktree teardown. Materialized back into
// future runs' worktrees so iterations on the same task can read what prior
// attempts tried.
//
// Source distinguishes agent-authored entries (the normal case, written by
// the agent in response to the envelope's write-before-finish instructions)
// from system-authored stubs (written by the spawner when a run dies
// involuntarily without producing a valid completion JSON, so the invariant
// "every terminal run produces exactly one task_memory row" still holds).
type TaskMemory struct {
	ID        string
	TaskID    string
	RunID     string
	Content   string
	Source    string // "agent" | "system"
	CreatedAt time.Time
}
