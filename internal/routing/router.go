package routing

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Scorer is the minimal interface the router needs from the AI runner.
type Scorer interface {
	Trigger()
}

// Delegator is the minimal interface the router needs from the delegate
// spawner — kicking off a run, plus cancelling one. Narrowed from
// *delegate.Spawner so tests can stub the spawn surface without bringing
// up a worktree, the agent subprocess, etc. Production wiring passes a
// *delegate.Spawner.
type Delegator interface {
	Delegate(task domain.Task, promptID, triggerType, triggerID string) (string, error)
	Cancel(runID string) error
}

// Router is the central eventbus subscriber that replaces the old auto-
// delegate hook. On every event it:
//  1. Records the event (durable audit log)
//  2. Handles entity lifecycle (merged/closed → close entity + all tasks)
//  3. Guards against closed entities (no task creation on dead PRs)
//  4. Matches task_rules + prompt_triggers via typed predicates
//  5. Dedup-creates or bumps tasks
//  6. Enqueues AI scoring
//  7. Auto-delegates on matching triggers — fires if the entity is idle,
//     enqueues onto pending_firings if the entity already has an active
//     auto run or earlier queued firings (SKY-189).
//  8. Runs inline close checks for the event type
type Router struct {
	db      *sql.DB
	spawner Delegator
	scorer  Scorer
	ws      *websocket.Hub

	// drainLocks serializes DrainEntity calls per entity. Without this,
	// the non-mutating PopPendingFiringForEntity creates a window between
	// pop and MarkPendingFiringFired/Skipped where a concurrent drain
	// (typically spawned by a fast-terminating run that the first drain
	// just fired) can pop the same row and double-fire it. The mutex
	// closes the window: a second drain blocks until the first marks the
	// firing terminal, so its pop returns the next row (or nothing).
	//
	// Map grows monotonically with the count of distinct entities ever
	// drained. Bounded by entity count for the lifetime of the process,
	// which is small enough that we don't bother evicting on entity
	// close.
	drainLockMu sync.Mutex
	drainLocks  map[string]*sync.Mutex
}

// NewRouter creates a Router.
func NewRouter(db *sql.DB, spawner Delegator, scorer Scorer, ws *websocket.Hub) *Router {
	return &Router{
		db:         db,
		spawner:    spawner,
		scorer:     scorer,
		ws:         ws,
		drainLocks: make(map[string]*sync.Mutex),
	}
}

// entityDrainLock returns the per-entity mutex used to serialize
// DrainEntity calls. Lazily created on first use; never evicted.
func (r *Router) entityDrainLock(entityID string) *sync.Mutex {
	r.drainLockMu.Lock()
	defer r.drainLockMu.Unlock()
	mu, ok := r.drainLocks[entityID]
	if !ok {
		mu = &sync.Mutex{}
		r.drainLocks[entityID] = mu
	}
	return mu
}

