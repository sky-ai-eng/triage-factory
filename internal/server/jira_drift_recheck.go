package server

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
// It runs after the write and never fails it: the save has already been
// answered for, and whatever this cannot read is left for the hourly check.
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
	var rules []domain.JiraProjectStatusRules
	if err := eh.tx.WithReadTx(ctx, orgID, userID, func(tx db.TxStores) error {
		settings, err := tx.Orgs.GetSettings(ctx, orgID)
		if err != nil {
			return fmt.Errorf("read org settings: %w", err)
		}
		if site := domain.EntityScope("jira", settings); site != "" {
			if rules, err = tx.JiraStatusRules.ListForTeam(ctx, stored.TeamID, site); err != nil {
				return fmt.Errorf("read team tracked jira projects: %w", err)
			}
		}
		return nil
	}); err != nil {
		serverLog.Warn("jira drift recheck after a handler save could not read the team's projects", "org", orgID, "handler", stored.ID, "error", err)
		return
	}
	client, err := eh.catalogs.jiraCatalogClient(ctx, orgID)
	if err != nil {
		// A source turned off or never connected has no drift to report: its
		// notifications were resolved when it went away.
		if !errors.Is(err, eventsource.ErrDisabled) && !errors.Is(err, jira.ErrNoJiraSystemCredential) {
			serverLog.Warn("jira drift recheck after a handler save could not reach Jira", "org", orgID, "handler", stored.ID, "error", err)
		}
		return
	}
	jiradrift.NewPass(client).Handler(ctx, orgID, stored, rules)
}

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
