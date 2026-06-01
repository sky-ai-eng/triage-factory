package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	dbpkg "github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
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
	handlers     dbpkg.EventHandlerStore
	agents       dbpkg.AgentStore
	teamAgents   dbpkg.TeamAgentStore        // SKY-261: read team_agents.enabled before auto-firing triggers
	users        dbpkg.UsersStore            // SKY-270: read local user's jira_account_id for inline close gates
	tasks        dbpkg.TaskStore             // SKY-283: task lifecycle, dedup, claims, breaker
	agentRuns    dbpkg.AgentRunStore         // SKY-285: lookup active runs for the task-close cancel cascade
	entities     dbpkg.EntityStore           // SKY-284: closed-entity guard + entity-terminating close cascade
	firings      dbpkg.PendingFiringsStore   // SKY-289: per-entity firing queue + active-run gate
	events       dbpkg.EventStore            // SKY-305: admin-pool RecordSystem + GetMetadataSystem for the background subscriber
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
func NewRouter(prompts dbpkg.PromptStore, handlers dbpkg.EventHandlerStore, agents dbpkg.AgentStore, teamAgents dbpkg.TeamAgentStore, users dbpkg.UsersStore, tasks dbpkg.TaskStore, agentRuns dbpkg.AgentRunStore, entities dbpkg.EntityStore, firings dbpkg.PendingFiringsStore, events dbpkg.EventStore, orgs dbpkg.OrgsStore, teams dbpkg.TeamsStore, teamRepos dbpkg.TeamGitHubReposStore, jiraRules dbpkg.JiraStatusRulesStore, githubGroups dbpkg.TeamGitHubGroupsStore, spawner Delegator, scorer Scorer, ws *websocket.Hub) *Router {
	return &Router{
		prompts:      prompts,
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

// HandleEvent is the eventbus subscriber callback. Called asynchronously
// from the bus's per-subscriber goroutine.
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

	// Step 1: Always record — durable audit log regardless of routing outcome.
	// Later routing logic relies on evt.ID referring to a persisted event row,
	// so stop here if the insert fails.
	id, err := r.events.RecordSystem(context.Background(), orgID, evt)
	if err != nil {
		log.Printf("[router] failed to record event %s: %v", evt.EventType, err)
		return
	}
	evt.ID = id

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

	// A review_requested task's visibility (and owner) come from the
	// *requested identity's* team(s), not from which teams' rules matched. A
	// review is the personal obligation of whoever was asked — routed to their
	// team(s) via the user→teams and github-team→TF-team lookups. The
	// matched rule above still gated whether the task is created and supplies
	// the priority; only the team set is overridden here. Falls back to the
	// handler-team visibility above for legacy events that carry no requested
	// identity or when the mapping stores are unwired.
	if evt.EventType == domain.EventGitHubPRReviewRequested {
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
		acting := effectiveActingTeam(teamID, task.TeamID)
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

// handlerTeamID resolves the team a matched event_handler routes its
// tasks to. Post-SKY-295 every handler is team-scoped — user-source
// rows carry the user's team, system-source rows are materialized
// into each team at boot / team-create time. An empty TeamID here
// indicates a pre-SKY-295 org-visibility row that survived a partial
// migration or a test fixture that bypassed the materialization
// path; we log it once per call but fall back to
// runmode.LocalDefaultTeamID so the router keeps functioning. In
// steady state this branch is unreachable.
// handlerScopeMatchesEvent reports whether handler h's team is allowed
// to act on evt given the team's tracking scope — the team↔repo gate
// (SKY-375, GitHub) and team↔project gate (SKY-376, Jira). It is the
// security teeth that keeps a team's handlers from firing on entities the
// team doesn't track once polling goes org-wide.
//
// Escape hatches return true (no drop):
//   - System/org-union handlers (NULL team_id) — they're scoped to the
//     org-wide union by construction; the event's entity is in the union
//     because *some* team tracks it, so gating them would always pass
//     anyway.
//   - The relevant tracking store unwired (nil) — callers from before the
//     gate / tests that don't exercise it; degrades to the pre-ticket
//     behavior where every team implicitly tracked every org-global
//     entity. Handled per-source in teamTracksEventScope.
//   - Any source other than github:/jira: — ungated (no tracking concept).
//
// The per-event result is memoized in cache, keyed by team id. A single
// event has one source/entity, so the repo and project lookups for a
// given team collapse to one cache entry.
func (r *Router) handlerScopeMatchesEvent(evt domain.Event, h domain.EventHandler, cache map[string]bool) bool {
	if h.TeamID == "" {
		return true
	}
	if allowed, ok := cache[h.TeamID]; ok {
		return allowed
	}
	allowed := r.teamTracksEventScope(evt, h.TeamID)
	cache[h.TeamID] = allowed
	return allowed
}

// teamTracksEventScope dispatches the tracking lookup on the event's
// source: github: → team↔repo (SKY-375), jira: → team↔project (SKY-376).
// Each branch fails open when its store is unwired so the gate degrades
// to "no drop" in pre-ticket / test wiring; any other source is ungated.
func (r *Router) teamTracksEventScope(evt domain.Event, teamID string) bool {
	switch {
	case strings.HasPrefix(evt.EventType, "github:"):
		if r.teamRepos == nil {
			return true
		}
		return r.teamTracksEventRepo(evt, teamID)
	case strings.HasPrefix(evt.EventType, "jira:"):
		if r.jiraRules == nil {
			return true
		}
		return r.teamTracksEventProject(evt, teamID)
	default:
		return true
	}
}

// teamTracksEventRepo extracts the repo from a GitHub event's metadata
// and asks the store whether teamID tracks it. Every GitHub PR metadata
// struct carries a top-level "repo" ("owner/name"), so a minimal
// unmarshal is enough — no per-type decoding. Fail-open on a missing /
// malformed repo or a store error: dropping a legitimate task on a
// transient DB blip or an unexpected metadata shape is worse than the
// pre-ticket behavior, and the events feeding this path come from TF's
// own trusted poller.
func (r *Router) teamTracksEventRepo(evt domain.Event, teamID string) bool {
	var m struct {
		Repo string `json:"repo"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.Repo == "" {
		return true
	}
	owner, name, ok := strings.Cut(m.Repo, "/")
	if !ok || owner == "" || name == "" {
		return true
	}
	tracks, err := r.teamRepos.TracksRepoSystem(context.Background(), teamID, owner, name)
	if err != nil {
		log.Printf("[router] team↔repo gate lookup failed for team %s repo %s: %v — allowing", teamID, m.Repo, err)
		return true
	}
	return tracks
}

// teamTracksEventProject extracts the Jira project key from an event's
// metadata and asks the store whether teamID tracks it (SKY-376). Every
// Jira issue metadata struct carries a top-level "project" (the project
// key, e.g. "SKY" — see internal/domain/events/jira.go), so a minimal
// unmarshal is enough. Fail-open on a missing / malformed project or a
// store error: dropping a legitimate task on a transient DB blip or an
// unexpected metadata shape is worse than the pre-ticket behavior, and
// the events feeding this path come from TF's own trusted poller.
func (r *Router) teamTracksEventProject(evt domain.Event, teamID string) bool {
	var m struct {
		Project string `json:"project"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.Project == "" {
		return true
	}
	tracks, err := r.jiraRules.TracksProjectSystem(context.Background(), teamID, m.Project)
	if err != nil {
		log.Printf("[router] team↔project gate lookup failed for team %s project %s: %v — allowing", teamID, m.Project, err)
		return true
	}
	return tracks
}

func handlerTeamID(h domain.EventHandler) string {
	if h.TeamID != "" {
		return h.TeamID
	}
	log.Printf("[router] WARNING handler %s (%s) has no team_id — falling back to LocalDefaultTeamID; check that EventHandlerStore.Seed was called with a real teamID", h.ID, h.Kind)
	return runmode.LocalDefaultTeamID
}

// maxRuleDefaultPriority returns the highest default_priority across the
// matched rules, defaulting to the 0.5 trigger-fallback when none carry one.
// Used for review_requested, whose owner/visibility teams come from the
// requested identity rather than the rule's team — so the per-owner-team
// teamScore can't supply the priority; the matched rule(s) do.
func maxRuleDefaultPriority(rules []domain.EventHandler) float64 {
	s := 0.5
	for _, rule := range rules {
		if rule.DefaultPriority != nil && *rule.DefaultPriority > s {
			s = *rule.DefaultPriority
		}
	}
	return s
}

// reviewRequestVisibilityTeams resolves the team(s) a review_requested task is
// visible to from the *requested identity* on the event metadata — not from
// which teams' rules matched. A requested user routes to the union of teams
// over every TF user the login resolves to (the set-valued reverse lookup, the
// regression guard for a login bound to two users); a requested github team
// routes to the TF team(s) the github-team→TF-team mapping returns.
//
// scoped=true means "route via the requested-identity path"; the caller uses
// the returned teams (an empty set → drop the task: a TF-known reviewer that
// maps to no team is a config gap, not a reason to over-fan to handler teams).
// scoped=false means "fall back to handler-team visibility" — returned for
// legacy events that carry no requested identity, or when the required stores
// are unwired (test / pre-ticket wiring). Claims-free ...System lookups
// throughout: the router runs on the eventbus subscriber goroutine with no
// JWT context.
func (r *Router) reviewRequestVisibilityTeams(orgID string, evt domain.Event) (teams []string, scoped bool) {
	var meta events.GitHubPRReviewRequestedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return nil, false
	}
	if meta.RequestedLogin == "" && meta.RequestedTeam == "" {
		return nil, false // legacy event, no requested identity captured
	}

	// Unwired mapping store is test / pre-ticket wiring only — fall back to
	// handler-team visibility (scoped=false) rather than dropping the task, so
	// pre-existing review_requested fixtures that don't thread these stores
	// keep working. Production always wires them, so a wired-but-unresolvable
	// identity below takes the scoped=true path (drop, no mis-attributed task).
	set := map[string]struct{}{}
	if meta.RequestedTeam != "" {
		if r.githubGroups == nil {
			return nil, false
		}
		orgLogin, slug, ok := strings.Cut(meta.RequestedTeam, "/")
		if !ok || orgLogin == "" || slug == "" {
			return nil, true // malformed handle → no team
		}
		tids, err := r.githubGroups.TeamsForGroupSystem(context.Background(), orgID, orgLogin, slug)
		if err != nil {
			log.Printf("[router] review_requested: github-team mapping lookup for %s: %v", meta.RequestedTeam, err)
			return nil, true
		}
		for _, t := range tids {
			set[t] = struct{}{}
		}
	} else {
		if r.users == nil || r.teams == nil || r.orgs == nil {
			return nil, false
		}
		orgSet, err := r.orgs.GetSettingsSystem(context.Background(), orgID)
		if err != nil {
			log.Printf("[router] review_requested: read org settings for host: %v", err)
			return nil, true
		}
		userIDs, err := r.users.UserIDsForGitHubLoginSystem(context.Background(), orgSet.GitHubBaseURL, meta.RequestedLogin)
		if err != nil {
			log.Printf("[router] review_requested: reverse login lookup for %s: %v", meta.RequestedLogin, err)
			return nil, true
		}
		for _, uid := range userIDs {
			tids, terr := r.teams.TeamIDsForUserInOrgSystem(context.Background(), orgID, uid)
			if terr != nil {
				log.Printf("[router] review_requested: teams for user %s: %v", uid, terr)
				continue
			}
			for _, t := range tids {
				set[t] = struct{}{}
			}
		}
	}

	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, true
}

// effectiveActingTeam normalizes a handler's acting team for the
// auto-delegate gates and the bot claim. An org-visible handler routes
// the LocalDefaultTeamID sentinel through handlerTeamID, but in
// multi-mode that sentinel has no teams / team_agents / team_settings
// row of its own — the store resolves it to the org's canonical team
// for the task's owner and visibility rows. The task's owner team_id
// already carries that resolution, so fall back to it. In local mode
// the sentinel IS the real team and task.TeamID equals it, so this is a
// no-op. Without this, multi-mode org-visible auto-delegation reads the
// sentinel's (missing) team_agents row and is wrongly skipped.
func effectiveActingTeam(actingTeamID, taskTeamID string) string {
	if actingTeamID == "" || actingTeamID == runmode.LocalDefaultTeamID {
		return taskTeamID
	}
	return actingTeamID
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
func (r *Router) tryAutoDelegate(orgID string, task *domain.Task, trigger domain.EventHandler, entityID string, triggeringEventID string, actingTeamID string) {
	// Exclusive claim: one task, one owner. If the bot has already
	// claimed this task on behalf of a different team (an earlier
	// matched team won the CAS), this team's trigger must not pile on a
	// second run against the same situation — that is the cross-team
	// duplication the one-task model exists to prevent. A trigger whose
	// acting team IS the current owner still proceeds, so multiple
	// prompts one team configured on the same event all run.
	if task.ClaimedByAgentID != "" && actingTeamID != "" && task.TeamID != actingTeamID {
		log.Printf("[router] auto-trigger skipped on task %s: already claimed by the bot for team %s (acting team %s lost the claim)", task.ID, task.TeamID, actingTeamID)
		return
	}
	// SKY-261 bot-disabled-team gate. If the task's team has the bot
	// turned off in team_agents.enabled, the auto-trigger is a no-op
	// — the task is already in the team queue (created by HandleEvent
	// upstream); a human will swipe-delegate later if they want a
	// run. Skip silently rather than firing on a disabled team.
	//
	// The gate requires BOTH stores (agents to resolve the org's
	// agent, teamAgents to read the enabled flag). If either is nil
	// — pre-D-Claims test wiring that didn't thread the new stores
	// — the gate degrades to "bot enabled check unavailable, proceed
	// with auto-fire," which preserves the pre-SKY-261 behavior.
	// Production server.New / main.go always passes both.
	if r.teamAgents != nil && r.agents != nil {
		a, err := r.agents.GetForOrgSystem(context.Background(), orgID)
		if err != nil {
			log.Printf("[router] auto-trigger skipped: agent lookup: %v", err)
			return
		}
		if a == nil {
			// No bootstrapped agent — bootstrap is now fatal at
			// startup, so this shouldn't reach us in practice.
			// Log + bail rather than crashing the goroutine.
			log.Printf("[router] auto-trigger skipped on task %s: no agent bootstrapped", task.ID)
			return
		}
		// Read the bot-enabled flag for the FIRING team — the team
		// whose trigger routed the bot here — not the task's owner
		// team. One task is now visible to many teams; the gate must
		// read the acting team's own team_agents row so a two-team org
		// where team B disabled the bot doesn't auto-fire on team B by
		// reading team A's flag. Fall back to the task's owner team,
		// then the local sentinel, when the caller didn't supply one.
		teamID := actingTeamID
		if teamID == "" {
			teamID = task.TeamID
		}
		if teamID == "" {
			teamID = runmode.LocalDefaultTeamID
		}
		ta, err := r.teamAgents.GetForTeamSystem(context.Background(), orgID, teamID, a.ID)
		if err != nil {
			log.Printf("[router] auto-trigger skipped on task %s: team_agents lookup: %v", task.ID, err)
			return
		}
		if ta == nil || !ta.Enabled {
			log.Printf("[router] auto-trigger skipped on task %s: bot disabled for team", task.ID)
			return
		}
	}
	// Breaker gate. trigger.BreakerThreshold is *int because the column
	// is nullable at the schema level (rule rows have NULL); kind='trigger'
	// rows are guaranteed non-nil by the per-kind CHECK constraint.
	breakerThreshold := derefIntDefault(trigger.BreakerThreshold, 0)
	failures, err := r.tasks.CountConsecutiveFailedRunsSystem(context.Background(), orgID, entityID, trigger.PromptID)
	if err != nil {
		log.Printf("[router] breaker query error for entity %s prompt %s: %v", entityID, trigger.PromptID, err)
		return
	}
	if failures >= breakerThreshold {
		log.Printf("[router] breaker tripped for entity %s prompt %s (%d >= %d)",
			entityID, trigger.PromptID, failures, breakerThreshold)
		// Look up prompt name for the toast — opportunistic, falls back to a
		// generic message if the lookup fails since the breaker trip itself
		// is the load-bearing signal. One toast per trip (happens rarely).
		promptName := ""
		if p, perr := r.prompts.GetSystem(context.Background(), orgID, trigger.PromptID); perr == nil && p != nil {
			promptName = p.Name
		}
		if promptName == "" {
			promptName = "prompt"
		}
		toast.Warning(r.ws, orgID, fmt.Sprintf("Auto-delegation paused: %s tripped the breaker (%d consecutive failures on this entity)", promptName, failures))
		return
	}

	// Per-entity gate. Closed if any auto run is active on the entity OR
	// any pending_firings rows are already queued (FIFO fairness).
	// Compose the per-entity firing gate from its two halves:
	// AgentRunStore owns the runs-shaped predicate, PendingFiringsStore
	// owns the queue-shaped one. canFire = neither side blocks.
	gateCtx := context.Background()
	hasActive, err := r.agentRuns.HasActiveAutoRunForEntitySystem(gateCtx, orgID, entityID)
	if err != nil {
		log.Printf("[router] entity gate active-run query error for %s: %v", entityID, err)
		return
	}
	hasPending := false
	if !hasActive {
		hasPending, err = r.firings.HasPendingForEntity(gateCtx, orgID, entityID)
		if err != nil {
			log.Printf("[router] entity gate pending query error for %s: %v", entityID, err)
			return
		}
	}
	if hasActive || hasPending {
		// System-actor firing rows have no human author. Empty user
		// here lets the Postgres impl's COALESCE walk to the org-
		// owner fallback (creator_user_id is NOT NULL but the table
		// has no separate "actor" column); SQLite ignores the column
		// entirely.
		inserted, err := r.firings.Enqueue(context.Background(), orgID, "", entityID, task.ID, trigger.ID, triggeringEventID)
		if err != nil {
			log.Printf("[router] enqueue firing failed (entity %s task %s trigger %s): %v",
				entityID, task.ID, trigger.ID, err)
			return
		}
		if inserted {
			log.Printf("[router] queued firing on entity %s (task %s, trigger %s) — entity busy",
				entityID, task.ID, trigger.ID)
			// SKY-261 D-Claims: pending firing landed in the queue,
			// commit the task to the org's agent. Stamp here (after
			// EnqueuePendingFiring succeeded) so a failed enqueue
			// doesn't leave a phantom claim on an otherwise queued
			// task. !inserted means a duplicate firing already in the
			// queue — the original enqueue already stamped, no re-
			// stamp needed.
			r.stampAgentClaim(orgID, task, actingTeamID)
		} else {
			log.Printf("[router] firing collapsed on entity %s (task %s, trigger %s) — duplicate already queued",
				entityID, task.ID, trigger.ID)
		}
		return
	}

	// Consolidate the owner team to the acting team BEFORE firing. An
	// auto-fired run inherits runs.team_id from tasks.team_id at insert,
	// and the claim (which also consolidates the owner) lands only after
	// the fire succeeds — so without this, a run fired by a team other
	// than the creation-time owner (e.g. the owner had auto-delegation
	// disabled and a lower-priority team is firing) would be attributed
	// to the stale owner. Owner-only update, no claim touch: if the fire
	// fails the task is owned by the team that tried, unclaimed — not a
	// phantom claim. Skipped when the acting team already is the owner
	// (the common path, and same-team multi-prompt where an active run
	// may exist).
	if actingTeamID != "" && actingTeamID != task.TeamID {
		if err := r.tasks.SetOwnerTeamSystem(context.Background(), orgID, task.ID, actingTeamID); err != nil {
			log.Printf("[router] failed to consolidate owner team on task %s before fire: %v", task.ID, err)
		} else {
			task.TeamID = actingTeamID
		}
	}
	if _, err := r.fireDelegate(orgID, task, trigger); err != nil {
		log.Printf("[router] fire failed for task %s (trigger %s): %v", task.ID, trigger.ID, err)
		return
	}
	// SKY-261 D-Claims: fireDelegate succeeded (run inserted + spawner
	// goroutine launched) — stamp the agent claim. Done AFTER success
	// so a fireDelegate that fails + reverts to status='queued'
	// doesn't leave a phantom bot claim on a task that's back in the
	// human-triage queue.
	r.stampAgentClaim(orgID, task, actingTeamID)
}

// stampAgentClaim writes claimed_by_agent_id on a task using the org's
// agent row AND broadcasts the SKY-261 B+ task_claimed event so
// listeners (Board) can re-render the per-claim lanes. Called from
// the two commitment points in tryAutoDelegate (post-fireDelegate
// success, post-EnqueuePendingFiring success). Both paths converge
// on "the bot has committed to this task." Nil-safe on r.agents and
// on GetForOrg returning (nil, nil) — both leave the claim
// unstamped rather than crashing, which matches the "transient seam
// between db init and agent bootstrap" case noted in §4 of the spec.
//
// Uses StampAgentClaimIfUnclaimed (race-safe variant) instead of the
// unconditional setter: if a user claims the same task while the
// trigger is mid-fire, the user-claim wins and the bot silently
// loses the race. The drain path's claim_changed guard then skips
// any pending firing for this task on the next pop — so the auto-
// trigger commitment never lands, which is the right outcome when
// the human said "I'll handle this." Same-agent rewrites also
// short-circuit here, avoiding redundant task_claimed broadcasts on
// flows that call stampAgentClaim twice in quick succession.
func (r *Router) stampAgentClaim(orgID string, task *domain.Task, actingTeamID string) {
	if r.agents == nil {
		return
	}
	a, err := r.agents.GetForOrgSystem(context.Background(), orgID)
	if err != nil {
		log.Printf("[router] agent lookup failed for task %s claim stamp: %v", task.ID, err)
		return
	}
	if a == nil {
		return
	}
	ok, err := r.tasks.StampAgentClaimIfUnclaimedSystem(context.Background(), orgID, task.ID, a.ID, actingTeamID)
	if err != nil {
		log.Printf("[router] failed to stamp agent claim on task %s: %v", task.ID, err)
		return
	}
	if !ok {
		// StampAgentClaimIfUnclaimed returns ok=false for any of
		// three reasons: a user beat the bot to the claim (don't
		// steal), the bot already owns it (idempotent no-op), or
		// the task is terminal (done/dismissed — sticky claim past
		// close, refuse new mutations). All three legitimately
		// produce "skip the broadcast"; we don't disambiguate at
		// the log level because the helper doesn't surface the
		// reason and re-reading just to log is wasted I/O. If a
		// future debug session needs the breakdown, the helper can
		// grow a tri-state return or the caller can re-read the
		// task — neither is load-bearing for correctness.
		log.Printf("[router] agent claim stamp on task %s was a no-op (user owns it, bot already owns it, or task is terminal)", task.ID)
		return
	}
	task.ClaimedByAgentID = a.ID
	if actingTeamID != "" {
		// Mirror the store's owner-consolidation so the shared task
		// object reflects the new owning team for later iterations.
		task.TeamID = actingTeamID
	}
	r.ws.Broadcast(websocket.Event{
		Type:  "task_claimed",
		OrgID: orgID,
		Data: map[string]any{
			"task_id":             task.ID,
			"claimed_by_agent_id": a.ID,
			"claimed_by_user_id":  "",
		},
	})
}

// fireDelegate transitions the task to delegated status, broadcasts the
// change, then fires the spawner. Returns the run ID on success — used by
// DrainEntity to record which run a queued firing materialized into.
func (r *Router) fireDelegate(orgID string, task *domain.Task, trigger domain.EventHandler) (string, error) {
	if r.spawner == nil {
		return "", fmt.Errorf("spawner not configured")
	}

	// SKY-261 B+: no status flip here. Pre-SKY-261 we transitioned to
	// status='delegated' for UI feedback + dedup. Post-B+ the
	// responsibility axis is the claim columns: stampAgentClaim
	// (called by the caller on fireDelegate success) writes
	// claimed_by_agent_id and broadcasts task_claimed, which is what
	// the Board now listens for. Status stays 'queued' until a
	// genuine lifecycle move (done / dismissed / snoozed). Dedup is
	// unaffected — the partial unique index gates on status NOT IN
	// ('done', 'dismissed'), so a queued+claimed task still matches.
	log.Printf("[router] auto-delegating task %s (trigger %s, prompt %s)",
		task.ID, trigger.ID, trigger.PromptID)

	// Re-read task to get entity-joined display fields the spawner needs.
	fresh, err := r.tasks.GetSystem(context.Background(), orgID, task.ID)
	if err != nil || fresh == nil {
		if err != nil {
			return "", fmt.Errorf("re-read task: %w", err)
		}
		return "", fmt.Errorf("task %s disappeared before spawn", task.ID)
	}

	runID, err := r.spawner.Delegate(*fresh, delegate.DelegateOpts{
		OrgID:            orgID,
		ExplicitPromptID: trigger.PromptID,
		TriggerType:      "event",
		TriggerID:        trigger.ID,
	})
	if err != nil {
		// Post-B+: nothing to revert status-wise (status stayed 'queued').
		// stampAgentClaim hasn't run yet either — the caller only calls
		// it after fireDelegate returns nil. So this failure leaves the
		// task in a clean unclaimed-queued state, which is correct.
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
func (r *Router) DrainEntity(orgID, entityID string) {
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
		firing, err := r.firings.PopForEntity(context.Background(), orgID, entityID)
		if err != nil {
			log.Printf("[router] drain pop error for entity %s: %v", entityID, err)
			return
		}
		if firing == nil {
			return // queue empty
		}

		runID, skipReason, transientErr := r.attemptDrainOne(orgID, firing)
		if transientErr != nil {
			// Transient failure (DB read, Delegate). Leave the firing in
			// 'pending' state and bail the drain loop — marking
			// 'skipped_stale' here would permanently drop a queued intent
			// over a temporary problem. The periodic sweeper or the next
			// run-terminal will retry.
			log.Printf("[router] drain transient error on firing %d (entity %s): %v — leaving pending for retry",
				firing.ID, entityID, transientErr)
			return
		}
		if runID != "" {
			if err := r.firings.MarkFired(context.Background(), orgID, firing.ID, runID); err != nil {
				// Durability race: the run was created (side-effect
				// committed inside the spawner goroutine) but the UPDATE
				// that records the firing→run association failed. The
				// firing row is still 'pending' — a later DrainEntity
				// would pop the same row and fire again, duplicating.
				//
				// Roll the side-effect chain back in reverse: Cancel the
				// run we just spawned, then revert the task to 'queued'
				// so the limbo state (task=delegated + no live run) is
				// not externally visible. Mirrors what fireDelegate
				// already does when spawner.Delegate itself fails. The
				// firing stays 'pending' and the next drain re-fires
				// fresh; until then the task reads as queued, which is
				// honest.
				log.Printf("[router] mark firing %d fired (run %s) failed: %v — rolling back: cancelling run + reverting task to queued",
					firing.ID, runID, err)
				if r.spawner != nil {
					if cerr := r.spawner.Cancel(orgID, runID, ""); cerr != nil {
						log.Printf("[router] cancel run %s after mark-fired failure: %v — run may already be terminal, drain still triggers from its defer",
							runID, cerr)
					}
				}
				r.revertTaskStatus(orgID, firing.TaskID, "queued")
			}
			return // one fire per drain — the new run gates the rest
		}
		// Skipped or fire failed; record reason and continue draining.
		if err := r.firings.MarkSkipped(context.Background(), orgID, firing.ID, skipReason); err != nil {
			log.Printf("[router] mark firing %d skipped (%s): %v", firing.ID, skipReason, err)
			return
		}
		log.Printf("[router] skipped firing %d on entity %s: %s", firing.ID, entityID, skipReason)
	}
}

// RunDrainSweeper periodically attempts to drain every entity that has at
// least one pending firing. The sweeper is the safety net for stuck
// queues: a firing left in 'pending' after a transient validation/fire
// error needs *some* drain to retry it, and the natural trigger
// (notifyDrainer from an auto-run terminal) only fires when an auto run
// is actively terminating. If nothing's terminating — entity has no
// active runs and no events arrive — the queue would otherwise sit
// indefinitely.
//
// Cadence is 30s by default; tuneable via interval. Each tick lists
// entities with pending firings (cheap — partial index) and calls
// DrainEntity on each. DrainEntity's per-entity mutex makes the sweeper
// safe to run alongside event-triggered drains: if a drain is already
// running for an entity, the sweeper's call blocks then re-pops, which
// is fine. Empty queues are no-ops.
//
// Returns when ctx is cancelled. Caller is responsible for the lifetime
// (typically a goroutine started from main, cancelled at shutdown).
func (r *Router) RunDrainSweeper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Per-org iteration mirrors the established poller /
			// classifier pattern. In local mode this collapses to N=1
			// over the sentinel org (the only row in orgs); in multi
			// mode it fans across every active tenant. OrgsStore is
			// a required NewRouter parameter — if it were nil the
			// dereference below would panic, which is the right
			// behavior for a wiring bug at startup.
			orgIDs, err := r.orgs.ListActiveSystem(ctx)
			if err != nil {
				log.Printf("[router] drain sweeper: list orgs error: %v", err)
				continue
			}
			for _, orgID := range orgIDs {
				r.sweepOrg(ctx, orgID)
			}
		}
	}
}

// sweepOrg drains every entity in a single org that has at least one
// pending firing. Factored out of RunDrainSweeper so the per-org loop
// reads as one statement and per-org errors don't bail the whole
// cycle.
func (r *Router) sweepOrg(ctx context.Context, orgID string) {
	ids, err := r.firings.ListEntitiesWithPending(ctx, orgID)
	if err != nil {
		log.Printf("[router] drain sweeper: list error for org %s: %v", orgID, err)
		return
	}
	for _, eid := range ids {
		active, err := r.agentRuns.HasActiveAutoRunForEntitySystem(ctx, orgID, eid)
		if err != nil {
			log.Printf("[router] drain sweeper: active-check error for %s (org %s): %v", eid, orgID, err)
			continue
		}
		if active {
			continue
		}
		r.DrainEntity(orgID, eid)
	}
}

// attemptDrainOne validates a popped firing against current state and
// fires it if everything still holds. Three outcomes:
//
//   - (runID, "", nil)         — fire succeeded; caller marks 'fired'.
//   - ("", skipReason, nil)    — definitive "no longer relevant"; caller
//     marks 'skipped_stale'. Reserved for: task_closed (done /
//     dismissed / snoozed — task isn't drain-eligible on the
//     lifecycle axis), trigger_disabled, breaker_tripped,
//     claim_changed (SKY-261 B+: a user took the task over or
//     requeued it after the firing was enqueued, so the bot's
//     original commitment is no longer current; drainer must not
//     fire a phantom bot run against a now-user-claimed task).
//   - ("", "", err)            — transient failure (DB read, fire-time);
//     caller leaves the firing in 'pending' state and bails the drain
//     loop. The periodic sweeper or next run-terminal will retry.
//
// Validation reads from live tables, not from the firing row, so the
// drainer reflects the world *now* — invalidation falls out for free
// from the close cascade and trigger config.
//
// We classify Delegate errors as transient too: even when spawner.Delegate
// refuses (rate-limited GitHub, missing creds, worktree race), the firing
// intent is still valid and worth retrying. The breaker handles the
// "actually broken, repeated failure" case via run-level failure counts —
// but only once we've started enough runs to trip it. Until then, retry.
func (r *Router) attemptDrainOne(orgID string, firing *domain.PendingFiring) (runID, skipReason string, transientErr error) {
	task, err := r.tasks.GetSystem(context.Background(), orgID, firing.TaskID)
	if err != nil {
		return "", "", fmt.Errorf("task lookup: %w", err)
	}
	// SKY-261 B+: status='snoozed' belongs on the lifecycle-skip axis,
	// not the claim axis. A bot-claimed task that gets snoozed (e.g.,
	// the user said "wait until Tuesday") shouldn't fire a queued
	// drain when the entity slot opens — the snooze itself is a "do
	// not act" signal on the lifecycle axis. Grouped with
	// done/dismissed under task_closed because all three mean "the
	// task is not currently drain-eligible." A snooze wake-on-bump
	// will create a NEW event → new firing if the trigger still
	// matches; the deferred firing is the wrong path to wake it.
	if task == nil || task.Status == "done" || task.Status == "dismissed" || task.Status == "snoozed" {
		return "", domain.PendingFiringSkipTaskClosed, nil
	}

	// SKY-261 B+: drain only fires if the bot's claim still holds.
	// User-claim (claimed_by_user_id set) or requeue (both cleared)
	// invalidates the original commitment. Without this check, a
	// pending firing would fire even after the user explicitly took
	// the task over, producing a phantom bot run on a now-user-
	// claimed task.
	if task.ClaimedByAgentID == "" {
		return "", domain.PendingFiringSkipClaimChanged, nil
	}

	trigger, err := r.handlers.GetSystem(context.Background(), orgID, firing.TriggerID)
	if err != nil {
		return "", "", fmt.Errorf("trigger lookup: %w", err)
	}
	if trigger == nil || trigger.Kind != domain.EventHandlerKindTrigger || !trigger.Enabled {
		return "", domain.PendingFiringSkipTriggerDisabled, nil
	}

	breakerThreshold := derefIntDefault(trigger.BreakerThreshold, 0)
	failures, err := r.tasks.CountConsecutiveFailedRunsSystem(context.Background(), orgID, firing.EntityID, trigger.PromptID)
	if err != nil {
		return "", "", fmt.Errorf("breaker query: %w", err)
	}
	if failures >= breakerThreshold {
		return "", domain.PendingFiringSkipBreakerTripped, nil
	}

	id, err := r.fireDelegate(orgID, task, *trigger)
	if err != nil {
		return "", "", fmt.Errorf("fire delegate: %w", err)
	}
	return id, "", nil
}

// revertTaskStatus moves a task's lifecycle axis back to the given
// status and broadcasts the change so the frontend doesn't get stuck
// showing a stale state. Claim cols are intentionally left alone —
// after SKY-261 B+ the three axes (lifecycle / claim / runs) are
// orthogonal, and this helper only touches lifecycle.
//
// The only caller today is the mark-fired-failure rollback path in
// DrainEntity: a run was successfully spawned but the UPDATE that
// records the firing→run association failed. The recovery flow
// cancels the run, leaves the firing in 'pending' so the next drain
// retries it, and reverts the task lifecycle for FE consistency.
// Critically, the firing's *commitment* (the bot has taken this task)
// is unchanged — the next drain pass needs the bot claim to remain
// set or attemptDrainOne's ClaimedByAgentID guard would skip the
// retry as claim_changed, silently dropping the queued intent. So
// the claim col stays with the bot here; only status moves.
//
// Code paths that DO want to release the claim (user requeue, task
// completion, swipe-undo) clear the claim cols on their own — they
// don't go through this helper.
func (r *Router) revertTaskStatus(orgID, taskID, status string) {
	if err := r.tasks.SetStatusSystem(context.Background(), orgID, taskID, status); err != nil {
		log.Printf("[router] failed to revert task %s to %s: %v", taskID, status, err)
		return
	}
	r.ws.Broadcast(websocket.Event{
		Type:  "task_updated",
		OrgID: orgID,
		Data:  map[string]any{"task_id": taskID, "status": status},
	})
}

// --- Post-scoring re-derive (SKY-181) ------------------------------------

// ReDeriveAfterScoring re-checks deferred triggers for tasks that just
// received AI scores. Triggers with MinAutonomySuitability > 0 are skipped
// during HandleEvent and deferred to this callback, which fires from the
// scorer's OnScoringCompleted hook. orgID is the scoring context — the
// scorer batches per-org so every task in the slice belongs to the same
// tenant. Per-team auto_delegate_enabled is checked inside reDeriveTask
// after the task's team_id is resolved.
func (r *Router) ReDeriveAfterScoring(orgID string, taskIDs []string) {
	for _, taskID := range taskIDs {
		r.reDeriveTask(orgID, taskID)
	}
}

func (r *Router) reDeriveTask(orgID, taskID string) {
	task, err := r.tasks.GetSystem(context.Background(), orgID, taskID)
	if err != nil || task == nil {
		return
	}

	// Only re-derive queued tasks. Post-SKY-261 B+ the lifecycle axis
	// collapsed to {queued, snoozed, done, dismissed} so this gate
	// also has to cover snoozed (a snoozed task is on a "wait" until
	// its wake-on-bump event lands; a deferred-threshold re-derive
	// should not bypass that signal). done/dismissed naturally fall
	// out of the != "queued" check.
	if task.Status != "queued" {
		return
	}

	// The per-team auto_delegate kill switch is checked per firing team
	// inside the trigger loop below — one task is now visible to many
	// teams, so a single owner-team gate here would wrongly suppress (or
	// admit) other teams' deferred triggers.

	// Re-derive must not promote a task that's already claimed. After
	// SKY-261 B+ the responsibility axis lives on the claim cols, not
	// status — so a queued task may still be "already taken" by either
	// the bot (auto-delegate already fired and enqueued a firing, or
	// drag-to-bot stamped) or a user ("I'll take this myself" claim).
	// Without this guard, re-derive could:
	//   - stamp an agent claim onto a user-claimed task (XOR violation
	//     blocked at the DB level, but the visible state would be a
	//     spurious 500 log + WS toast)
	//   - fire a second bot run on a task the bot is already
	//     committed to (duplicate firing, breaker noise)
	// Either claim col set = "not the re-derive's business; the
	// commitment is real and the lifecycle event that ends the
	// commitment will arrive via its own path."
	if task.ClaimedByAgentID != "" || task.ClaimedByUserID != "" {
		return
	}

	// No score landed — nothing to gate against.
	if task.AutonomySuitability == nil {
		return
	}

	// Fetch handlers for this event type — same call HandleEvent uses,
	// kind-discriminated here.
	handlers, err := r.handlers.GetEnabledForEventSystem(context.Background(), orgID, task.EventType)
	if err != nil {
		log.Printf("[router] re-derive: failed to query event_handlers for %s: %v", task.EventType, err)
		return
	}

	// Fetch the primary event's metadata for predicate matching.
	metadata, err := r.events.GetMetadataSystem(context.Background(), orgID, task.PrimaryEventID)
	if err != nil {
		log.Printf("[router] re-derive: failed to fetch event metadata for %s: %v", task.PrimaryEventID, err)
		return
	}

	// The task's recorded visibility set — the teams whose handlers
	// matched the original event. Re-derive re-queries every enabled
	// trigger for the event type, so without this gate a trigger whose
	// team was never part of the situation (enabled after the fact, or
	// otherwise absent from the match) could match the stored metadata,
	// fire, and consolidate ownership onto a task that team can't see.
	// The owner team_id always grants visibility, so it qualifies too.
	visibleTeams, err := r.tasks.VisibilityTeamsSystem(context.Background(), orgID, taskID)
	if err != nil {
		log.Printf("[router] re-derive: failed to fetch visibility teams for task %s: %v", taskID, err)
		return
	}
	visibleSet := map[string]struct{}{task.TeamID: {}}
	for _, vt := range visibleTeams {
		visibleSet[vt] = struct{}{}
	}

	for _, trigger := range handlers {
		if trigger.Kind != domain.EventHandlerKindTrigger {
			continue
		}
		// Each matched team's deferred trigger fires against the single
		// task; the claim CAS resolves contention. Gate first on the
		// task's recorded visibility set so a team that wasn't part of
		// the original match can't ride (and claim) a task it can't see,
		// then on the firing team's own auto_delegate kill switch. The
		// org-visible sentinel normalizes to the resolved owner team so
		// it matches the (resolved) visibility-set entries.
		firingTeam := effectiveActingTeam(handlerTeamID(trigger), task.TeamID)
		if _, visible := visibleSet[firingTeam]; !visible {
			continue
		}
		if !r.autoDelegateEnabledForTeam(context.Background(), firingTeam) {
			continue
		}
		minAutonomy := derefFloatDefault(trigger.MinAutonomySuitability, 0)
		// Only process deferred triggers — immediate ones already fired in HandleEvent.
		if minAutonomy <= 0 {
			continue
		}

		// Autonomy gate.
		if *task.AutonomySuitability < minAutonomy {
			log.Printf("[router] re-derive: task %s suitability %.2f < trigger %s threshold %.2f, skipping",
				taskID, *task.AutonomySuitability, trigger.ID, minAutonomy)
			continue
		}

		// Predicate match — same logic as HandleEvent.
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
			taskID, *task.AutonomySuitability, trigger.ID, minAutonomy)
		// Triggering event for the queued firing is the task's primary
		// event — that's the one whose match scored autonomously above
		// threshold. Real-event provenance keeps the audit trail honest
		// when a re-derived firing ends up enqueued.
		r.tryAutoDelegate(orgID, task, trigger, task.EntityID, task.PrimaryEventID, firingTeam)
	}
}

// derefIntDefault unwraps a *int with a default if nil. Used on
// trigger-only fields that are nullable on the column level but
// guaranteed non-nil by the per-kind CHECK constraint for kind='trigger'
// rows; the default is the defensive value for the impossible nil case.
func derefIntDefault(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func derefFloatDefault(p *float64, def float64) float64 {
	if p == nil {
		return def
	}
	return *p
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
func (r *Router) cancelActiveRunsForTask(orgID, taskID string) {
	if r.spawner == nil {
		return
	}
	ids, err := r.agentRuns.ActiveIDsForTaskSystem(context.Background(), orgID, taskID)
	if err != nil {
		log.Printf("[router] active-run lookup for task %s failed: %v", taskID, err)
		return
	}
	for _, id := range ids {
		if err := r.spawner.Cancel(orgID, id, ""); err != nil {
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
func (r *Router) closeEntity(orgID, entityID string) (int, error) {
	if tasks, err := r.tasks.FindActiveByEntitySystem(context.Background(), orgID, entityID); err != nil {
		// Non-fatal: better to cascade-close the entity than to abort
		// because we couldn't enumerate tasks for cancellation. Any
		// orphaned runs can be cleaned up by the existing startup
		// worktree.Cleanup pass.
		log.Printf("[router] entity close: list active tasks for %s failed: %v", entityID, err)
	} else {
		for _, t := range tasks {
			r.cancelActiveRunsForTask(orgID, t.ID)
		}
	}

	if err := r.entities.CloseSystem(context.Background(), orgID, entityID); err != nil {
		return 0, err
	}
	closed, err := r.tasks.CloseAllForEntitySystem(context.Background(), orgID, entityID, "entity_closed")
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
func (r *Router) closeTaskWithAudit(orgID, taskID, closingEventID, closeReason, closeEventType string) error {
	r.cancelActiveRunsForTask(orgID, taskID)
	if err := r.tasks.CloseSystem(context.Background(), orgID, taskID, closeReason, closeEventType); err != nil {
		return err
	}
	if closingEventID != "" {
		_ = r.tasks.RecordEventSystem(context.Background(), orgID, taskID, closingEventID, "closed")
	}
	return nil
}

func (r *Router) runInlineCloseChecks(orgID string, evt domain.Event, entityID string) bool {
	switch evt.EventType {
	case domain.EventGitHubPRCICheckPassed:
		return r.closeCheckCIPassed(orgID, evt, entityID)
	case domain.EventGitHubPRReviewApproved,
		domain.EventGitHubPRReviewCommented,
		domain.EventGitHubPRReviewDismissed:
		// A submitted review does two things: (a) satisfies the reviewer's
		// own review_requested obligation, and (b) may resolve the author's
		// outstanding changes_requested. Run both — they touch different
		// tasks (the reviewer's request vs the author's changes).
		reviewed := r.closeReviewerRequestOnReview(orgID, evt, entityID)
		resolved := r.closeCheckReviewResolved(orgID, evt, entityID)
		return reviewed || resolved
	case domain.EventGitHubPRReviewChangesRequested:
		// Requesting changes is still a review — the reviewer fulfilled their
		// review_requested obligation. (The changes themselves spawn the
		// author-side task via the normal rule path, not here.)
		return r.closeReviewerRequestOnReview(orgID, evt, entityID)
	case domain.EventGitHubPRReviewRequestRemoved:
		return r.closeCheckReviewRequestRemoved(orgID, evt, entityID)
	case domain.EventJiraIssueAssigned:
		return r.closeCheckJiraReassigned(orgID, evt, entityID)
	}
	return false
}

// closeCheckCIPassed: if no failing check-runs remain on this entity at the
// latest SHA, close active ci_check_failed tasks.
func (r *Router) closeCheckCIPassed(orgID string, evt domain.Event, entityID string) bool {
	// Parse metadata to get head_sha.
	var meta events.GitHubPRCICheckPassedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return false
	}

	// Query: any active ci_check_failed tasks still open on this entity?
	failedTasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, domain.EventGitHubPRCICheckFailed)
	if err != nil || len(failedTasks) == 0 {
		return false
	}

	// Check entity snapshot for remaining failures at the current SHA.
	entity, err := r.entities.GetSystem(context.Background(), orgID, entityID)
	if err != nil || entity == nil {
		return false
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		return false
	}

	// If any check is still failing at the latest SHA, don't close.
	for _, cr := range snap.CheckRuns {
		if domain.IsFailingConclusion(cr.Conclusion) {
			return false
		}
	}

	// All green — close the failure tasks.
	closed := false
	for _, t := range failedTasks {
		if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", domain.EventGitHubPRCICheckPassed); err != nil {
			log.Printf("[router] failed to close ci_check_failed task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (ci_check_failed → ci_check_passed)", t.ID)
			closed = true
		}
	}
	return closed
}

// closeCheckReviewResolved: if the reviewer's prior state was
// changes_requested and no other reviewer still has outstanding
// changes_requested, close active review_changes_requested tasks.
func (r *Router) closeCheckReviewResolved(orgID string, evt domain.Event, entityID string) bool {
	// We need to know which reviewer just changed state. Parse metadata.
	var reviewer string
	switch evt.EventType {
	case domain.EventGitHubPRReviewApproved:
		var meta events.GitHubPRReviewApprovedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			return false
		}
		reviewer = meta.Reviewer
	case domain.EventGitHubPRReviewCommented:
		var meta events.GitHubPRReviewCommentedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			return false
		}
		reviewer = meta.Reviewer
	case domain.EventGitHubPRReviewDismissed:
		var meta events.GitHubPRReviewDismissedMetadata
		if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
			return false
		}
		reviewer = meta.Reviewer
	}
	if reviewer == "" {
		return false
	}

	// Check entity snapshot: does this reviewer's prior state include
	// changes_requested, and is no other reviewer still requesting changes?
	entity, err := r.entities.GetSystem(context.Background(), orgID, entityID)
	if err != nil || entity == nil {
		return false
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		return false
	}

	anyOutstandingChanges := false
	for _, rs := range snap.Reviews {
		if rs.State == "CHANGES_REQUESTED" && rs.Author != reviewer {
			anyOutstandingChanges = true
			break
		}
	}
	if anyOutstandingChanges {
		return false
	}

	// Close review_changes_requested tasks on this entity.
	tasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, domain.EventGitHubPRReviewChangesRequested)
	if err != nil {
		return false
	}
	closed := false
	for _, t := range tasks {
		if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", evt.EventType); err != nil {
			log.Printf("[router] failed to close changes_requested task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (review resolved by %s)", t.ID, reviewer)
			closed = true
		}
	}
	return closed
}

