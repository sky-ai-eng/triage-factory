package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
	"github.com/sky-ai-eng/triage-factory/internal/toast"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// registerSubscribers attaches the in-process bus subscribers. Pollers
// publish onto the bus rather than invoking callbacks directly, so a poll
// cycle, a scorer run, and a UI push all stay decoupled.
func (a *App) registerSubscribers() {
	// Brain-only: every subscriber below either forwards to the websocket
	// hub (control/all) or kicks a brain manager (scorer/profiler/
	// reconciler/marketplace) that an executor never builds. An
	// executor publishes run sentinels onto the bus for the cross-pod relay
	// (TFAC-584) but subscribes to nothing locally.
	if !a.plan.brain {
		return
	}
	// Forward every event to the frontend over the websocket.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "ws-broadcast",
		Handle: a.broadcastEvent,
	})
	// Kick the per-org scorer on poll-complete sentinels.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "scorer",
		Filter: []string{"system:poll:"},
		Handle: a.brainHolderOnly(func(evt domain.Event) { a.scorer.Trigger(evt.OrgID) }),
	})
	// Kick the per-org repo profiler on GitHub poll-complete sentinels. The
	// cycle is TTL-gated, so steady state is ~one staleness check per repo
	// per cycle; it naturally profiles new / stale / newly-reachable
	// (App-only) repos with no "github changed" plumbing. Filtered to
	// source=="github" so a Jira-only cycle doesn't kick a no-op pass.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "profiler",
		Filter: []string{"system:poll:"},
		Handle: a.brainHolderOnly(a.handleProfilerPoll),
	})
	// Kick the per-org artifact reconciler on GitHub poll-complete sentinels.
	// Mirrors the profiler: gated to source=="github" (a Jira cycle can't
	// change a PR/branch's GitHub state), per-org via evt.OrgID. This is the
	// webhook-independent baseline that keeps `artifacts` fresh in both modes
	// (TFAC-464).
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "reconciler",
		Filter: []string{"system:poll:"},
		Handle: a.brainHolderOnly(a.handleReconcilerPoll),
	})
	// Track Jira/GitHub poll completions: gate /api/jira/stock and surface
	// the one-shot "config took effect" toast.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "poll-tracker",
		Filter: []string{"system:poll:"},
		Handle: a.handlePollCompleted,
	})
	// Kick the per-org marketplace listing-stats aggregator on poll-complete
	// sentinels (TFAC-540). Multi-mode only: a.marketplaceStats is nil in
	// local mode (buildAI), so the subscriber is never registered there — no
	// bus overhead for a job local mode can't run anyway. The cycle is itself
	// TTL-gated (marketplacestats.Aggregator), so steady state is ~one no-op
	// check per org per poll cycle regardless of source.
	if a.marketplaceStats != nil {
		a.bus.Subscribe(eventbus.Subscriber{
			Name:   "marketplace-stats",
			Filter: []string{"system:poll:"},
			Handle: a.brainHolderOnly(func(evt domain.Event) { a.marketplaceStats.Trigger(evt.OrgID) }),
		})
	}
	// PR coherence runs from the router's settled disposition rather than the raw
	// GitHub event. The source event is already durable by then, normal handlers
	// have completed, and the disposition names any same-task conversation that
	// already received the event through additive injection.
	//
	// Not brainHolderOnly, unlike the sentinel-driven subscribers above. The bus
	// is in-process and the disposition is published by Router.HandleEvent, whose
	// only caller is the brain's queue-drain worker, so a standby's bus never
	// carries one. The guard would also be wrong on the demotion race it exists
	// for: this kicks no brain-owned manager, and a note reaches its conversation
	// from any pod, so one handled just after a demotion should still land.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "pr-coherence-injection",
		Filter: []string{domain.EventSystemRoutingDisposition},
		Handle: a.spawner.HandlePRCoherence,
	})
}

// brainHolderOnly wraps a subscriber handler so it fires only while this
// process actually holds the brain lease. Subscribers are registered for the
// process lifetime (control/all), but a demoted control standby must not kick
// a per-org AI manager off a stray sentinel — e.g. one emitted by a poll
// cycle that was still in flight at demotion — since the new leader is
// already running that scoring/profile/reconcile work. At
// TF_ROLE=all and local, isBrainHolder is always true, so this is a no-op
// there. Applied only to the sentinel-driven brain-manager kickers, not to
// ws-broadcast or the poll-readiness tracker, which any control pod may serve.
func (a *App) brainHolderOnly(h func(domain.Event)) func(domain.Event) {
	return func(evt domain.Event) {
		if !a.isBrainHolder() {
			return
		}
		h(evt)
	}
}

