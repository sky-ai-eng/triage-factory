package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/jira"
	"github.com/sky-ai-eng/triage-factory/internal/jiradrift"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// recheckJiraDrift checks a Jira task rule or trigger a save just changed for
// filter values that are gone, so a save that removes them resolves the
// team's notification at once rather than at the next hourly check, and one
// that enables a handler naming them raises it. prior is the row before the
// save; a save that changed neither the filter nor whether the handler is
// enabled changes nothing the check reads, and is not checked.
//
// It runs once the write has committed and never fails it: whatever it cannot
// read is left for the hourly check. It runs before the response is written
// rather than in the background, so the checks of two quick saves of one
// handler cannot land out of order, and it gives up after
// jiraDriftRecheckTimeout, so a slow Jira holds a save that has already
// succeeded only that long.
func (eh *eventHandlersHandler) recheckJiraDrift(ctx context.Context, orgID, userID string, prior, stored domain.EventHandler) {
	if !strings.HasPrefix(stored.EventType, "jira:") {
		return
	}
	if prior.Enabled == stored.Enabled && prior.Kind == stored.Kind && samePredicate(prior.ScopePredicateJSON, stored.ScopePredicateJSON) {
		return
	}
	if !stored.Enabled {
		// A disabled handler matches nothing by choice; nothing to read.
		kinds.JiraPredicateDrift.Set(ctx, orgID, kinds.JiraHandlerFilter{HandlerID: stored.ID}, false)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, jiraDriftRecheckTimeout)
	defer cancel()
	// The save's own catalog check reads the team's tracked projects and the
	// client the same way, so the two judge a filter against one catalog.
	c := newCatalogCheck(ctx, eh.catalogs, orgID, stored.TeamID)
	if err := eh.tx.WithReadTx(ctx, orgID, userID, c.readTracked); err != nil {
		serverLog.Warn("jira drift recheck after a handler save could not read the team's projects", "org", orgID, "handler", stored.ID, "error", err)
		return
	}
	client, err := c.jira()
	if err != nil {
		// A source turned off or never connected has no drift to report: its
		// notifications were resolved when it went away.
		if !errors.Is(err, eventsource.ErrDisabled) && !errors.Is(err, jira.ErrNoJiraSystemCredential) {
			serverLog.Warn("jira drift recheck after a handler save could not reach Jira", "org", orgID, "handler", stored.ID, "error", err)
		}
		return
	}
	jiradrift.NewPass(client).Handler(ctx, orgID, stored, c.jiraRefs)
}

// jiraDriftRecheckTimeout bounds the Jira reads a handler save's drift check
// makes: one per tracked project, plus the priorities.
const jiraDriftRecheckTimeout = 5 * time.Second

// samePredicate reports whether two stored filters are the same text.
func samePredicate(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// resolveJiraDriftOnDelete resolves the drift notification of a deleted Jira
// task rule or trigger: a handler that no longer exists filters on nothing.
func resolveJiraDriftOnDelete(ctx context.Context, orgID string, h domain.EventHandler) {
	if strings.HasPrefix(h.EventType, "jira:") {
		kinds.JiraPredicateDrift.Set(ctx, orgID, kinds.JiraHandlerFilter{HandlerID: h.ID}, false)
	}
}
