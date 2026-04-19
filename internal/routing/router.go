package routing

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/config"
	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Scorer is the minimal interface the router needs from the AI runner.
type Scorer interface {
	Trigger()
}

// Router is the central eventbus subscriber that replaces the old auto-
// delegate hook. On every event it:
//  1. Records the event (durable audit log)
//  2. Handles entity lifecycle (merged/closed → close entity + all tasks)
//  3. Guards against closed entities (no task creation on dead PRs)
//  4. Matches task_rules + prompt_triggers via typed predicates
//  5. Dedup-creates or bumps tasks
//  6. Enqueues AI scoring
//  7. Auto-delegates on matching triggers (with breaker + cooldown gates)
//  8. Runs inline close checks for the event type
type Router struct {
	db      *sql.DB
	spawner *delegate.Spawner
	scorer  Scorer
	ws      *websocket.Hub
}

// NewRouter creates a Router.
func NewRouter(db *sql.DB, spawner *delegate.Spawner, scorer Scorer, ws *websocket.Hub) *Router {
	return &Router{db: db, spawner: spawner, scorer: scorer, ws: ws}
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
		closed, err := HandleEntityClose(r.db, *evt.EntityID)
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
	if cfg, err := config.Load(); err != nil || !cfg.AI.AutoDelegateEnabled {
		// Disabled or config error — skip auto-delegation entirely.
		// Inline close checks still run below.
	} else if created {
		// Auto-delegate fires immediately on task creation (cooldown doesn't
		// gate the first fire). Only triggers with min_autonomy_suitability==0
		// fire now; gated triggers defer to post-scoring re-derivation (SKY-181).
		for _, trigger := range matchedTriggers {
			if trigger.MinAutonomySuitability > 0 {
				continue // deferred to post-scoring handler
			}
			r.tryAutoDelegate(task, trigger, entityID)
		}
	} else {
		// Bump path: cooldown gates re-fires. Only fire if cooldown has elapsed.
		for _, trigger := range matchedTriggers {
			if trigger.MinAutonomySuitability > 0 {
				continue
			}
			r.tryAutoDelegateWithCooldown(task, trigger, entityID)
		}
	}

	// Step 10: Inline close checks.
	r.runInlineCloseChecks(evt, entityID)
}

// tryAutoDelegate fires a trigger immediately (no cooldown check — used on
// task creation). Checks breaker + in-flight gate only.
func (r *Router) tryAutoDelegate(task *domain.Task, trigger domain.PromptTrigger, entityID string) {
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

	// In-flight gate: don't stack runs on the same task.
	active, err := dbpkg.HasActiveRunForTask(r.db, task.ID)
	if err != nil {
		log.Printf("[router] in-flight check error for task %s: %v", task.ID, err)
		return
	}
	if active {
		return
	}

	r.fireDelegate(task, trigger)
}

// tryAutoDelegateWithCooldown is like tryAutoDelegate but also checks the
// cooldown window. Used on bump events.
func (r *Router) tryAutoDelegateWithCooldown(task *domain.Task, trigger domain.PromptTrigger, entityID string) {
	// Cooldown gate.
	if trigger.CooldownSeconds > 0 {
		lastStart, err := dbpkg.LastAutoRunStartedAt(r.db, task.ID)
		if err != nil {
			log.Printf("[router] cooldown query error for task %s: %v", task.ID, err)
			return
		}
		if lastStart != nil {
			elapsed := int(time.Since(*lastStart).Seconds())
			if elapsed < trigger.CooldownSeconds {
				log.Printf("[router] cooldown active for task %s (%ds remaining)",
					task.ID, trigger.CooldownSeconds-elapsed)
				return
			}
		}
	}

	r.tryAutoDelegate(task, trigger, entityID)
}

// fireDelegate transitions the task to delegated status, broadcasts the
// change, then fires the spawner.
func (r *Router) fireDelegate(task *domain.Task, trigger domain.PromptTrigger) {
	if r.spawner == nil {
		log.Printf("[router] spawner not configured, skipping delegation for task %s", task.ID)
		return
	}

	// Transition task queued → delegated BEFORE spawning so the frontend
	// reflects the state change immediately and dedup logic sees it.
	if err := dbpkg.SetTaskStatus(r.db, task.ID, "delegated"); err != nil {
		log.Printf("[router] failed to set task %s to delegated: %v", task.ID, err)
		return
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
		log.Printf("[router] failed to re-read task %s for delegation: %v", task.ID, err)
		r.revertTaskStatus(task.ID, "queued")
		return
	}

	runID, err := r.spawner.Delegate(*fresh, trigger.PromptID, "event", trigger.ID)
	if err != nil {
		log.Printf("[router] delegation failed for task %s: %v", task.ID, err)
		r.revertTaskStatus(task.ID, "queued")
		return
	}
	log.Printf("[router] started run %s for task %s", runID, task.ID)
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
		r.tryAutoDelegate(task, trigger, task.EntityID)
	}
}

// --- Inline close checks --------------------------------------------------

// closeTaskWithAudit closes a task and records the closing event in task_events
// so the full close timeline is reconstructable. All inline close checks use
// this instead of calling dbpkg.CloseTask directly.
func (r *Router) closeTaskWithAudit(taskID, closingEventID, closeReason, closeEventType string) error {
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