// HandleEvent is the eventbus subscriber callback. Called asynchronously
// from the bus's per-subscriber goroutine.
func (r *Router) HandleEvent(evt domain.Event) {
	// Step 1: Always record — durable audit log regardless of routing outcome.
	// Later routing logic relies on evt.ID referring to a persisted event row,
	// so stop here if the insert fails.
	id, err := dbpkg.RecordEvent(r.db, evt)
	if err != nil {
		log.Printf("[router] failed to record event %s: %v", evt.EventType, err)
		return
	}
	evt.ID = id

	// Step 2: Entity lifecycle — entity-terminating events close the entity
	// and cascade-close all its tasks. Return after — no task creation on a
	// closing entity.
	if evt.EntityID != nil && EntityTerminatingEvents[evt.EventType] {
		closed, err := r.closeEntity(*evt.EntityID)
		if err != nil {
			log.Printf("[router] entity lifecycle error for %s: %v", *evt.EntityID, err)
		}
		if closed > 0 {
			r.ws.Broadcast(websocket.Event{Type: "tasks_updated", Data: map[string]any{}})
		}
		return
	}

	// Step 3: Skip system events (no entity context → no task creation).
	if evt.EntityID == nil {
		return
	}
	entityID := *evt.EntityID

	// Step 4: Closed-entity guard — late events on already-closed entities
	// are recorded (step 1) but don't spawn tasks.
	entity, err := dbpkg.GetEntity(r.db, entityID)
	if err != nil || entity == nil {
		log.Printf("[router] failed to load entity %s: %v", entityID, err)
		return
	}
	if entity.State != "active" {
		return
	}

	// became_atomic is the belated-discovery path for parents whose
	// subtasks just closed. Only create a task when none exists on the
	// entity — otherwise an atomic ticket that gained and then lost
	// subtasks would end up with two cards for the same entity. The
	// dedup index doesn't catch this because the existing task's
	// event_type is jira:issue:assigned while the new one would be
	// jira:issue:became_atomic. Event is still recorded (audit trail
	// stays honest); only task creation and trigger firing are
	// suppressed.
	if evt.EventType == domain.EventJiraIssueBecameAtomic {
		active, err := dbpkg.FindActiveTasksByEntity(r.db, entityID)
		if err != nil {
			log.Printf("[router] became_atomic: failed to check active tasks on entity %s: %v", entityID, err)
			return
		}
		if len(active) > 0 {
			log.Printf("[router] became_atomic: entity %s has %d active task(s), skipping duplicate creation", entityID, len(active))
			return
		}
	}

	// Step 5: Match task_rules for this event type.
	rules, err := dbpkg.GetEnabledRulesForEvent(r.db, evt.EventType)
	if err != nil {
		log.Printf("[router] failed to query rules for %s: %v", evt.EventType, err)
	}

	var matchedRules []domain.TaskRule
	for _, rule := range rules {
		predJSON := ""
		if rule.ScopePredicateJSON != nil {
			predJSON = *rule.ScopePredicateJSON
		}
		matched, err := matchPredicate(evt.EventType, predJSON, evt.MetadataJSON)
		if err != nil {
			log.Printf("[router] rule %s predicate error: %v", rule.ID, err)
			continue
		}
		if matched {
			matchedRules = append(matchedRules, rule)
		}
	}

	// Step 6: Match prompt_triggers for this event type.
	triggers, err := dbpkg.GetActiveTriggersForEvent(r.db, evt.EventType)
	if err != nil {
		log.Printf("[router] failed to query triggers for %s: %v", evt.EventType, err)
	}

	var matchedTriggers []domain.PromptTrigger
	for _, trigger := range triggers {
		predJSON := ""
		if trigger.ScopePredicateJSON != nil {
			predJSON = *trigger.ScopePredicateJSON
		}
		matched, err := matchPredicate(evt.EventType, predJSON, evt.MetadataJSON)
		if err != nil {
			log.Printf("[router] trigger %s predicate error: %v", trigger.ID, err)
			continue
		}
		if matched {
			matchedTriggers = append(matchedTriggers, trigger)
		}
	}

	// Nothing matched — event is recorded but no task created.
	if len(matchedRules) == 0 && len(matchedTriggers) == 0 {
		return
	}

	// Step 7: Find or create task via dedup index.
	// Use the highest-priority matching rule's default_priority, or 0.5 if
	// only triggers matched (forgiving path).
	defaultPriority := 0.5
	for _, rule := range matchedRules {
		if rule.DefaultPriority > defaultPriority {
			defaultPriority = rule.DefaultPriority
		}
	}

	task, created, err := dbpkg.FindOrCreateTask(r.db, entityID, evt.EventType, evt.DedupKey, evt.ID, defaultPriority)
	if err != nil {
		log.Printf("[router] failed to find/create task for %s on entity %s: %v", evt.EventType, entityID, err)
		return
	}

	if created {
		if err := dbpkg.RecordTaskEvent(r.db, task.ID, evt.ID, "spawned"); err != nil {
			log.Printf("[router] failed to record spawned task_event: %v", err)
		}
		log.Printf("[router] created task %s (%s) on entity %s", task.ID, evt.EventType, entityID)
	} else {
		if err := dbpkg.BumpTask(r.db, task.ID, evt.ID); err != nil {
			log.Printf("[router] failed to bump task %s: %v", task.ID, err)
		}
		if err := dbpkg.RecordTaskEvent(r.db, task.ID, evt.ID, "bumped"); err != nil {
			log.Printf("[router] failed to record bumped task_event: %v", err)
		}
	}

	// Step 8: Enqueue AI scoring (always — produces UI metadata regardless).
	r.scorer.Trigger()

	// Broadcast task update to frontend.
	r.ws.Broadcast(websocket.Event{Type: "tasks_updated", Data: map[string]any{}})

	// Step 9: Auto-delegate for matching triggers.
	// Gate: global kill switch — if auto-delegation is disabled, skip all triggers.
	// Create vs bump no longer branches differently here: the per-entity
	// queue handles bursts via its dedup index, and cooldown was removed
	// in SKY-189 (collapse on (task_id, trigger_id) covers the same case
	// the cooldown was protecting against). Triggers with
	// min_autonomy_suitability > 0 still defer to post-scoring re-derive.
	if cfg, err := config.Load(); err == nil && cfg.AI.AutoDelegateEnabled {
		for _, trigger := range matchedTriggers {
			if trigger.MinAutonomySuitability > 0 {
				continue // deferred to post-scoring handler
			}
			r.tryAutoDelegate(task, trigger, entityID, evt.ID)
		}
	}

	// Step 10: Inline close checks.
	r.runInlineCloseChecks(evt, entityID)
}

