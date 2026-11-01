package routing

import (
	"context"
	"fmt"
	"reflect"
	"strings"

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
	// TaskHasUnresolvedDeliverablesSystem reports whether any conversation on
	// the task holds a deliverable a person has not resolved yet (a draft PR,
	// a ready pending review). A read failure answers true, the direction
	// that tears nothing down.
	TaskHasUnresolvedDeliverablesSystem(ctx context.Context, orgID, taskID string) bool
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
// routing outcomes. Same minimal shape as internal/eventing/tracker's Publisher and
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
	// One field per RouterDeps field, which documents each. The workers'
	// doorbells below are the router's own.
	prompts         dbpkg.PromptStore
	blueprints      dbpkg.BlueprintStore
	handlers        dbpkg.EventHandlerStore
	agents          dbpkg.AgentStore
	teamAgents      dbpkg.TeamAgentStore
	users           dbpkg.UsersStore
	tasks           dbpkg.TaskStore
	conversations   dbpkg.ConversationStore
	entities        dbpkg.EntityStore
	firings         dbpkg.PendingFiringsStore
	events          dbpkg.EventStore
	eventQueue      dbpkg.EventQueueStore
	externalActions dbpkg.ExternalActionStore
	orgs            dbpkg.OrgsStore
	teams           dbpkg.TeamsStore
	teamRepos       dbpkg.TeamGitHubReposStore
	jiraRules       dbpkg.JiraStatusRulesStore
	linearRules     dbpkg.LinearTeamRulesStore
	githubGroups    dbpkg.TeamGitHubGroupsStore
	rederive        dbpkg.TaskReDeriveStore
	mergeability    dbpkg.PRMergeabilityStore
	sourceGate      *sourceGate // built over RouterDeps.EventSources; see source_gate.go
	spawner         Delegator
	scorer          Scorer
	ws              *websocket.Hub
	publisher       EventPublisher
	executorID      string
	bootEpoch       int64

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

// RouterDeps is everything a Router is built from. A field is either required,
// and NewRouter refuses a RouterDeps without it, or nil-safe, and its doc says
// what the router does without it. Production wires every field.
type RouterDeps struct {
	// Prompts reads the name of the prompt a tripped breaker reports in its
	// toast. Required.
	Prompts dbpkg.PromptStore
	// Blueprints resolves a trigger's blueprint to its first step's prompt,
	// which the per-(entity, prompt) breaker keys on, and reads the blueprint
	// runs the close-deferral sweep waits on. Required.
	Blueprints dbpkg.BlueprintStore
	// Handlers is the event_handlers store: the rules and triggers an event
	// matches, and the trigger a queued firing names. Required.
	Handlers dbpkg.EventHandlerStore
	// Agents resolves the org's agent, which a fired run records as its actor
	// and a task's claim names. Nil-safe: an auto-fire records no actor and
	// stamps no agent claim, and no team's bot switch (TeamAgents) is read.
	Agents dbpkg.AgentStore
	// TeamAgents reads team_agents.enabled, the per-team bot switch an
	// auto-fire and an automation-conflict notice check. Nil-safe: every
	// team's bot reads as enabled.
	TeamAgents dbpkg.TeamAgentStore
	// Users is the reverse identity lookup (GitHub login, Jira account, Linear
	// user → TF users) the owner ladders and requested-party routing resolve
	// through. Nil-safe: no identity resolves to a member, so ownership falls
	// to the ladder's other tiers and a requested party routes to the
	// handler's own team.
	Users dbpkg.UsersStore
	// Tasks is task lifecycle: dedup, claims, closes and the breaker. Required.
	Tasks dbpkg.TaskStore
	// Conversations finds a task's live conversation: the busy half of the
	// task gate, and the conversation an additive event folds into. Required.
	Conversations dbpkg.ConversationStore
	// Entities is the closed-entity guard, the structural owner tier and the
	// entity-terminating close cascade. Required.
	Entities dbpkg.EntityStore
	// Firings is the per-task firing queue a busy task's trigger is admitted
	// onto and the firing worker claims from. Required.
	Firings dbpkg.PendingFiringsStore
	// Events records each routed event and reads back the ones a queued unit
	// names. Required.
	Events dbpkg.EventStore
	// EventQueue is the durable queue the drain worker (RunEventQueue) claims
	// events from. Required.
	EventQueue dbpkg.EventQueueStore
	// ExternalActions is the ledger the close phase reads to tell whether a
	// task's own run caused the event closing it. Required.
	ExternalActions dbpkg.ExternalActionStore
	// Orgs is per-org iteration for the terminal-state checker and the org
	// settings team routing reads (hosts, the background jobs model).
	// Required.
	Orgs dbpkg.OrgsStore
	// Teams reads a team's auto_delegate_enabled switch, the teams an
	// identity's users belong to for owner routing, and the winning team's
	// name for an automation-conflict notice. Required.
	Teams dbpkg.TeamsStore
	// TeamRepos is the team↔repository tracking gate. Required.
	TeamRepos dbpkg.TeamGitHubReposStore
	// JiraRules is the team↔project tracking gate and the source of the
	// terminal-state checker's Jira done statuses. Required.
	JiraRules dbpkg.JiraStatusRulesStore
	// LinearRules is the team↔Linear-team tracking gate and the source of the
	// terminal-state checker's Linear done states. Required.
	LinearRules dbpkg.LinearTeamRulesStore
	// GitHubGroups maps a GitHub team to TF teams, resolving review_requested /
	// mentioned team visibility. Nil-safe: an event naming a GitHub team routes
	// to the handler's own team.
	GitHubGroups dbpkg.TeamGitHubGroupsStore
	// ReDerive is the score re-evaluation queue the re-derive worker
	// (RunReDeriveQueue) claims from. Required.
	ReDerive dbpkg.TaskReDeriveStore
	// Mergeability is a pull request's last known mergeability, which a
	// conflicts resolution's close reads. Required.
	Mergeability dbpkg.PRMergeabilityStore
	// EventSources is the org event-source policy store the router's pause
	// gate reads, so a source an org admin turned off mints no tasks. Required:
	// this is the one funnel every source's events cross.
	EventSources dbpkg.OrgEventSourceStore
	// Spawner starts, stops and injects into delegated runs. Required.
	Spawner Delegator
	// Scorer is kicked per org each time a routed event creates or bumps a
	// task. Required.
	Scorer Scorer
	// WS is the hub the router broadcasts task changes and toasts on.
	// Required.
	WS *websocket.Hub
	// Publisher is the bus the router mirrors each event's routing disposition
	// sentinel onto, so an async event source such as Slack can learn the
	// outcome. Required.
	Publisher EventPublisher
	// ExecutorID is this process's persistent instance-registry identity,
	// stamped as the lease owner on every event_queue, pending_firings and
	// task_rederive_queue row the router's workers claim: provenance for the
	// reclaim log line, never the write fence, which is the lease generation.
	// Required, because a claim refuses an empty owner.
	ExecutorID string
	// BootEpoch is ExecutorID's boot epoch, stamped beside it. Any value,
	// zero included.
	BootEpoch int64
}

// missing names every required field d leaves unset, in field order.
func (d RouterDeps) missing() []string {
	required := []struct {
		name  string
		value any
	}{
		{"Prompts", d.Prompts},
		{"Blueprints", d.Blueprints},
		{"Handlers", d.Handlers},
		{"Tasks", d.Tasks},
		{"Conversations", d.Conversations},
		{"Entities", d.Entities},
		{"Firings", d.Firings},
		{"Events", d.Events},
		{"EventQueue", d.EventQueue},
		{"ExternalActions", d.ExternalActions},
		{"Orgs", d.Orgs},
		{"Teams", d.Teams},
		{"TeamRepos", d.TeamRepos},
		{"JiraRules", d.JiraRules},
		{"LinearRules", d.LinearRules},
		{"ReDerive", d.ReDerive},
		{"Mergeability", d.Mergeability},
		{"EventSources", d.EventSources},
		{"Spawner", d.Spawner},
		{"Scorer", d.Scorer},
		{"WS", d.WS},
		{"Publisher", d.Publisher},
	}
	var out []string
	for _, f := range required {
		if isNil(f.value) {
			out = append(out, f.name)
		}
	}
	if d.ExecutorID == "" {
		out = append(out, "ExecutorID")
	}
	return out
}

// isNil reports whether v is nil, including a nil pointer (or other nilable
// value) held in a non-nil interface: production wires concrete pointers, and a
// nil *delegate.Spawner passed as a Delegator compares unequal to nil.
func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return rv.IsNil()
	}
	return false
}

