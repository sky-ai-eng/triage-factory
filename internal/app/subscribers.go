package app

import (
	"encoding/json"
	"fmt"
	"log"
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
	// Kick the project classifier on poll-complete sentinels; it rotates
	// through orgs internally.
	a.bus.Subscribe(eventbus.Subscriber{
		Name:   "classifier",
		Filter: []string{"system:poll:"},
		Handle: func(evt domain.Event) { a.classifier.Trigger() },
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
		log.Printf("[poll-tracker] warning: failed to parse poll completion metadata: %v; raw metadata=%q", err, evt.MetadataJSON)
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

// pluralize picks the singular or plural form based on count, for toast
// copy where "1 entity tracked" reads nicer than a naive "(s)" suffix.
func pluralize(n int, singular, plural string) string {
	if n == 1 {
		return singular
	}
	return plural
}