// closeReviewerRequestOnReview: a reviewer submitted a review (any type:
// approved / commented / changes_requested / dismissed), so their
// review_requested obligation is satisfied — close that reviewer's per-reviewer
// task (keyed "user:<reviewer>"), and only that one. A review by reviewer A
// must not close reviewer B's task on the same PR.
//
// Driven off the typed review events (which fire for EVERY reviewer in both
// local and multi mode and carry Reviewer), not a self-only "submitted" event:
// the close is per-reviewer by identity, mode-agnostic, and independent of
// whether GitHub happens to drop the reviewer from the requested list (a
// comment-only review may not, so review_request_removed alone wouldn't cover
// it). A team review_requested task (keyed "team:<org>/<slug>") is NOT closed
// here — an individual's review doesn't satisfy a team request; that closes via
// the membership-dismissal review_request_removed (closeCheckReviewRequestRemoved).
//
// The reviewer is always an individual (github teams don't submit reviews), so
// the key is always the user namespace.
func (r *Router) closeReviewerRequestOnReview(orgID string, evt domain.Event, entityID string) bool {
	// All four typed review metadata structs carry a top-level "reviewer".
	var m struct {
		Reviewer string `json:"reviewer"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &m); err != nil || m.Reviewer == "" {
		return false
	}
	dedupKey := events.ReviewerDedupKeyUser(m.Reviewer)

	tasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, domain.EventGitHubPRReviewRequested)
	if err != nil {
		return false
	}
	closed := false
	for _, t := range tasks {
		if t.DedupKey != dedupKey {
			continue // a different reviewer's task — leave it open
		}
		if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", evt.EventType); err != nil {
			log.Printf("[router] failed to close review_requested task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (reviewed by %s via %s)", t.ID, m.Reviewer, evt.EventType)
			closed = true
		}
	}
	return closed
}

// closeCheckReviewRequestRemoved: a requested reviewer was dropped from the
// PR's requested-reviewers list (reviewed or request rescinded). Close the
// review_requested task keyed to that reviewer.
//
// review_requested tasks are now per-reviewer (dedup_key =
// "user:<login>" / "team:<org>/<slug>"), so a removal closes only the one
// task whose dedup_key matches the removal event's — dropping reviewer A must
// not close B's task. A legacy removal carrying no dedup_key closes every
// review_requested task on the entity (the pre-ticket behavior).
func (r *Router) closeCheckReviewRequestRemoved(orgID string, evt domain.Event, entityID string) bool {
	tasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, domain.EventGitHubPRReviewRequested)
	if err != nil {
		return false
	}
	closed := false
	for _, t := range tasks {
		if evt.DedupKey != "" && t.DedupKey != evt.DedupKey {
			continue // different reviewer's task — leave it open
		}
		if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", domain.EventGitHubPRReviewRequestRemoved); err != nil {
			log.Printf("[router] failed to close review_requested task %s: %v", t.ID, err)
		} else {
			log.Printf("[router] inline-closed task %s (review request removed: %s)", t.ID, evt.DedupKey)
			closed = true
		}
	}
	return closed
}

// closeCheckJiraReassigned: when a Jira issue is assigned to someone who is
// NOT self, close any active jira:issue:assigned or jira:issue:available
// tasks on this entity. "Self" here is the local user's jira_account_id;
// in multi mode the inline close still over-closes acceptably (the next
// poll reopens via discovery for any other user the assignment landed on).
func (r *Router) closeCheckJiraReassigned(orgID string, evt domain.Event, entityID string) bool {
	var meta events.JiraIssueAssignedMetadata
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return false
	}
	if r.users != nil {
		// TODO(SKY-XXX-future): in multi-mode there is no single "the
		// user" whose Jira identity counts for this inline close
		// check — the router serves all org members. The current
		// sentinel-keyed read keeps local-mode behavior intact and
		// over-closes in multi-mode (acceptable: a still-assigned
		// member's next poll reopens via discovery). Resolving this
		// needs a product decision about team-membership-driven
		// semantics, tracked separately from D9-core.
		localAccountID, localDisplayName, err := r.users.GetJiraIdentitySystem(context.Background(), runmode.LocalDefaultUserID)
		if err == nil {
			if meta.AssigneeAccountID != "" && localAccountID != "" && strings.EqualFold(meta.AssigneeAccountID, localAccountID) {
				return false // assigned to me — not a reassignment-away
			}
			// Legacy snapshots (pre-fix) or Server/DC with no accountId: fall
			// back to a case-insensitive display-name comparison so we don't
			// auto-close tasks that are still assigned to the local user.
			if meta.AssigneeAccountID == "" && meta.Assignee != "" && localDisplayName != "" && strings.EqualFold(meta.Assignee, localDisplayName) {
				return false // display-name fallback — not a reassignment-away
			}
		}
	}

	// Close active assigned tasks.
	closed := false
	for _, eventType := range []string{domain.EventJiraIssueAssigned, domain.EventJiraIssueAvailable} {
		tasks, err := r.tasks.FindActiveByEntityAndTypeSystem(context.Background(), orgID, entityID, eventType)
		if err != nil {
			continue
		}
		for _, t := range tasks {
			if err := r.closeTaskWithAudit(orgID, t.ID, evt.ID, "auto_closed_by_event", domain.EventJiraIssueAssigned); err != nil {
				log.Printf("[router] failed to close %s task %s: %v", eventType, t.ID, err)
			} else {
				log.Printf("[router] inline-closed task %s (jira reassigned away)", t.ID)
				closed = true
			}
		}
	}
	return closed
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