// NewRouter builds a Router from deps, refusing one that leaves a required
// field unset. The error names every missing field at once.
func NewRouter(deps RouterDeps) (*Router, error) {
	if missing := deps.missing(); len(missing) > 0 {
		return nil, fmt.Errorf("routing: RouterDeps missing required %s", strings.Join(missing, ", "))
	}
	return &Router{
		prompts:         deps.Prompts,
		blueprints:      deps.Blueprints,
		handlers:        deps.Handlers,
		agents:          deps.Agents,
		teamAgents:      deps.TeamAgents,
		users:           deps.Users,
		tasks:           deps.Tasks,
		conversations:   deps.Conversations,
		entities:        deps.Entities,
		firings:         deps.Firings,
		events:          deps.Events,
		eventQueue:      deps.EventQueue,
		externalActions: deps.ExternalActions,
		orgs:            deps.Orgs,
		teams:           deps.Teams,
		teamRepos:       deps.TeamRepos,
		jiraRules:       deps.JiraRules,
		linearRules:     deps.LinearRules,
		githubGroups:    deps.GitHubGroups,
		rederive:        deps.ReDerive,
		mergeability:    deps.Mergeability,
		sourceGate:      newSourceGate(deps.EventSources),
		spawner:         deps.Spawner,
		scorer:          deps.Scorer,
		ws:              deps.WS,
		publisher:       deps.Publisher,
		executorID:      deps.ExecutorID,
		bootEpoch:       deps.BootEpoch,
		firingWake:      make(chan struct{}, 1),
		rederiveWake:    make(chan struct{}, 1),
	}, nil
}