// tryAutoDelegate decides whether a matched (task, trigger) fires now or
// queues. Order of checks: breaker (per-(entity,prompt)) → entity gate
// (per-entity, auto-only) → fire or enqueue.
//
// Breaker is a hard skip — a tripped breaker means the user has work to
// investigate before more runs land on this entity-prompt pair. Queueing
// past it would just stack stale firings the user didn't ask for.
//
// Entity gate is the per-entity serialization point: at most one auto run
// in flight per entity, regardless of which task/trigger it came from. If
// the gate is closed (active auto run, or older firings already queued
// for FIFO fairness), the firing enqueues onto pending_firings instead of
// being dropped silently.
func (r *Router) tryAutoDelegate(task *domain.Task, trigger domain.PromptTrigger, entityID string, triggeringEventID string) {
	// Breaker gate.
	failures, err := dbpkg.CountConsecutiveFailedRuns(r.db, entityID, trigger.PromptID)
	if err != nil {
		log.Printf("[router] breaker query error for entity %s prompt %s: %v", entityID, trigger.PromptID, err)
		return
	}
	if failures >= trigger.BreakerThreshold {
		log.Printf("[router] breaker tripped for entity %s prompt %s (%d >= %d)",
			entityID, trigger.PromptID, failures, trigger.BreakerThreshold)
		// Look up prompt name for the toast — opportunistic, falls back to a
		// generic message if the lookup fails since the breaker trip itself
		// is the load-bearing signal. One toast per trip (happens rarely).
		promptName := ""
		if p, perr := dbpkg.GetPrompt(r.db, trigger.PromptID); perr == nil && p != nil {
			promptName = p.Name
		}
		if promptName == "" {
			promptName = "prompt"
		}
		toast.Warning(r.ws, fmt.Sprintf("Auto-delegation paused: %s tripped the breaker (%d consecutive failures on this entity)", promptName, failures))
		return
	}

	// Per-entity gate. Closed if any auto run is active on the entity OR
	// any pending_firings rows are already queued (FIFO fairness).
	canFire, err := dbpkg.EntityCanFireImmediately(r.db, entityID)
	if err != nil {
		log.Printf("[router] entity gate query error for %s: %v", entityID, err)
		return
	}
	if !canFire {
		inserted, err := dbpkg.EnqueuePendingFiring(r.db, entityID, task.ID, trigger.ID, triggeringEventID)
		if err != nil {
			log.Printf("[router] enqueue firing failed (entity %s task %s trigger %s): %v",
				entityID, task.ID, trigger.ID, err)
			return
		}
		if inserted {
			log.Printf("[router] queued firing on entity %s (task %s, trigger %s) — entity busy",
				entityID, task.ID, trigger.ID)
		} else {
			log.Printf("[router] firing collapsed on entity %s (task %s, trigger %s) — duplicate already queued",
				entityID, task.ID, trigger.ID)
		}
		return
	}

	if _, err := r.fireDelegate(task, trigger); err != nil {
		log.Printf("[router] fire failed for task %s (trigger %s): %v", task.ID, trigger.ID, err)
	}
}

