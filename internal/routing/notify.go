package routing

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/eventsource"
	"github.com/sky-ai-eng/triage-factory/internal/notify"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// notifyConditionTypes is every event type a typed close relation closes. A
// notify rule on one raises a condition, which that relation resolves; on any
// other type it posts a notice, which expires. A terminating relation does not
// count: its close set is every type of the entity, and what it says is that
// the entity ended, not that the situation an event described stopped being
// true.
var notifyConditionTypes = func() map[string]bool {
	m := map[string]bool{}
	for _, rel := range closeRelations {
		if rel.terminatesEntity {
			continue
		}
		for _, et := range rel.closes {
			m[et] = true
		}
	}
	return m
}()

// raiseNotifications tells the audiences of the notify rules whose predicate
// matched evt, keeping only the rules whose team the strict team↔scope gate
// shows tracks the entity. A rule with no team is dropped too: every handler
// names one, so a row without is not one to trust. notify resolves who each
// rule tells and writes nothing on a failure, which it logs and counts;
// routing never waits on it or fails for it.
func (r *Router) raiseNotifications(ctx context.Context, orgID string, evt domain.Event, entity domain.Entity, matched []domain.EventHandler, cache teamScopeCache) {
	rules := make([]notify.EventRule, 0, len(matched))
	for _, h := range matched {
		if h.TeamID == "" || !r.teamScopeCached(ctx, evt, h.TeamID, cache).strict {
			continue
		}
		rules = append(rules, notify.EventRule{
			HandlerID: h.ID, TeamID: h.TeamID, Name: h.Name,
			Audience: h.Audience, CreatorUserID: h.CreatorUserID,
		})
	}
	kinds.Event.Raise(ctx, orgID, notify.EventRaise{
		EventID:   evt.ID,
		EventType: evt.EventType,
		Source:    eventsource.KindOf(evt.EventType),
		DedupKey:  evt.DedupKey,
		Entity:    entity,
		Condition: notifyConditionTypes[evt.EventType],
		Rules:     rules,
	})
}
