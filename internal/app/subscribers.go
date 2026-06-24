package app

import (
	"encoding/json"
	"fmt"
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
	// Forward every event to the frontend over the websocket.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "ws-broadcast",
		Handle: a.broadcastEvent,
	})
	// Kick the per-org scorer on poll-complete sentinels.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "scorer",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) { a.scorer.Trigger(evt.OrgID) },
	})
	// Kick the per-org project classifier on poll-complete sentinels.
	// evt.OrgID scopes the per-org Runner (like the scorer above); an empty
	// value is dropped by Manager.Trigger.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "classifier",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) { a.classifier.Trigger(evt.OrgID) },
	})
	// Kick the per-org repo profiler on GitHub poll-complete sentinels. The
	// cycle is TTL-gated, so steady state is ~one staleness check per repo
	// per cycle; it naturally profiles new / stale / newly-reachable
	// (App-only) repos with no "github changed" plumbing. Filtered to
	// source=="github" so a Jira-only cycle doesn't kick a no-op pass.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "profiler",
		Filter: []string{"system:poll:"},
		Handle: a.handleProfilerPoll,
	})
	// Track Jira/GitHub poll completions: gate /api/jira/stock and surface
	// the one-shot "config took effect" toast.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "poll-tracker",
		Filter: []string{"system:poll:"},
		Handle: a.handlePollCompleted,
	})
}

// broadcastEvent forwards a bus event to the websocket hub, scoped to the
// originating tenant (system events with an empty OrgID fan out
// everywhere). It also re-emits the legacy "tasks_updated" message on poll
// completion for backward compatibility.
func (a *App) broadcastEvent(evt domain.Event) {
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

// handlePollCompleted reacts to poll-complete sentinels: it lets the server
// know Jira snapshots are ready (gating /api/jira/stock) and surfaces a
// one-shot "first poll complete after config change" toast so users see
// their settings actually took effect.
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
	if meta.Source == "jira" {
		// Pass the poll's started_at so MarkJiraPollComplete can ignore
		// stale sentinels from pre-restart poll goroutines that finish late.
		// A missing field yields StartedAt=0 → a zero time.Time, which
		// MarkJiraPollComplete treats as "unknown generation" and accepts.
		var startedAt time.Time
		if meta.StartedAt != 0 {
			startedAt = time.Unix(0, meta.StartedAt)
		}
		a.srv.MarkJiraPollComplete(startedAt)
	}
	if a.announce.shouldAnnounce(meta.Source) {
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