// fireDelegate transitions the task to delegated status, broadcasts the
// change, then fires the spawner. Returns the run ID on success — used by
// DrainEntity to record which run a queued firing materialized into.
func (r *Router) fireDelegate(task *domain.Task, trigger domain.PromptTrigger) (string, error) {
	if r.spawner == nil {
		return "", fmt.Errorf("spawner not configured")
	}

	// Transition task queued → delegated BEFORE spawning so the frontend
	// reflects the state change immediately and dedup logic sees it.
	if err := dbpkg.SetTaskStatus(r.db, task.ID, "delegated"); err != nil {
		return "", fmt.Errorf("set task delegated: %w", err)
	}
	r.ws.Broadcast(websocket.Event{
		Type: "task_updated",
		Data: map[string]any{"task_id": task.ID, "status": "delegated"},
	})

	log.Printf("[router] auto-delegating task %s (trigger %s, prompt %s)",
		task.ID, trigger.ID, trigger.PromptID)

	// Re-read task to get entity-joined display fields the spawner needs.
	fresh, err := dbpkg.GetTask(r.db, task.ID)
	if err != nil || fresh == nil {
		r.revertTaskStatus(task.ID, "queued")
		if err != nil {
			return "", fmt.Errorf("re-read task: %w", err)
		}
		return "", fmt.Errorf("task %s disappeared between status flip and re-read", task.ID)
	}

	runID, err := r.spawner.Delegate(*fresh, trigger.PromptID, "event", trigger.ID)
	if err != nil {
		r.revertTaskStatus(task.ID, "queued")
		return "", err
	}
	log.Printf("[router] started run %s for task %s", runID, task.ID)
	return runID, nil
}

// DrainEntity is the spawner's hook into the per-entity firing queue.
// Called when an auto run terminates on the entity (any terminal status,
// including pending_approval per the SKY-189 design — pending_approval
// releases the entity lock so user deliberation doesn't block downstream
// processing).
//
// Pops pending firings in FIFO order, validates each against current
// state (task still active? trigger still enabled? breaker still under
// threshold?), and fires the first valid one. Stale firings are
// soft-deleted with a skip_reason and the loop continues. At most one
// firing actually fires per drain — that run becomes the new in-flight
// for the entity and gates further drains naturally.
func (r *Router) DrainEntity(entityID string) {
	// Serialize drains per entity. Without this, a fast-terminating run
	// fired by an earlier drain can spawn a second DrainEntity goroutine
	// that pops the same pending_firings row before the first drain
	// transitions it out of 'pending' — leading to duplicate fireDelegate
	// calls. The MarkPendingFiringFired/Skipped guards on
	// status='pending' protect the row's own mutation but cannot un-fire
	// the duplicate run. This mutex closes the window: the second drain
	// blocks until the first releases, by which point the firing has
	// landed in a terminal status and the second drain's pop returns the
	// next row (or nothing).
	mu := r.entityDrainLock(entityID)
	mu.Lock()
	defer mu.Unlock()

	for {
		firing, err := dbpkg.PopPendingFiringForEntity(r.db, entityID)
		if err != nil {
			log.Printf("[router] drain pop error for entity %s: %v", entityID, err)
			return
		}
		if firing == nil {
			return // queue empty
		}

		runID, skipReason := r.attemptDrainOne(firing)
		if runID != "" {
			if err := dbpkg.MarkPendingFiringFired(r.db, firing.ID, runID); err != nil {
				log.Printf("[router] mark firing %d fired (run %s): %v", firing.ID, runID, err)
			}
			return // one fire per drain — the new run gates the rest
		}
		// Skipped or fire failed; record reason and continue draining.
		if err := dbpkg.MarkPendingFiringSkipped(r.db, firing.ID, skipReason); err != nil {
			log.Printf("[router] mark firing %d skipped (%s): %v", firing.ID, skipReason, err)
			return
		}
		log.Printf("[router] skipped firing %d on entity %s: %s", firing.ID, entityID, skipReason)
	}
}

