package repoprofile

import (
	"database/sql"
	"log"
	"sync"
)

// ProfileGate coordinates access to repo profiles. The scorer checks
// Ready() before running; the profiler calls Signal() when done.
// Invalidate() resets the gate and clears stale repo matches from the DB.
type ProfileGate struct {
	mu       sync.Mutex
	ready    bool
	database *sql.DB
}

// NewProfileGate creates a gate in the not-ready state.
func NewProfileGate(database *sql.DB) *ProfileGate {
	return &ProfileGate{database: database}
}

// Ready returns true if profiling has completed and repo data is current.
func (g *ProfileGate) Ready() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.ready
}

// Signal marks profiling as complete. The scorer can now run.
func (g *ProfileGate) Signal() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ready = true
	log.Println("[repoprofile] gate: profiles ready")
}

// Invalidate marks profiling as stale (e.g., GitHub repos changed).
// Clears matched_repos and blocked_reason on all tasks so they get
// re-scored with fresh profiles on the next cycle.
func (g *ProfileGate) Invalidate() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ready = false
	log.Println("[repoprofile] gate: invalidated, clearing stale repo matches")

	if g.database != nil {
		if _, err := g.database.Exec(`UPDATE tasks SET matched_repos = NULL, blocked_reason = NULL`); err != nil {
			log.Printf("[repoprofile] gate: failed to clear repo matches: %v", err)
		}
	}
}