// breakerPromptID resolves the prompt id the auto-delegate breaker keys on
// for a trigger. The trigger fires a blueprint; the breaker tracks failed
// runs per (entity, prompt), and runs are prompt-keyed, so it tracks the
// blueprint's first step prompt — exactly the wrapped prompt for the 1-step
// blueprints every shipped trigger uses. Returns "" when the blueprint or its
// steps can't be resolved (CountConsecutiveFailedConversationsSystem then matches no
// runs → breaker never trips, the safe default that doesn't block
// auto-delegation on a transient read error).
func (r *Router) breakerPromptID(ctx context.Context, orgID, blueprintID string) string {
	if blueprintID == "" {
		return ""
	}
	steps, err := r.blueprints.ListStepsSystem(ctx, orgID, blueprintID)
	if err != nil || len(steps) == 0 {
		return ""
	}
	return steps[0].StepPromptID
}

// autoDelegateEnabledForTeam returns the team's auto_delegate_enabled
// kill switch. TeamsStore.GetSettingsSystem returns
// domain.DefaultTeamSettings() on missing rows, so a missing team
// settings row reads as false (the schema default; multi-mode's "new
// teams require explicit opt-in" rule). The local-mode sentinel team's
// baseline seed flips this to true, preserving the local-first
// happy-path behavior. An empty team reads as false too.
//
// A store failure returns (false, err) rather than a bare false. Both
// callers still decline to fire — an unreadable switch is never treated as
// permission — but they can tell "the user turned this off" from "we
// couldn't find out," which is the difference between an event that is
// finished and one that deserves another attempt.
func (r *Router) autoDelegateEnabledForTeam(ctx context.Context, teamID string) (bool, error) {
	if teamID == "" {
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
