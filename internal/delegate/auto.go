package delegate

import (
	"database/sql"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// MaybeAutoDelegate checks whether an event should trigger an automatic
// delegation and fires it if all gates pass. Called from the event bus
// subscriber — runs asynchronously in its own goroutine.
//
// Gates (checked in order):
//  1. Global kill switch (config.AI.AutoDelegateEnabled)
//  2. Trigger lookup (prompt_triggers for the event type)
//  3. In-flight gate (no active run for this task)
//  4. Breaker threshold (consecutive_unsuccessful_runs < breaker_threshold)
//  5. Cooldown (last auto-run started_at + cooldown_seconds < now)
func MaybeAutoDelegate(database *sql.DB, spawner *Spawner, evt domain.Event) {
	if evt.TaskID == "" {
		return
	}

	// Gate 1: global kill switch
	cfg, err := config.Load()
	if err != nil {
		log.Printf("[auto-delegate] failed to load config: %v", err)
		return
	}
	if !cfg.AI.AutoDelegateEnabled {
		return
	}

	// Gate 2: find matching triggers
	triggers, err := db.GetActiveTriggersForEvent(database, evt.EventType)
	if err != nil {
		log.Printf("[auto-delegate] failed to lookup triggers for %s: %v", evt.EventType, err)
		return
	}
	if len(triggers) == 0 {
		return
	}

	// Gate 3: in-flight — don't stack runs on the same task
	active, err := db.HasActiveRunForTask(database, evt.TaskID)
	if err != nil {
		log.Printf("[auto-delegate] failed to check active runs for task %s: %v", evt.TaskID, err)
		return
	}
	if active {
		log.Printf("[auto-delegate] skipping %s for task %s: run already in flight", evt.EventType, evt.TaskID)
		return
	}

	// Load the task once for iteration cap checks
	task, err := db.GetTask(database, evt.TaskID)
	if err != nil || task == nil {
		log.Printf("[auto-delegate] failed to load task %s: %v", evt.TaskID, err)
		return
	}

	for _, trigger := range triggers {
		// Gate 4: breaker threshold
		if task.ConsecutiveUnsuccessfulRuns >= trigger.BreakerThreshold {
			log.Printf("[auto-delegate] skipping trigger %s for task %s: breaker tripped (%d >= %d)",
				trigger.ID, task.ID, task.ConsecutiveUnsuccessfulRuns, trigger.BreakerThreshold)
			continue
		}

		// Gate 5: cooldown
		if trigger.CooldownSeconds > 0 {
			lastStart, err := db.LastAutoRunStartedAt(database, task.ID)
			if err != nil {
				log.Printf("[auto-delegate] failed to check cooldown for task %s: %v", task.ID, err)
				continue
			}
			if lastStart != nil {
				cooldownExpires := lastStart.Add(time.Duration(trigger.CooldownSeconds) * time.Second)
				if time.Now().Before(cooldownExpires) {
					log.Printf("[auto-delegate] skipping trigger %s for task %s: cooldown (%ds remaining)",
						trigger.ID, task.ID, int(time.Until(cooldownExpires).Seconds()))
					continue
				}
			}
		}

		// All gates passed — fire
		log.Printf("[auto-delegate] firing trigger %s (prompt %s) for task %s on event %s",
			trigger.ID, trigger.PromptID, task.ID, evt.EventType)

		runID, err := spawner.Delegate(*task, trigger.PromptID, "event", trigger.ID)
		if err != nil {
			log.Printf("[auto-delegate] failed to delegate task %s: %v", task.ID, err)
			continue
		}
		log.Printf("[auto-delegate] started run %s for task %s", runID, task.ID)
	}
}
