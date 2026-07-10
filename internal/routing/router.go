package routing

import (
	"context"
	"sync"

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
// spawner — kicking off a run, cancelling one, and injecting into a live
// one. Narrowed from *delegate.Spawner so tests can stub the spawn surface
// without bringing up a worktree, the agent subprocess, etc. Production
// wiring passes a *delegate.Spawner.
type Delegator interface {
	Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error)
	Cancel(orgID, runID, userID string) error
	// StageOrDeliverAdditiveEvent routes one agent-facing additive-event
	// injection for a run by its live state — local process, live remote
	// executor (TFAC-585's `inject` run_signals kind), or the durable
	// staged-injection fallback — returning a 4-way outcome. Signature
	// matches *delegate.Spawner's method exactly. Used by tryAutoDelegate's
	// additive-event branch to fold a follow-up event into an entity's
	// already-active auto run instead of deferring a second one:
	// InjectNotDelivered means the firing must fall through to the normal
	// deferral (no durable row to fall back on); InjectDeliveredRemote
	// means a live remote executor now owns recording the outcome (the
	// caller must NOT record task_events 'injected' itself); the other two
	// outcomes are handled exactly like the pre-TFAC-585 delivered/staged
	// cases.
	StageOrDeliverAdditiveEvent(ctx context.Context, orgID, runID, producer, body string, firing delegate.AdditiveFiringRef) delegate.InjectOutcome
}

// EventPublisher is the bus-publish seam the router uses to mirror the
// per-event routing disposition sentinel (TFAC-593) onto the event bus, so
// an async event source (e.g. Slack) can learn synchronously-unavailable
// routing outcomes. Same minimal shape as internal/tracker's Publisher and
// delegate.Spawner's EventPublisher; the plain *eventbus.Bus satisfies it
// directly.
type EventPublisher interface {
	Publish(evt domain.Event)
}

