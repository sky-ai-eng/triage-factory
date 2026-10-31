package poller

import (
	"context"
	"sync"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	jiraclient "github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/jira/jiradrift"
	"github.com/sky-ai-eng/triage-factory/internal/telemetry"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// jiraDriftInterval is how often an org's Jira configuration is checked for
// values Jira no longer has. A status or a priority is deleted, or a workflow
// changed, a few times a year, and the check costs a request per tracked
// project, so it rides one Jira cycle an hour rather than every one. A rules
// save that reads a project's workflow, and a task rule or trigger save,
// check what they changed at once, so the wait is only for a change made in
// Jira.
const jiraDriftInterval = time.Hour

// jiraDriftClock is when each org's drift was last checked. Memory only: a new
// process or lease holder checks on its first cycle, which costs one early
// pass and loses nothing, since the pass keeps what it cannot read.
type jiraDriftClock struct {
	mu sync.Mutex
	at map[string]time.Time
}

// due reports whether orgID is due a check at now, and if so marks it
// checked: a pass that fails is retried at the next interval, not on every
// cycle until it succeeds.
func (c *jiraDriftClock) due(orgID string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.at[orgID]; ok && now.Sub(last) < jiraDriftInterval {
		return false
	}
	if c.at == nil {
		c.at = map[string]time.Time{}
	}
	c.at[orgID] = now
	return true
}

func (c *jiraDriftClock) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = nil
}

// checkJiraDrift checks, when orgID is due, its teams' status rules and its
// enabled Jira task rules and triggers against what Jira has now. rules is
// every team's rules on the org's Jira site, refreshed from the registry, and
// client is the cycle's service client. A handler read that fails leaves the
// handlers to the next pass rather than resolving them on half a list.
func (m *Manager) checkJiraDrift(ctx context.Context, client *jiraclient.Client, orgID string, rules []domain.JiraProjectStatusRules, now time.Time) {
	if m.EventHandlers == nil || !m.jiraDrift.due(orgID, now) {
		return
	}
	ctx, span := tracer.Start(ctx, "poll.jira.drift",
		trace.WithAttributes(telemetry.Source("jira"), telemetry.OrgID(orgID), telemetry.Count(len(rules))))
	defer span.End()

	pass := jiradrift.NewPass(client)
	pass.Rules(ctx, orgID, rules)

	var handlers []domain.EventHandler
	for _, eventType := range jiradrift.EventTypes() {
		hs, err := m.EventHandlers.GetEnabledForEventSystem(ctx, orgID, eventType)
		if err != nil {
			span.SetStatus(codes.Error, "list handlers")
			jiraLog.ErrorContext(ctx, "list jira handlers for the drift check failed; checking them next pass",
				"org", orgID, "event_type", eventType, "error", err)
			return
		}
		handlers = append(handlers, hs...)
	}
	pass.Handlers(ctx, orgID, rules, handlers)
}

// noJiraDrift is the cycle of an org whose teams track no Jira project on its
// site, with rules read to say so: nothing can have drifted, so every open
// occurrence resolves. It needs no Jira call, so it runs on every such cycle
// rather than on the hourly clock, and what was open when the last project
// was untracked resolves on the next cycle.
func (m *Manager) noJiraDrift(ctx context.Context, orgID string) {
	if m.EventHandlers == nil {
		return
	}
	jiradrift.Untracked(ctx, orgID)
}