// attemptDrainOne validates a popped firing against current state and
// fires it if everything still holds. Returns (runID, "") on successful
// fire, or ("", skipReason) if the firing should be soft-deleted.
//
// Validation reads from live tables, not from the firing row, so the
// drainer reflects the world *now* — invalidation falls out for free
// from the existing close cascade and trigger config: a task that was
// closed mid-pause is already 'done' here, and a trigger the user
// disabled while we waited is no longer enabled.
func (r *Router) attemptDrainOne(firing *domain.PendingFiring) (runID, skipReason string) {
	task, err := dbpkg.GetTask(r.db, firing.TaskID)
	if err != nil {
		log.Printf("[router] drain task lookup failed (firing %d): %v", firing.ID, err)
		return "", domain.PendingFiringSkipFireFailed
	}
	if task == nil || task.Status == "done" || task.Status == "dismissed" {
		return "", domain.PendingFiringSkipTaskClosed
	}

	trigger, err := dbpkg.GetPromptTrigger(r.db, firing.TriggerID)
	if err != nil {
		log.Printf("[router] drain trigger lookup failed (firing %d): %v", firing.ID, err)
		return "", domain.PendingFiringSkipFireFailed
	}
	if trigger == nil || !trigger.Enabled {
		return "", domain.PendingFiringSkipTriggerDisabled
	}

	failures, err := dbpkg.CountConsecutiveFailedRuns(r.db, firing.EntityID, trigger.PromptID)
	if err != nil {
		log.Printf("[router] drain breaker query failed (firing %d): %v", firing.ID, err)
		return "", domain.PendingFiringSkipFireFailed
	}
	if failures >= trigger.BreakerThreshold {
		return "", domain.PendingFiringSkipBreakerTripped
	}

	id, err := r.fireDelegate(task, *trigger)
	if err != nil {
		log.Printf("[router] drain fire failed (firing %d, task %s): %v", firing.ID, task.ID, err)
		return "", domain.PendingFiringSkipFireFailed
	}
	return id, ""
}

// revertTaskStatus sets a task back to the given status and broadcasts the
// change so the frontend doesn't get stuck showing a stale state (e.g.,
// "delegated" after a delegation failure).
func (r *Router) revertTaskStatus(taskID, status string) {
	if err := dbpkg.SetTaskStatus(r.db, taskID, status); err != nil {
		log.Printf("[router] failed to revert task %s to %s: %v", taskID, status, err)
		return
	}
	r.ws.Broadcast(websocket.Event{
		Type: "task_updated",
		Data: map[string]any{"task_id": taskID, "status": status},
	})
}

// --- Post-scoring re-derive (SKY-181) ------------------------------------

// ReDeriveAfterScoring re-checks deferred triggers for tasks that just
// received AI scores. Triggers with MinAutonomySuitability > 0 are skipped
// during HandleEvent and deferred to this callback, which fires from the
// scorer's OnScoringCompleted hook.
func (r *Router) ReDeriveAfterScoring(taskIDs []string) {
	// Global kill switch — same gate as HandleEvent step 9.
	cfg, err := config.Load()
	if err != nil || !cfg.AI.AutoDelegateEnabled {
		return
	}

	for _, taskID := range taskIDs {
		r.reDeriveTask(taskID)
	}
}