// Router is the central eventbus subscriber that replaces the old auto-
// delegate hook. On every event it:
//  1. Records the event (durable audit log)
//  2. Handles entity lifecycle (merged/closed → close entity + all tasks)
//  3. Guards against closed entities (no task creation on dead PRs)
//  4. Matches event_handlers (rules + triggers, unified) via
//     typed predicates — one store query, kind-discriminated handling
//  5. Dedup-creates or bumps tasks
//  6. Enqueues AI scoring
//  7. Auto-delegates on matching triggers — fires if the entity is idle,
//     enqueues onto pending_firings if the entity already has an active
//     auto run or earlier queued firings.
//  8. Runs inline close checks for the event type
type Router struct {
	prompts      dbpkg.PromptStore
	blueprints   dbpkg.BlueprintStore // resolves a trigger's blueprint → first step prompt for the per-(entity, prompt) breaker
	handlers     dbpkg.EventHandlerStore
	agents       dbpkg.AgentStore
	teamAgents   dbpkg.TeamAgentStore        // read team_agents.enabled before auto-firing triggers
	users        dbpkg.UsersStore            // read local user's host-scoped Jira identity for inline close gates
	tasks        dbpkg.TaskStore             // task lifecycle, dedup, claims, breaker
	agentRuns    dbpkg.AgentRunStore         // lookup active runs for the task-close cancel cascade
	entities     dbpkg.EntityStore           // closed-entity guard + entity-terminating close cascade
	firings      dbpkg.PendingFiringsStore   // per-entity firing queue + active-run gate
	events       dbpkg.EventStore            // admin-pool RecordSystem + GetMetadataSystem for the background subscriber
	eventQueue   dbpkg.EventQueueStore       // durable router queue the drain worker claims from; set post-construction via SetEventQueue (nil → worker is a no-op)
	orgs         dbpkg.OrgsStore             // per-org iteration for the drain sweeper; required (RunDrainSweeper dereferences it directly)
	teams        dbpkg.TeamsStore            // per-team auto_delegate_enabled kill-switch read post-internal/config deletion
	teamRepos    dbpkg.TeamGitHubReposStore  // team↔repo tracking gate; nil-safe — gate is skipped (no filtering) when unset
	jiraRules    dbpkg.JiraStatusRulesStore  // team↔project tracking gate; nil-safe — Jira gate skipped when unset
	githubGroups dbpkg.TeamGitHubGroupsStore // github-team→TF-team mapping; resolves review_requested team visibility. nil-safe — review routing degrades to handler-team visibility when unset
	spawner      Delegator
	scorer       Scorer
	ws           *websocket.Hub
	publisher    EventPublisher // nil-safe; set post-construction via SetEventPublisher — mirrors the per-event routing disposition sentinel onto the bus (TFAC-593)

	// executorID/bootEpoch are this process's persistent instance-registry
	// identity (TFAC-577), set post-construction via SetExecutorID before
	// RunEventQueue starts. Stamped onto every event_queue row this router
	// claims (mirroring delegate.Spawner's executorID/bootEpoch for runs), so
	// the boot-time ResetProcessing sweep (TFAC-578) can self-scope to rows
	// this instance itself claimed, never a live sibling's. Zero values
	// (tests that never call SetExecutorID) degrade to the empty-string
	// identity — fine for single-router tests, never used in production.
	//
	// Guarded by executorMu, mirroring delegate.Spawner's executorID/
	// bootEpoch + s.mu: today SetExecutorID only ever runs once at startup
	// before RunEventQueue's goroutine starts, so the lock is a no-op in
	// practice, but it keeps the pattern symmetric with the spawner and
	// keeps a future second call site (e.g. a hot-reload path) from
	// becoming a real data race.
	executorMu sync.Mutex
	executorID string
	bootEpoch  int64

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
// before that behavior existed can pass nil; the bot-disabled-team check
// degrades to "always enabled" in that case (the prior
// behavior). users is nil-safe too — the inline-close gate
// degrades to "treat every reassignment as away-from-me" when missing,
// which over-closes (acceptable: user can reopen via the next poll).
// orgs is required — the drain sweeper (RunDrainSweeper) iterates it per
// org and dereferences it directly, so a nil orgs is a startup wiring bug
// that panics there, not a degraded mode.
// teamRepos is nil-safe — the team↔repo gate is skipped (no
// handler is dropped) when missing, matching prior behavior where
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

// SetEventPublisher wires the bus publisher the router mirrors the
// per-event routing disposition sentinel onto (TFAC-593), same
// post-construction injection pattern as SetEventQueue — the bus is built
// before the router in app composition, but the setter keeps
// internal/routing decoupled from internal/eventbus's concrete type. Safe
// to call once at startup; nil publisher (the default) disables the bus
// mirror entirely — routing behavior is unaffected either way.
func (r *Router) SetEventPublisher(p EventPublisher) {
	r.publisher = p
}

// SetExecutorID wires this router's persistent instance-registry identity
// (TFAC-577), mirroring delegate.Spawner.SetExecutorID. Call once at startup,
// before RunEventQueue starts; tests that never call this keep the
// zero-value ("", 0) identity.
func (r *Router) SetExecutorID(id string, bootEpoch int64) {
	r.executorMu.Lock()
	defer r.executorMu.Unlock()
	r.executorID = id
	r.bootEpoch = bootEpoch
}

// executorIdentity returns the current (executorID, bootEpoch) pair under
// lock — the read side of SetExecutorID, mirroring
// delegate.Spawner.executorIdentity. Used by RunEventQueue's boot reset and
// drainEventQueue's claim.
func (r *Router) executorIdentity() (string, int64) {
	r.executorMu.Lock()
	defer r.executorMu.Unlock()
	return r.executorID, r.bootEpoch
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
		routerLog.Warn("auto_delegate gate: team settings read failed, defaulting to disabled",
			"team", teamID, "error", err)
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

func matchPredicate(eventType, predJSON, metaJSON string) (bool, error) {
	schema, ok := events.Get(eventType)
	if !ok {
		// Unknown event type — can't match. Not an error per se; system events
		// don't have schemas and won't match any rules.
		return false, nil
	}
	return schema.Match(predJSON, metaJSON)
}
