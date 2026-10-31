package routing

import (
	"context"
	"fmt"
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
// spawner — kicking off a run, tearing one down, and injecting into a live
// one. Narrowed from *delegate.Spawner so tests can stub the spawn surface
// without bringing up a worktree, the agent subprocess, etc. Production
// wiring passes a *delegate.Spawner.
type Delegator interface {
	Delegate(task domain.Task, opts delegate.DelegateOpts) (string, error)
	// StopConversationAndCancelBlueprint is the teardown half: the router only
	// stops a conversation when the layer above it has already ended — a task
	// closed, a firing rolled back — so the owning blueprint dies with it
	// rather than freezing 'running' over work nothing will resume. The cause
	// names which of those it was, and reaches the stopped conversation's
	// transcript.
	StopConversationAndCancelBlueprint(orgID, conversationID, userID string, cause delegate.StopCause) error
	// StopBlueprintRun is the same teardown addressed from the other end, for
	// the caller that holds the id Delegate returned and has no conversation
	// in hand: it stops whatever steps the run has and cancels the run itself.
	// Which verb a caller wants follows from which id it is holding — neither
	// id resolves against the other's table.
	StopBlueprintRun(orgID, blueprintRunID string, cause delegate.StopCause) error
	// TeardownTaskDeliverablesSystem retires the unresolved deliverables a closed
	// task's conversations hold — the draft PRs closed, the staged reviews
	// dismissed. It rides only the close, because a task's deliverables survive
	// every other boundary: a firing rolled back or a conversation stopped
	// leaves them for whatever picks the task up next. Nobody authorized an
	// event-driven close, so the audit rows it writes name no actor.
	TeardownTaskDeliverablesSystem(ctx context.Context, orgID, taskID string)
	// StageOrDeliverAdditiveEvent routes one agent-facing additive-event
	// injection for a run by its live state — local process, live remote
	// executor (TFAC-585's `inject` conversation_signals kind), or the durable
	// staged-injection fallback — returning a 4-way outcome. Signature
	// matches *delegate.Spawner's method exactly. Used by tryAutoDelegate's
	// additive-event branch to fold a follow-up event into the task's live
	// conversation instead of deferring a second one:
	// InjectNotDelivered means the firing must fall through to the normal
	// deferral (no durable row to fall back on); InjectDeliveredRemote
	// means a live remote executor now owns recording the outcome (the
	// caller must NOT record task_events 'injected' itself); the other two
	// outcomes are handled exactly like the pre-TFAC-585 delivered/staged
	// cases.
	StageOrDeliverAdditiveEvent(ctx context.Context, orgID, conversationID, producer, body string, prov domain.NoteProvenance, firing delegate.AdditiveFiringRef) delegate.InjectOutcome
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
//  7. Auto-delegates on matching triggers — fires if the task is idle,
//     folds the event into the task's live conversation if it has one, and
//     admits a firing onto pending_firings when the task is busy, where the
//     firing worker (RunFiringQueue) claims it once the task's gate — no live
//     conversation and no run still marked running, the claim query's own
//     filter — opens.
//  8. Runs inline close checks for the event type
type Router struct {
	prompts       dbpkg.PromptStore
	blueprints    dbpkg.BlueprintStore // resolves a trigger's blueprint → first step prompt for the per-(entity, prompt) breaker
	handlers      dbpkg.EventHandlerStore
	agents        dbpkg.AgentStore
	teamAgents    dbpkg.TeamAgentStore        // read team_agents.enabled before auto-firing triggers
	users         dbpkg.UsersStore            // reverse identity lookups (GitHub login, Jira account, Linear user → TF users) for owner routing
	tasks         dbpkg.TaskStore             // task lifecycle, dedup, claims, breaker
	conversations dbpkg.ConversationStore     // lookup active runs for the task-close cancel cascade
	entities      dbpkg.EntityStore           // closed-entity guard + entity-terminating close cascade
	firings       dbpkg.PendingFiringsStore   // per-task firing queue the firing worker claims from; its unsettled read is half the gate
	events        dbpkg.EventStore            // admin-pool RecordSystem + GetMetadataSystem for the background subscriber
	eventQueue    dbpkg.EventQueueStore       // durable router queue the drain worker claims from; set post-construction via SetEventQueue (nil → worker is a no-op)
	orgs          dbpkg.OrgsStore             // per-org iteration for the terminal-state checker and the org-settings reads of team routing; required (the checker dereferences it directly)
	teams         dbpkg.TeamsStore            // per-team auto_delegate_enabled kill-switch read post-internal/config deletion
	teamRepos     dbpkg.TeamGitHubReposStore  // team↔repo tracking gate; nil-safe — gate is skipped (no filtering) when unset
	jiraRules     dbpkg.JiraStatusRulesStore  // team↔project tracking gate; nil-safe — Jira gate skipped when unset
	linearRules   dbpkg.LinearTeamRulesStore  // team↔Linear-team tracking gate; nil-safe — Linear gate skipped when unset
	githubGroups  dbpkg.TeamGitHubGroupsStore // github-team→TF-team mapping; resolves review_requested / mentioned team visibility. nil-safe — that routing degrades to handler-team visibility when unset
	rederive      dbpkg.TaskReDeriveStore     // score re-evaluation queue the re-derive worker claims from; set post-construction via SetTaskReDerive (nil → worker is a no-op)
	spawner       Delegator
	scorer        Scorer
	ws            *websocket.Hub
	publisher     EventPublisher // nil-safe; set post-construction via SetEventPublisher — mirrors the per-event routing disposition sentinel onto the bus (TFAC-593)
	// externalActions is the ledger the close phase reads to tell whether a
	// task's own run caused the event closing it; set post-construction via
	// SetExternalActions. nil → no close is ever deferred.
	externalActions dbpkg.ExternalActionStore
	// sourceGate answers whether an org has paused an event source. nil-safe;
	// set post-construction via SetEventSourceGate — a router without one
	// never drops. See source_gate.go.
	sourceGate *sourceGate

	// executorID/bootEpoch are this process's persistent instance-registry
	// identity, set post-construction via SetExecutorID before RunEventQueue
	// starts. Stamped as the lease owner onto every event_queue row this
	// router claims (mirroring delegate.Spawner's executorID/bootEpoch for
	// runs): provenance for the reclaim log line, never the write fence,
	// which is the lease generation. A claim refuses an empty owner, so a
	// test that drains sets an identity too.
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

	// firingWake is the firing worker's doorbell: a conversation terminal
	// sends on it (WakeFirings) so the task's next firing is claimed at once
	// rather than on the next scan tick. Capacity one, non-blocking sends.
	firingWake chan struct{}

	// rederiveWake is the re-derive worker's doorbell: the scorer's
	// completion callback sends on it (WakeReDerive) so the rows its score
	// write admitted are claimed at once rather than on the next scan tick.
	// Capacity one, non-blocking sends.
	rederiveWake chan struct{}
}

// NewRouter creates a Router. teamAgents is nil-safe — callers wired
// before that behavior existed can pass nil; the bot-disabled-team check
// degrades to "always enabled" in that case (the prior
// behavior). users is nil-safe too — the identity-based owner routing
// resolves nobody when it is missing.
// orgs is required — the terminal-state checker iterates it per org and
// dereferences it directly, so a nil orgs is a startup wiring bug that
// panics there, not a degraded mode.
// teamRepos is nil-safe for task routing — with it missing, the team↔repo
// gate drops no rule or trigger, as though every team tracked every
// repository — and fails closed for notify rules, which then match no GitHub
// event. jiraRules and linearRules are nil-safe the same way for the
// team↔project and team↔Linear-team gates. githubGroups is
// nil-safe — review_requested / mentioned visibility routing degrades to
// handler-team visibility (the pre-ticket behavior) when missing or when an
// event carries no identity.
func NewRouter(prompts dbpkg.PromptStore, blueprints dbpkg.BlueprintStore, handlers dbpkg.EventHandlerStore, agents dbpkg.AgentStore, teamAgents dbpkg.TeamAgentStore, users dbpkg.UsersStore, tasks dbpkg.TaskStore, conversations dbpkg.ConversationStore, entities dbpkg.EntityStore, firings dbpkg.PendingFiringsStore, events dbpkg.EventStore, orgs dbpkg.OrgsStore, teams dbpkg.TeamsStore, teamRepos dbpkg.TeamGitHubReposStore, jiraRules dbpkg.JiraStatusRulesStore, linearRules dbpkg.LinearTeamRulesStore, githubGroups dbpkg.TeamGitHubGroupsStore, spawner Delegator, scorer Scorer, ws *websocket.Hub) *Router {
	return &Router{
		prompts:       prompts,
		blueprints:    blueprints,
		handlers:      handlers,
		agents:        agents,
		teamAgents:    teamAgents,
		users:         users,
		tasks:         tasks,
		conversations: conversations,
		entities:      entities,
		firings:       firings,
		events:        events,
		orgs:          orgs,
		teams:         teams,
		teamRepos:     teamRepos,
		jiraRules:     jiraRules,
		linearRules:   linearRules,
		githubGroups:  githubGroups,
		spawner:       spawner,
		scorer:        scorer,
		ws:            ws,
		firingWake:    make(chan struct{}, 1),
		rederiveWake:  make(chan struct{}, 1),
	}
}

// breakerPromptID resolves the prompt id the auto-delegate breaker keys on
// for a trigger. The trigger fires a blueprint; the breaker tracks failed
// runs per (entity, prompt), and runs are prompt-keyed, so it tracks the
// blueprint's first step prompt — exactly the wrapped prompt for the 1-step
// blueprints every shipped trigger uses. Returns "" when the blueprint or its
// steps can't be resolved (CountConsecutiveFailedConversationsSystem then matches no
// runs → breaker never trips, the safe default that doesn't block
// auto-delegation on a transient read error). nil-safe on the store.
func (r *Router) breakerPromptID(ctx context.Context, orgID, blueprintID string) string {
	if r.blueprints == nil || blueprintID == "" {
		return ""
	}
	steps, err := r.blueprints.ListStepsSystem(ctx, orgID, blueprintID)
	if err != nil || len(steps) == 0 {
		return ""
	}
	return steps[0].StepPromptID
}

// SetEventQueue wires the durable router queue post-
// construction, mirroring the spawner.SetFiringWaker pattern. The drain
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

// SetTaskReDerive wires the score re-evaluation queue post-construction,
// same injection as SetEventQueue. The re-derive worker (RunReDeriveQueue)
// claims from this store; the score store admits into it. Kept off
// NewRouter's signature for the same reason: only the worker needs it, and
// leaving it nil makes RunReDeriveQueue a no-op so the many existing test
// constructions don't have to thread it.
func (r *Router) SetTaskReDerive(s dbpkg.TaskReDeriveStore) {
	r.rederive = s
}

// SetExternalActions wires the ledger the close phase reads to defer the
// close of a task whose own run caused the closing event (close_deferral.go),
// post-construction like SetEventQueue so the existing test constructions
// need not thread it. Left nil, every task an event closes closes at once.
func (r *Router) SetExternalActions(s dbpkg.ExternalActionStore) {
	r.externalActions = s
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
// delegate.Spawner.executorIdentity. Read per claim by drainEventQueue.
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
// happy-path behavior. Nil teams store (test wiring) or empty team
// degrades to false — safer than the prior "fail open and silently
// auto-delegate" behavior the deleted internal/config had.
//
// A store failure returns (false, err) rather than a bare false. Both
// callers still decline to fire — an unreadable switch is never treated as
// permission — but they can tell "the user turned this off" from "we
// couldn't find out," which is the difference between an event that is
// finished and one that deserves another attempt.
func (r *Router) autoDelegateEnabledForTeam(ctx context.Context, teamID string) (bool, error) {
	if r.teams == nil || teamID == "" {
		return false, nil
	}
	settings, err := r.teams.GetSettingsSystem(ctx, teamID)
	if err != nil {
		routerLog.Warn("auto_delegate gate: team settings read failed, declining to fire",
			"team", teamID, "error", err)
		return false, fmt.Errorf("read team settings: %w", err)
	}
	return settings.AutoDelegateEnabled, nil
}

// broadcastTasksUpdated pushes the frontend's "re-read the board" nudge for an
// org. The payload is empty by design — the hub carries the fact that
// something changed, and the client re-fetches — so every producer of a task
// change (the typed close phase, the terminating close, the task upsert) sends
// the identical event rather than each spelling out the same literal.
func (r *Router) broadcastTasksUpdated(orgID string) {
	r.ws.Broadcast(websocket.Event{Type: "tasks_updated", OrgID: orgID, Data: map[string]any{}})
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