func (r *Router) reDeriveTask(taskID string) {
	task, err := dbpkg.GetTask(r.db, taskID)
	if err != nil || task == nil {
		return
	}

	// Only re-derive queued tasks — claimed/delegated/done are already handled.
	if task.Status != "queued" {
		return
	}

	// No score landed — nothing to gate against.
	if task.AutonomySuitability == nil {
		return
	}

	// Fetch triggers for this event type.
	triggers, err := dbpkg.GetActiveTriggersForEvent(r.db, task.EventType)
	if err != nil {
		log.Printf("[router] re-derive: failed to query triggers for %s: %v", task.EventType, err)
		return
	}

	// Fetch the primary event's metadata for predicate matching.
	metadata, err := dbpkg.GetEventMetadata(r.db, task.PrimaryEventID)
	if err != nil {
		log.Printf("[router] re-derive: failed to fetch event metadata for %s: %v", task.PrimaryEventID, err)
		return
	}

	for _, trigger := range triggers {
		// Only process deferred triggers — immediate ones already fired in HandleEvent.
		if trigger.MinAutonomySuitability <= 0 {
			continue
		}

		// Autonomy gate.
		if *task.AutonomySuitability < trigger.MinAutonomySuitability {
			log.Printf("[router] re-derive: task %s suitability %.2f < trigger %s threshold %.2f, skipping",
				taskID, *task.AutonomySuitability, trigger.ID, trigger.MinAutonomySuitability)
			continue
		}

		// Predicate match — same logic as HandleEvent step 6.
		predJSON := ""
		if trigger.ScopePredicateJSON != nil {
			predJSON = *trigger.ScopePredicateJSON
		}
		matched, err := matchPredicate(task.EventType, predJSON, metadata)
		if err != nil {
			log.Printf("[router] re-derive: trigger %s predicate error: %v", trigger.ID, err)
			continue
		}
		if !matched {
			continue
		}

		log.Printf("[router] re-derive: task %s suitability %.2f >= trigger %s threshold %.2f, firing",
			taskID, *task.AutonomySuitability, trigger.ID, trigger.MinAutonomySuitability)
		// Triggering event for the queued firing is the task's primary
		// event — that's the one whose match scored autonomously above
		// threshold. Real-event provenance keeps the audit trail honest
		// when a re-derived firing ends up enqueued.
		r.tryAutoDelegate(task, trigger, task.EntityID, task.PrimaryEventID)
	}
}

// --- Inline close checks --------------------------------------------------

// cancelActiveRunsForTask asks the spawner to abort any non-terminal runs
// on the task. Called before a task transitions to done/dismissed so the
// agent stops work on a task the system has decided is resolved.
//
// Errors are logged and swallowed. "no active run" from the spawner is
// expected when a run races us to natural completion between the DB
// lookup and the cancel call — the run ends up terminal either way and
// the task close will still land. Cancellation itself is fire-and-forget;
// the spawner's handleCancelled writes the cancelled status asynchronously.
func (r *Router) cancelActiveRunsForTask(taskID string) {
	if r.spawner == nil {
		return
	}
	ids, err := dbpkg.ActiveRunIDsForTask(r.db, taskID)
	if err != nil {
		log.Printf("[router] active-run lookup for task %s failed: %v", taskID, err)
		return
	}
	for _, id := range ids {
		if err := r.spawner.Cancel(id); err != nil {
			log.Printf("[router] cancel run %s on close of task %s: %v", id, taskID, err)
		}
	}
}