// handleReconcilerPoll kicks the per-org artifact reconciler for the org whose
// GitHub poll just completed. Reuses pollEventProfilesGitHub's gate: only GitHub
// completions qualify (a Jira cycle can't move a PR/branch's GitHub state), and
// an empty / malformed event is a silent no-op. evt.OrgID scopes the per-org
// Runner; an empty value is dropped by Manager.Trigger.
func (a *App) handleReconcilerPoll(evt domain.Event) {
	orgID, ok := pollEventProfilesGitHub(evt)
	if !ok {
		return
	}
	a.reconciler.Trigger(orgID)
}

// broadcastEvent forwards a bus event to the websocket hub, scoped to the
// originating tenant (system events with an empty OrgID fan out
// everywhere). It also re-emits the legacy "tasks_updated" message on poll
// completion for backward compatibility.
func (a *App) broadcastEvent(evt domain.Event) {
	// system:conversation:* sentinels (TFAC-592) are EE-observable bus mirrors of
	// the conversation_update/message websocket events the spawner's
	// two broadcast choke points already emit — forwarding them here
	// would double every tool call on the wire in a second shape. This
	// is also defense-in-depth for tenant isolation: broadcastEvent fans
	// out org-unstamped events to ALL clients, so a future emitter bug
	// that forgot OrgID must not leak run activity text cross-org.
	if strings.HasPrefix(evt.EventType, "system:conversation:") {
		return
	}
	a.wsHub.Broadcast(websocket.Event{
		Type:  "event",
		OrgID: evt.OrgID,
		Data:  evt,
	})
	if evt.EventType == domain.EventSystemPollCompleted {
		a.wsHub.Broadcast(websocket.Event{
			Type:  "tasks_updated",
			OrgID: evt.OrgID,
			Data:  map[string]any{},
		})
	}
}

// handlePollCompleted reacts to poll-complete sentinels: it records Jira
// readiness (gating /api/jira/stock — TFAC-583's org-scoped
// poll_readiness table, read by whichever control pod's API serves that
// endpoint, not just the one that ran this poll) and surfaces a one-shot
// "first poll complete after config change" toast so users see their
// settings actually took effect.
func (a *App) handlePollCompleted(evt domain.Event) {
	if evt.EventType != domain.EventSystemPollCompleted {
		return
	}
	var meta struct {
		Source    string `json:"source"`
		StartedAt int64  `json:"started_at"`
		Entities  int    `json:"entities"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		pollTrackerLog.Warn("parse poll completion metadata failed", "error", err, "raw_metadata", evt.MetadataJSON)
		return
	}
	ctx := context.Background()
	if meta.Source == "jira" {
		// Pass the poll's started_at so MarkPollComplete can ignore stale
		// sentinels from pre-restart poll goroutines that finish late. A
		// missing field yields StartedAt=0 → a zero time.Time, which
		// MarkPollComplete treats as "unknown generation" and accepts.
		var startedAt time.Time
		if meta.StartedAt != 0 {
			startedAt = time.Unix(0, meta.StartedAt)
		}
		if err := a.stores.PollReadiness.MarkPollComplete(ctx, evt.OrgID, "jira", startedAt); err != nil {
			pollTrackerLog.Warn("mark jira poll complete failed", "org", evt.OrgID, "error", err)
		}
	}
	taken, err := a.stores.PollReadiness.TakeAnnouncePending(ctx, evt.OrgID, meta.Source)
	if err != nil {
		pollTrackerLog.Warn("take announce-pending flag failed", "org", evt.OrgID, "source", meta.Source, "error", err)
		return
	}
	if taken {
		label := "GitHub"
		if meta.Source == "jira" {
			label = "Jira"
		}
		toast.Info(a.wsHub, evt.OrgID, fmt.Sprintf(
			"First %s poll complete — %d %s tracked",
			label, meta.Entities, pluralize(meta.Entities, "entity", "entities"),
		))
	}
}

// handleProfilerPoll kicks a TTL-gated repo-profiling cycle for the org
// whose GitHub poll just completed. evt.OrgID scopes the per-org Runner; an
// empty value is dropped by Manager.Trigger.
func (a *App) handleProfilerPoll(evt domain.Event) {
	orgID, ok := pollEventProfilesGitHub(evt)
	if !ok {
		return
	}
	a.profiler.Trigger(orgID, false)
}

// pollEventProfilesGitHub reports the org to profile for a poll-complete
// event and whether a profiling pass should run. Only GitHub poll
// completions qualify: a Jira completion can't change the tracked repo set,
// so profiling on it would be a no-op staleness scan. Malformed metadata is
// treated as "don't profile" — the poll-tracker subscriber already warns on
// the same event, so this stays silent rather than double-logging.
func pollEventProfilesGitHub(evt domain.Event) (string, bool) {
	if evt.EventType != domain.EventSystemPollCompleted {
		return "", false
	}
	var meta struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(evt.MetadataJSON), &meta); err != nil {
		return "", false
	}
	if meta.Source != "github" {
		return "", false
	}
	return evt.OrgID, true
}

// pluralize picks the singular or plural form based on count, for toast
// copy where "1 entity tracked" reads nicer than a naive "(s)" suffix.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
