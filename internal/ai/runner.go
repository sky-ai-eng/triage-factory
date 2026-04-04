package ai

import (
	"database/sql"
	"log"
	"sync"

	"github.com/sky-ai-eng/todo-tinder/internal/db"
	"github.com/sky-ai-eng/todo-tinder/internal/domain"
)

// RunnerCallbacks are optional hooks fired during the scoring lifecycle.
// The caller wires these to WS broadcasts or other side effects.
type RunnerCallbacks struct {
	OnScoringStarted   func(taskIDs []string)
	OnScoringCompleted func(taskIDs []string)
}

// Runner manages AI scoring as a background process.
// It exposes a Trigger channel that pollers signal after ingesting new tasks.
type Runner struct {
	database  *sql.DB
	callbacks RunnerCallbacks
	trigger   chan struct{}
	stop      chan struct{}
	mu        sync.Mutex
	running   bool
}

func NewRunner(database *sql.DB, callbacks RunnerCallbacks) *Runner {
	return &Runner{
		database:  database,
		callbacks: callbacks,
		trigger:   make(chan struct{}, 1),
		stop:      make(chan struct{}),
	}
}

// Trigger signals the runner to check for unscored tasks.
// Non-blocking — if a scoring run is already pending, the signal is merged.
func (r *Runner) Trigger() {
	select {
	case r.trigger <- struct{}{}:
	default:
		// already triggered, skip
	}
}

func (r *Runner) Start() {
	go func() {
		for {
			select {
			case <-r.trigger:
				r.run()
			case <-r.stop:
				return
			}
		}
	}()
}

func (r *Runner) Stop() {
	close(r.stop)
}

func (r *Runner) run() {
	r.mu.Lock()
	if r.running {
		r.mu.Unlock()
		return
	}
	r.running = true
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.running = false
		r.mu.Unlock()
	}()

	tasks, err := db.UnscoredTasks(r.database)
	if err != nil {
		log.Printf("[ai] error fetching unscored tasks: %v", err)
		return
	}

	if len(tasks) == 0 {
		return
	}

	log.Printf("[ai] scoring %d unscored tasks...", len(tasks))

	// Collect task IDs for callbacks
	taskIDs := make([]string, len(tasks))
	for i, t := range tasks {
		taskIDs[i] = t.ID
	}

	// Persist scoring state before calling AI
	if err := db.MarkScoring(r.database, taskIDs); err != nil {
		log.Printf("[ai] error marking tasks as scoring: %v", err)
	}

	if r.callbacks.OnScoringStarted != nil {
		r.callbacks.OnScoringStarted(taskIDs)
	}

	scores, err := ScoreTasks(tasks)
	if err != nil {
		log.Printf("[ai] scoring failed: %v", err)
		return
	}

	updates := make([]domain.TaskScoreUpdate, len(scores))
	for i, s := range scores {
		updates[i] = domain.TaskScoreUpdate{
			ID:                s.ID,
			PriorityScore:     s.PriorityScore,
			AgentConfidence:   s.AgentConfidence,
			PriorityReasoning: s.PriorityReasoning,
			Summary:           s.Summary,
		}
	}

	if err := db.UpdateTaskScores(r.database, updates); err != nil {
		log.Printf("[ai] error saving scores: %v", err)
		return
	}

	log.Printf("[ai] scored %d tasks successfully", len(updates))

	if r.callbacks.OnScoringCompleted != nil {
		r.callbacks.OnScoringCompleted(taskIDs)
	}
}
