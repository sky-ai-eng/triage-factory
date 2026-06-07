package routing

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// Scorer is the minimal interface the router needs from the AI runner.
// Trigger takes the event's orgID so the per-org scoring Manager kicks
// the right tenant's Runner — a single-Trigger() across all orgs would
// head-of-line-block min_autonomy_suitability triggers on the slowest
// org's scoring cycle.
type Scorer interface {
	Trigger(orgID string)
}

// Delegator is the minimal interface the router needs from the delegate
// spawner — kicking off a run, plus cancelling one. Narrowed from
// *delegate.Spawner so tests can stub the spawn surface without bringing
// up a worktree, the agent subprocess, etc. Production wiring passes a
// *delegate.Spawner.
type Delegator interface {
	Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error)
	Cancel(orgID, runID, userID string) error
}

// Router is the central eventbus subscriber that replaces the old auto-
// delegate hook. On every event it:
//  1. Records the event (durable audit log)
//  2. Handles entity lifecycle (merged/closed → close entity + all tasks)
//  3. Guards against closed entities (no task creation on dead PRs)
//  4. Matches event_handlers (rules + triggers, unified post-SKY-259) via
//     typed predicates — one store query, kind-discriminated handling
//  5. Dedup-creates or bumps tasks
//  6. Enqueues AI scoring
//  7. Auto-delegates on matching triggers — fires if the entity is idle,
//     enqueues onto pending_firings if the entity already has an active
//     auto run or earlier queued firings (SKY-189).
//  8. Runs inline close checks for the event type
type Router struct {
	prompts      dbpkg.PromptStore
	blueprints   dbpkg.BlueprintStore // resolves a trigger's blueprint → first step prompt for the per-(entity, prompt) breaker
	handlers     dbpkg.EventHandlerStore
	agents       dbpkg.AgentStore
	teamAgents   dbpkg.TeamAgentStore        // SKY-261: read team_agents.enabled before auto-firing triggers
	users        dbpkg.UsersStore            // SKY-270: read local user's jira_account_id for inline close gates
	tasks        dbpkg.TaskStore             // SKY-283: task lifecycle, dedup, claims, breaker
	agentRuns    dbpkg.AgentRunStore         // SKY-285: lookup active runs for the task-close cancel cascade
	entities     dbpkg.EntityStore           // SKY-284: closed-entity guard + entity-terminating close cascade
	firings      dbpkg.PendingFiringsStore   // SKY-289: per-entity firing queue + active-run gate
	events       dbpkg.EventStore            // SKY-305: admin-pool RecordSystem + GetMetadataSystem for the background subscriber
	eventQueue   dbpkg.EventQueueStore       // durable router queue the drain worker claims from; set post-construction via SetEventQueue (nil → worker is a no-op)
	orgs         dbpkg.OrgsStore             // per-org iteration for the drain sweeper; nil-safe, falls back to N=1 sentinel when unset
	teams        dbpkg.TeamsStore            // per-team auto_delegate_enabled kill-switch read post-internal/config deletion
	teamRepos    dbpkg.TeamGitHubReposStore  // team↔repo tracking gate; nil-safe — gate is skipped (no filtering) when unset
	jiraRules    dbpkg.JiraStatusRulesStore  // team↔project tracking gate; nil-safe — Jira gate skipped when unset
	githubGroups dbpkg.TeamGitHubGroupsStore // github-team→TF-team mapping; resolves review_requested team visibility. nil-safe — review routing degrades to handler-team visibility when unset
	spawner      Delegator
	scorer       Scorer
	ws           *websocket.Hub

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

// NewRouter creates a Router. teamAgents is nil-safe — callers wired
// before SKY-261 D-Claims can pass nil; the bot-disabled-team check
// degrades to "always enabled" in that case (the pre-SKY-261
// behavior). users is nil-safe too — the SKY-270 inline-close gate
// degrades to "treat every reassignment as away-from-me" when missing,
// which over-closes (acceptable: user can reopen via the next poll).
// orgs is nil-safe — the drain sweeper collapses to a single-org pass
// over the local sentinel when missing, matching pre-D9 behavior.
// teamRepos is nil-safe — the SKY-375 team↔repo gate is skipped (no
// handler is dropped) when missing, matching pre-SKY-375 behavior where
// repos were org-global and every team implicitly tracked them all.
// jiraRules is nil-safe the same way — the team↔project gate is
// skipped when missing. githubGroups is nil-safe — review_requested
// visibility routing degrades to handler-team visibility (the
// pre-ticket behavior) when missing or when an event carries no requested
// identity.
func NewRouter(prompts dbpkg.PromptStore, blueprints dbpkg.BlueprintStore, handlers dbpkg.EventHandlerStore, agents dbpkg.AgentStore, teamAgents dbpkg.TeamAgentStore, users dbpkg.UsersStore, tasks dbpkg.TaskStore, agentRuns dbpkg.AgentRunStore, entities dbpkg.EntityStore, firings dbpkg.PendingFiringsStore, events dbpkg.EventStore, orgs dbpkg.OrgsStore, teams dbpkg.TeamsStore, teamRepos dbpkg.TeamGitHubReposStore, jiraRules dbpkg.JiraStatusRulesStore, githubGroups dbpkg.TeamGitHubGroupsStore, spawner Delegator, scorer Scorer, ws *websocket.Hub) *Router {
	return &Router{
		prompts:      prompts,
		blueprints:   blueprints,
		handlers:     handlers,
		agents:       agents,
		teamAgents:   teamAgents,
		users:        users,
		tasks:        tasks,
		agentRuns:    agentRuns,
		entities:     entities,
		firings:      firings,
		events:       events,
		orgs:         orgs,
		teams:        teams,
		teamRepos:    teamRepos,
		jiraRules:    jiraRules,
		githubGroups: githubGroups,
		spawner:      spawner,
		scorer:       scorer,
		ws:           ws,
		drainLocks:   make(map[string]*sync.Mutex),
	}
}

// breakerPromptID resolves the prompt id the auto-delegate breaker keys on
// for a trigger. The trigger fires a blueprint; the breaker tracks failed
// runs per (entity, prompt), and runs are prompt-keyed, so it tracks the
// blueprint's first step prompt — exactly the wrapped prompt for the 1-step
// blueprints every shipped trigger uses. Returns "" when the blueprint or its
// steps can't be resolved (CountConsecutiveFailedRunsSystem then matches no
// runs → breaker never trips, the safe default that doesn't block
// auto-delegation on a transient read error). nil-safe on the store.
func (r *Router) breakerPromptID(orgID, blueprintID string) string {
	if r.blueprints == nil || blueprintID == "" {
		return ""
	}
	steps, err := r.blueprints.ListStepsSystem(context.Background(), orgID, blueprintID)
	if err != nil || len(steps) == 0 {
		return ""
	}
	return steps[0].StepPromptID
}

// SetEventQueue wires the durable router queue post-
// construction, mirroring the spawner.SetQueueDrainer pattern. The drain
// worker (RunEventQueue) claims from this store; the ingestor enqueues to
// it. Kept off NewRouter's already-wide signature — it's a late-bound dep
// only the worker needs, and leaving it nil makes RunEventQueue a no-op so
// the ~30 existing test constructions don't have to thread it.
func (r *Router) SetEventQueue(q dbpkg.EventQueueStore) {
	r.eventQueue = q
}

// autoDelegateEnabledForTeam returns the team's auto_delegate_enabled
// kill switch. TeamsStore.GetSettingsSystem returns
// domain.DefaultTeamSettings() on missing rows, so a missing team
// settings row reads as false (the schema default; multi-mode's "new
// teams require explicit opt-in" rule). The local-mode sentinel team's
// baseline seed flips this to true, preserving the local-first
// happy-path behavior. Nil teams store (test wiring) or store error
// degrades to false — safer than the prior "fail open and silently
// auto-delegate" behavior the deleted internal/config had.
func (r *Router) autoDelegateEnabledForTeam(ctx context.Context, teamID string) bool {
	if r.teams == nil || teamID == "" {
		return false
	}
	settings, err := r.teams.GetSettingsSystem(ctx, teamID)
	if err != nil {
		log.Printf("[router] auto_delegate gate: team %s settings read: %v (defaulting to disabled)", teamID, err)
		return false
	}
	return settings.AutoDelegateEnabled
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

// HandleEvent routes a single event: entity lifecycle, dedup task
// creation/bump, inline close checks, and auto-delegation. In production
// it is invoked by the durable event-queue drain worker with an
// already-persisted event (evt.ID set) — it is no longer an eventbus
// subscriber. It is also safe to call directly with an unpersisted event
// (step 1 records it first); that path is used by tests.
//
// Processing is best-effort and idempotent w.r.t. re-delivery: the queue
// is at-least-once (a crash mid-process replays the event), and the task
// dedup index makes a replay a no-op rather than a double-create.
func (r *Router) HandleEvent(evt domain.Event) {
	// Defensive: every upstream emitter (poller, per-org loop in
	// SKY-312) tags events with evt.OrgID. A missing OrgID indicates
	// an emitter bug — failing loud here prevents tenant-mixed writes
	// that would silently land on the local sentinel. The check lives
	// here at the single entry point; downstream helpers take orgID
	// as a typed parameter and trust it.
	if evt.OrgID == "" {
		log.Printf("[router] dropping event %s with no OrgID — emitter bug", evt.EventType)
		return
	}
	orgID := evt.OrgID

	// Step 1: ensure the event is persisted. In production the ingestor
	// wrote the events row at enqueue (the durable outbox) and the
	// drain worker loaded it via EventStore.GetSystem, so evt.ID is
	// already set — recording is decoupled from routing and we route the
	// pre-persisted event as-is. As a safety net for a caller that hands
	// us an unpersisted event (no id — e.g. a direct test call), we record
	// it here. Later routing relies on evt.ID referring to a real row, so
	// stop if that insert fails.
	if evt.ID == "" {
		id, err := r.events.RecordSystem(context.Background(), orgID, evt)
		if err != nil {
			log.Printf("[router] failed to record event %s: %v", evt.EventType, err)
			return
		}
		evt.ID = id
	}

	// Step 2: Entity lifecycle — entity-terminating events close the entity
	// and cascade-close all its tasks. Return after — no task creation on a
	// closing entity.
	if evt.EntityID != nil && EntityTerminatingEvents[evt.EventType] {
		closed, err := r.closeEntity(orgID, *evt.EntityID)
		if err != nil {
			log.Printf("[router] entity lifecycle error for %s: %v", *evt.EntityID, err)
		}
		if closed > 0 {
			r.ws.Broadcast(websocket.Event{Type: "tasks_updated", OrgID: orgID, Data: map[string]any{}})
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
	entity, err := r.entities.GetSystem(context.Background(), orgID, entityID)
	if err != nil || entity == nil {
		log.Printf("[router] failed to load entity %s: %v", entityID, err)
		return
	}
	if entity.State != "active" {
		return
	}

	// Inline close checks run unconditionally — they are lifecycle signals
	// that close stale tasks. They must fire even when no task_rules or
	// triggers match the event, because close-signal events (ci_check_passed,
	// a submitted review, review_request_removed) are not task-creating events.
	if r.runInlineCloseChecks(orgID, evt, entityID) {
		r.ws.Broadcast(websocket.Event{Type: "tasks_updated", OrgID: orgID, Data: map[string]any{}})
	}

	// Step 5: Match event_handlers (rules + triggers, unified) for this
	// event type. One query, kind-discriminated locally — preserves the
	// pre-SKY-259 rules-before-triggers order via the store's kind-ASC
	// ORDER BY.
	handlers, err := r.handlers.GetEnabledForEventSystem(context.Background(), orgID, evt.EventType)
	if err != nil {
		log.Printf("[router] failed to query event_handlers for %s: %v", evt.EventType, err)
	}

	var (
		matchedRules    []domain.EventHandler
		matchedTriggers []domain.EventHandler
	)
	// scopeCache memoizes the team↔scope (e.g. team↔repo) gate per team
	// for this one event, so a team with several matching handlers does a
	// single tracking lookup. Built once here and threaded through every
	// handlerScopeMatchesEvent call below.
	scopeCache := map[string]bool{}
	for _, h := range handlers {
		predJSON := ""
		if h.ScopePredicateJSON != nil {
			predJSON = *h.ScopePredicateJSON
		}
		matched, err := matchPredicate(evt.EventType, predJSON, evt.MetadataJSON)
		if err != nil {
			log.Printf("[router] event_handler %s (%s) predicate error: %v", h.ID, h.Kind, err)
			continue
		}
		if !matched {
			continue
		}
		// Team↔scope gate (SKY-375 repos / SKY-376 Jira projects): a
		// team's handler only fires for events whose entity the team
		// tracks. Dropped here — before the
		// team grouping below — so the team never enters the visibility
		// set, its triggers never fire, and SKY-368's task_teams excludes
		// it for free. System/org-union handlers (NULL team_id) skip the
		// gate. A dropped handler is silently not-matched, same as a
		// predicate miss.
		if !r.handlerScopeMatchesEvent(evt, h, scopeCache) {
			continue
		}
		switch h.Kind {
		case domain.EventHandlerKindRule:
			matchedRules = append(matchedRules, h)
		case domain.EventHandlerKindTrigger:
			matchedTriggers = append(matchedTriggers, h)
		}
	}

	// Nothing matched — event is recorded but no task created.
	if len(matchedRules) == 0 && len(matchedTriggers) == 0 {
		return
	}

	// Step 7: Find or create ONE task for this (entity, event_type,
	// dedup_key). The teams whose handlers matched are the visibility
	// set, not a count — one situation, one task, many teams can see
	// it. Group matched handlers by team so step 9 can fire each
	// team's triggers against the single task, and so the owner pick +
	// visibility set can be computed. Org-visibility handlers (system-
	// shipped rules with team_id NULL) route to LocalDefaultTeamID via
	// handlerTeamID; the Postgres store resolves that sentinel to the
	// org's canonical team.
	teamRules := map[string][]domain.EventHandler{}
	teamTriggers := map[string][]domain.EventHandler{}
	for _, h := range matchedRules {
		teamRules[handlerTeamID(h)] = append(teamRules[handlerTeamID(h)], h)
	}
	for _, h := range matchedTriggers {
		teamTriggers[handlerTeamID(h)] = append(teamTriggers[handlerTeamID(h)], h)
	}

	// The visibility set: every team that had any matching handler.
	// Sorted so the task_teams writes are deterministic.
	visibleTeams := make([]string, 0, len(teamRules)+len(teamTriggers))
	seen := map[string]struct{}{}
	for t := range teamRules {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			visibleTeams = append(visibleTeams, t)
		}
	}
	for t := range teamTriggers {
		if _, ok := seen[t]; !ok {
			seen[t] = struct{}{}
			visibleTeams = append(visibleTeams, t)
		}
	}
	sort.Strings(visibleTeams)

	// orderedTeams ranks the matched teams by per-team rule priority
	// (desc), ties broken by lowest team id. The owner is the first —
	// the highest-priority matched team — and step 9 fires triggers in
	// this same order, so the team that wins the exclusive claim (and
	// thus becomes the consolidated owner) is deterministic and matches
	// the creation-time owner pick. A team with only triggers
	// contributes the 0.5 trigger-fallback priority.
	orderedTeams := make([]string, len(visibleTeams))
	copy(orderedTeams, visibleTeams)
	teamScore := func(teamID string) float64 {
		s := 0.5
		for _, rule := range teamRules[teamID] {
			if rule.DefaultPriority != nil && *rule.DefaultPriority > s {
				s = *rule.DefaultPriority
			}
		}
		return s
	}
	sort.SliceStable(orderedTeams, func(i, j int) bool {
		si, sj := teamScore(orderedTeams[i]), teamScore(orderedTeams[j])
		if si != sj {
			return si > sj
		}
		return orderedTeams[i] < orderedTeams[j]
	})
	ownerTeam := orderedTeams[0]
	taskPriority := teamScore(ownerTeam)

	// Resolve the task's owner team, visibility set, firing order, and
	// creation-seed priority. Three routing axes, distinct from the
	// handler-team grouping computed above:
	//   - review_requested → the requested reviewer's team(s) (a reviewer's
	//     personal obligation), scoped at emit time;
	//   - author-centric github events → the entity's owning team via the
	//     owning-team ladder (the PR author's CI/conflict/feedback lifecycle);
	//   - everything else (Jira, etc.) → the handler-team grouping above.
	// The matched handlers above still gate whether a task is created at all
	// and supply the priority; only the team set is overridden here.
	switch {
	case evt.EventType == domain.EventGitHubPRReviewRequested:
		// Falls back to the handler-team visibility above for legacy events
		// that carry no requested identity or when the mapping stores are
		// unwired (scoped=false).
		if reqTeams, scoped := r.reviewRequestVisibilityTeams(orgID, evt); scoped {
			if len(reqTeams) == 0 {
				log.Printf("[router] review_requested on entity %s: requested identity maps to no TF team — recording event, no task", entityID)
				return
			}
			visibleTeams = reqTeams
			orderedTeams = reqTeams // already sorted by team id in the helper
			ownerTeam = reqTeams[0]
			taskPriority = maxRuleDefaultPriority(matchedRules)
		}

	case isAuthorCentricGitHubEvent(evt.EventType):
		owner, ownerSet := r.authorCentricOwner(orgID, evt, entityID)
		// Visibility = {owner set} ∪ {teams whose explicit user-authored rule
		// matched}. System/default rules gate creation + the owner's
		// automation but never grant visibility on their own; a team widens
		// visibility beyond ownership only by opting in with its own rule
		// (the deliberate watch — e.g. a platform team tracking CI it doesn't
		// author, or surfacing an external/dependabot author's CI).
		vis := map[string]struct{}{}
		for _, t := range ownerSet {
			vis[t] = struct{}{}
		}
		for t := range explicitUserRuleTeams(matchedRules) {
			vis[t] = struct{}{}
		}
		if len(vis) == 0 {
			// Nothing resolved (external/non-TF author — dependabot, renovate,
			// outside contributors) and no team opted in via an explicit rule
			// → record the event + durable entity only, no task. This is the
			// repo-wide-poll fix: a stranger's CI failure no longer mints a
			// task for the local user/team. Deliberately silent: this is the
			// expected high-volume path (dozens of external-author events per
			// poll cycle in a busy repo) with nothing actionable — the event is
			// already durably recorded at step 1.
			return
		}
		visibleTeams = sortedKeys(vis)
		// ownerTeam "" when the author maps to multiple teams (or none with a
		// watch rule): the owner is unresolved, stored as NULL — the auto-fire
		// gate for free, resolving on the first human claim.
		ownerTeam = owner
		taskPriority = maxRuleDefaultPriority(matchedRules)
		// Only the owner's automation fires. A resolved owner fires its own
		// matched triggers (acting = owner); a NULL owner fires nothing (the
		// empty-team gate), so the bot never claims an unowned task.
		if owner != "" {
			orderedTeams = []string{owner}
		} else {
			orderedTeams = nil
		}
	}

	// became_atomic is the belated-discovery path for parents whose
	// subtasks just closed. Suppress the new card if any active task
	// already exists on the entity — otherwise an atomic ticket that
	// gained and then lost subtasks ends up with two cards. The dedup
	// index can't catch this because the existing task's event_type is
	// jira:issue:assigned while the new one is jira:issue:became_atomic.
	// The event is still recorded (step 1); only task creation is
	// skipped.
	if evt.EventType == domain.EventJiraIssueBecameAtomic {
		active, err := r.tasks.FindActiveByEntitySystem(context.Background(), orgID, entityID)
		if err != nil {
			log.Printf("[router] became_atomic: failed to check active tasks on entity %s: %v", entityID, err)
			return
		}
		if len(active) > 0 {
			log.Printf("[router] became_atomic: entity %s already has an active task, skipping duplicate creation", entityID)
			return
		}
	}

	// Task createdAt = OccurredAt when the source reported a time,
	// falling back to time.Now(). This keeps the backfill path's
	// "stamp the task with the PR's CreatedAt for week-old review
	// requests" semantic — queue ordering reflects when the world said
	// the event happened, not when we noticed.
	createdAt := time.Now()
	if !evt.OccurredAt.IsZero() {
		createdAt = evt.OccurredAt
	}
	task, created, err := r.tasks.FindOrCreateAtSystem(context.Background(), orgID, ownerTeam, entityID, evt.EventType, evt.DedupKey, evt.ID, taskPriority, createdAt)
	if err != nil {
		log.Printf("[router] failed to find/create task for %s on entity %s: %v", evt.EventType, entityID, err)
		return
	}

	// Record the visibility set transactionally with the task. Additive:
	// a re-arrival matching new teams widens visibility. Failure here is
	// logged but not fatal — the owning team_id still grants the owner
	// visibility; a follow-up event re-attempts the wider set.
	if err := r.tasks.SetVisibilityTeamsSystem(context.Background(), orgID, task.ID, visibleTeams); err != nil {
		log.Printf("[router] failed to set visibility teams for task %s: %v", task.ID, err)
	}

	if created {
		if err := r.tasks.RecordEventSystem(context.Background(), orgID, task.ID, evt.ID, "spawned"); err != nil {
			log.Printf("[router] failed to record spawned task_event: %v", err)
		}
		log.Printf("[router] created task %s (%s) on entity %s (owner team %s, %d visible teams)", task.ID, evt.EventType, entityID, ownerTeam, len(visibleTeams))
	} else {
		if err := r.tasks.BumpSystem(context.Background(), orgID, task.ID, evt.ID); err != nil {
			log.Printf("[router] failed to bump task %s: %v", task.ID, err)
		}
		if err := r.tasks.RecordEventSystem(context.Background(), orgID, task.ID, evt.ID, "bumped"); err != nil {
			log.Printf("[router] failed to record bumped task_event: %v", err)
		}
	}

	// Step 8: Enqueue AI scoring (always — produces UI metadata regardless).
	r.scorer.Trigger(orgID)

	// Broadcast task update to frontend.
	r.ws.Broadcast(websocket.Event{Type: "tasks_updated", OrgID: orgID, Data: map[string]any{}})

	// Step 9: Auto-delegate for matching triggers. Each matched team's
	// triggers fire against the single task; the exclusive claim CAS
	// resolves contention (first claimer — human or bot — wins, the
	// rest no-op). The per-team kill switch is checked per firing team.
	// Triggers with min_autonomy_suitability > 0 still defer to
	// post-scoring re-derive.
	//
	// Iterate orderedTeams (not the map) so the firing order is
	// deterministic and the first team to claim — which consolidates
	// the owner — is the highest-priority matched team, matching the
	// creation-time owner pick. The exclusive claim means only the
	// first team's trigger actually runs; later teams' triggers find
	// the task already claimed and skip inside tryAutoDelegate.
	for _, teamID := range orderedTeams {
		triggers := teamTriggers[teamID]
		if len(triggers) == 0 {
			continue
		}
		// Normalize the org-visible sentinel to the resolved owner team
		// so the kill-switch / team_agents / claim all read a real team.
		acting := effectiveActingTeam(teamID, teamIDValue(task))
		if !r.autoDelegateEnabledForTeam(context.Background(), acting) {
			continue
		}
		for _, trigger := range triggers {
			if trigger.MinAutonomySuitability != nil && *trigger.MinAutonomySuitability > 0 {
				continue // deferred to post-scoring handler
			}
			r.tryAutoDelegate(orgID, task, trigger, entityID, evt.ID, acting)
		}
	}

}

func matchPredicate(eventType, predJSON, metaJSON string) (bool, error) {
	schema, ok := events.Get(eventType)
	if !ok {
		// Unknown event type — can't match. Not an error per se; system events
		// don't have schemas and won't match any rules.
		return false, nil
	}
	return schema.Match(predJSON, metaJSON)
}
