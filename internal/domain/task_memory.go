package domain

import "time"

// TaskMemory is a durable per-run narrative of what an agent tried on a task
// and why. Written to `./_scratch/entity-memory/<namespace>/<run_id>.md` in the
// worktree during the run, then ingested into the `run_memory` table before
// worktree teardown. The namespace is the run's BlueprintRunID when set, else
// the run's own id (see the spawner's memoryNamespace helper).
// Materialized back into future runs' worktrees so iterations on the same
// entity can read what prior attempts tried — and so the sibling steps of one
// blueprint run can read each other's memory as their handoff.
//
// Stored in the `run_memory` table with a denormalized `entity_id` for fast
// entity-scoped queries (memory materialization walks the entity graph) and a
// denormalized `blueprint_run_id` that groups one blueprint run's files.
type TaskMemory struct {
	ID             string
	RunID          string
	EntityID       string // denormalized from run→task→entity for fast entity-scoped queries
	BlueprintRunID string // denormalized from the run; empty for a standalone (non-blueprint) run
	Content        string
	CreatedAt      time.Time
}