// closeEntity cascades entity → tasks → runs: enumerate active tasks,
// cancel any in-flight run on each, then flip the entity to closed and
// batch-close its tasks with close_reason="entity_closed".
//
// Cancellation happens before the task close SQL so the spawner stops
// work as promptly as possible. The cancel is async (handleCancelled
// runs off a context done channel) but the task row is authoritative —
// subsequent callers see 'done' immediately, and the run lands on
// 'cancelled' when its goroutine unwinds.
func (r *Router) closeEntity(entityID string) (int, error) {
	if tasks, err := dbpkg.FindActiveTasksByEntity(r.db, entityID); err != nil {
		// Non-fatal: better to cascade-close the entity than to abort
		// because we couldn't enumerate tasks for cancellation. Any
		// orphaned runs can be cleaned up by the existing startup
		// worktree.Cleanup pass.
		log.Printf("[router] entity close: list active tasks for %s failed: %v", entityID, err)
	} else {
		for _, t := range tasks {
			r.cancelActiveRunsForTask(t.ID)
		}
	}

	if err := dbpkg.CloseEntity(r.db, entityID); err != nil {
		return 0, err
	}
	closed, err := dbpkg.CloseAllEntityTasks(r.db, entityID, "entity_closed")
	if err != nil {
		return closed, err
	}
	if closed > 0 {
		log.Printf("[lifecycle] entity %s closed → %d tasks cascade-closed", entityID, closed)
	}
	return closed, nil
}

// closeTaskWithAudit closes a task and records the closing event in task_events
// so the full close timeline is reconstructable. All inline close checks use
// this instead of calling dbpkg.CloseTask directly.
//
// Also cancels any in-flight run on the task — task state is the
// authoritative invalidation surface, so runs and queued firings derive
// from it. Without the cancel, an inline close check that closes a task
// mid-run would leave the agent churning on work the system already
// considers resolved.
func (r *Router) closeTaskWithAudit(taskID, closingEventID, closeReason, closeEventType string) error {
	r.cancelActiveRunsForTask(taskID)
	if err := dbpkg.CloseTask(r.db, taskID, closeReason, closeEventType); err != nil {
		return err
	}
	if closingEventID != "" {
		_ = dbpkg.RecordTaskEvent(r.db, taskID, closingEventID, "closed")
	}
	return nil
}

func (r *Router) runInlineCloseChecks(evt domain.Event, entityID string) {
	switch evt.EventType {
	case domain.EventGitHubPRCICheckPassed:
		r.closeCheckCIPassed(evt, entityID)
	case domain.EventGitHubPRReviewApproved,
		domain.EventGitHubPRReviewCommented,
		domain.EventGitHubPRReviewDismissed:
		r.closeCheckReviewResolved(evt, entityID)
	case domain.EventGitHubPRReviewSubmitted:
		r.closeCheckReviewSubmitted(evt, entityID)
	case domain.EventJiraIssueAssigned:
		r.closeCheckJiraReassigned(evt, entityID)
	}
}

// closeCheckCIPassed: if no failing check-runs remain on this entity at the
// latest SHA, close active ci_check_failed tasks.
func (r *Router) closeCheckCIPassed(evt domain.Event, entityID string) {
	// Parse metadata to get head_sha.
	var meta events.GitHubPRCICheckPassedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return
	}

	// Query: any active ci_check_failed tasks still open on this entity?
	failedTasks, err := dbpkg.FindActiveTasksByEntityAndType(r.db, entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil || len(failedTasks) == 0 {
		return
	}

	// Check entity snapshot for remaining failures at the current SHA.
	entity, err := dbpkg.GetEntity(r.db, entityID)
	if err != nil || entity == nil {
		return
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		return
	}

	// If any check is still failing at the latest SHA, don't close.
	for _, cr := range snap.CheckRuns {
		if domain.IsFailingConclusion(cr.Conclusion) {
			return
		}
	}

	// All green — close the failure tasks.
	for _, t := range failedTasks {
		if err := r.closeTaskWithAudit(t.ID, evt.ID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed); err != nil {
			log.Printf("[router] failed to close ci_check_failed task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (ci_check_failed → ci_check_passed)", t.ID)
		}
	}
}

// closeCheckReviewResolved: if the reviewer's prior state was
// changes_requested and no other reviewer still has outstanding
// changes_requested, close active review_changes_requested tasks.
func (r *Router) closeCheckReviewResolved(evt domain.Event, entityID string) {
	// We need to know which reviewer just changed state. Parse metadata.
	var reviewer string
	switch evt.EventType {
	case domain.EventGitHubPRReviewApproved:
		var meta events.GitHubPRReviewApprovedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			return
		}
		reviewer = meta.Reviewer
	case domain.EventGitHubPRReviewCommented:
		var meta events.GitHubPRReviewCommentedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			return
		}
		reviewer = meta.Reviewer
	case domain.EventGitHubPRReviewDismissed:
		var meta events.GitHubPRReviewDismissedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			return
		}
		reviewer = meta.Reviewer
	}
	if reviewer == "" {
		return
	}

	// Check entity snapshot: does this reviewer's prior state include
	// changes_requested, and is no other reviewer still requesting changes?
	entity, err := dbpkg.GetEntity(r.db, entityID)
	if err != nil || entity == nil {
		return
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		return
	}

	anyOutstandingChanges := false
	for _, rs := range snap.Reviews {
		if rs.State == "CHANGES_REQUESTED" && rs.Author != reviewer {
			anyOutstandingChanges = true
			break
		}
	}
	if anyOutstandingChanges {
		return
	}

	// Close review_changes_requested tasks on this entity.
	tasks, err := dbpkg.FindActiveTasksByEntityAndType(r.db, entityID, domain.EventGitHubPRReviewChangesRequested)
	if err != nil {
		return
	}
	for _, t := range tasks {
		if err := r.closeTaskWithAudit(t.ID, evt.ID, "auto_closed_by_event", evt.EventType); err != nil {
			log.Printf("[router] failed to close changes_requested task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (review resolved by %s)", t.ID, reviewer)
		}
	}
}

// closeCheckReviewSubmitted: if I submitted my review, close any active
// review_requested task on this entity (the request is satisfied).
func (r *Router) closeCheckReviewSubmitted(evt domain.Event, entityID string) {
	var meta events.GitHubPRReviewSubmittedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return
	}
	if !meta.ReviewerIsSelf {
		return
	}

	tasks, err := dbpkg.FindActiveTasksByEntityAndType(r.db, entityID, domain.EventGitHubPRReviewRequested)
	if err != nil {
		return
	}
	for _, t := range tasks {
		if err := r.closeTaskWithAudit(t.ID, evt.ID, "auto_closed_by_event", domain.EventGitHubPRReviewSubmitted); err != nil {
			log.Printf("[router] failed to close review_requested task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (review submitted by self)", t.ID)
		}
	}
}

// closeCheckJiraReassigned: when a Jira issue is assigned to someone who is
// NOT self, close any active jira:issue:assigned or jira:issue:available
// tasks on this entity.
func (r *Router) closeCheckJiraReassigned(evt domain.Event, entityID string) {
	var meta events.JiraIssueAssignedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return
	}
	if meta.AssigneeIsSelf {
		return // assigned to me — not a reassignment-away
	}

	// Close active assigned tasks.
	for _, eventType := range []string{domain.EventJiraIssueAssigned, domain.EventJiraIssueAvailable} {
		tasks, err := dbpkg.FindActiveTasksByEntityAndType(r.db, entityID, eventType)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if err := r.closeTaskWithAudit(t.ID, evt.ID, "auto_closed_by_event", domain.EventJiraIssueAssigned); err != nil {
				log.Printf("[router] failed to close %s task %s: %v", eventType, t.ID, err)
			} else {
				log.Printf("[router] inline-closed task %s (jira reassigned away)", t.ID)
			}
		}
	}
}

// --- Predicate matching ---

func matchPredicate(eventType, predJSON, metaJSON string) (bool, error) {
	schema, ok := events.Get(eventType)
	if !ok {
		// Unknown event type — can't match. Not an error per se; system events
		// don't have schemas and won't match any rules.
		return false, nil
	}
	return schema.Match(predJSON, metaJSON)
}
