package routing

import (
	"context"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/notify/kinds"
)

// DefaultBreakerSyncInterval is how often RunBreakerSync syncs, which bounds
// how long kinds.BreakerTripped stays open after its breaker stops holding.
const DefaultBreakerSyncInterval = 30 * time.Second

// breakerHolds reports whether trigger's breaker holds on entityID, the gate
// both the event-time path and the firing worker read before a run is fired:
// the trigger's runs there have failed breaker_threshold times in a row
// (TaskStore.BreakerStreakSystem). A breaker that holds opens or bumps
// kinds.BreakerTripped, on every refusal rather than only the first, so a
// failed write corrects itself on the next one. subject is the entity's key
// when the caller has it; an empty one is read.
func (r *Router) breakerHolds(ctx context.Context, orgID string, trigger domain.EventHandler, entityID, subject string) (bool, error) {
	threshold := derefIntDefault(trigger.BreakerThreshold, 0)
	streak, err := r.tasks.BreakerStreakSystem(ctx, orgID, entityID, trigger.BlueprintID)
	if err != nil {
		return false, err
	}
	if streak.Failures < threshold {
		return false, nil
	}
	routerLog.Info("breaker holds",
		"entity", entityID, "trigger", trigger.ID, "blueprint", trigger.BlueprintID, "failures", streak.Failures, "threshold", threshold)
	kinds.BreakerTripped.Set(ctx, orgID, r.breakerOccurrence(ctx, orgID, trigger, entityID, subject, streak), true)
	return true, nil
}

// breakerOccurrence is trigger's breaker on entityID as kinds.BreakerTripped
// raises it. A failed read of the entity leaves the subject empty, which the
// copy reads as "this task": the name is display, and the gate's answer does
// not wait on it.
func (r *Router) breakerOccurrence(ctx context.Context, orgID string, trigger domain.EventHandler, entityID, subject string, streak domain.BreakerStreak) kinds.Breaker {
	if subject == "" {
		if e, err := r.entities.GetSystem(ctx, orgID, entityID); err != nil {
			routerLog.Warn("breaker: entity read failed; the notification names no subject", "entity", entityID, "error", err)
		} else if e != nil {
			subject = e.SourceID
		}
	}
	return kinds.Breaker{
		TriggerID:      trigger.ID,
		EntityID:       entityID,
		TeamID:         trigger.TeamID,
		EventType:      trigger.EventType,
		Subject:        subject,
		Failures:       streak.Failures,
		ConversationID: streak.LastConversationID,
	}
}

// RunBreakerSync resolves kinds.BreakerTripped every interval until ctx is
// cancelled, in every org where it is open. The gate opens it; this resolves
// it, because a breaker stops holding through more doors than the gate sees: a
// run a person started completing, the entity closing, and the trigger turned
// off, deleted or given a higher threshold, none of which fires the trigger
// again. It runs once, on the brain.
func (r *Router) RunBreakerSync(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.syncBreakers(ctx)
		}
	}
}

// syncBreakers is one pass. It resolves every open occurrence whose breaker no
// longer holds and keeps the rest as they are; it opens only a breaker that
// started holding during the pass (reopenTrippedDuring), so a breaker that
// held before anything opened the kind is raised by the gate when it next
// refuses, not here. An org whose read fails is left as it is.
func (r *Router) syncBreakers(ctx context.Context) {
	orgs, err := kinds.BreakerTripped.OpenOrgs(ctx)
	if err != nil {
		routerLog.Warn("list orgs with a tripped-breaker notification failed; resolving none this pass", "error", err)
		return
	}
	for _, orgID := range orgs {
		if ctx.Err() != nil {
			return
		}
		before, err := r.tasks.ListTrippedBreakersSystem(ctx, orgID)
		if err != nil {
			routerLog.Warn("read the org's tripped breakers failed; leaving their notifications as they are",
				"org_id", orgID, "error", err)
			continue
		}
		kinds.BreakerTripped.Sync(ctx, orgID, nil, breakerKeys(before)...)
		r.reopenTrippedDuring(ctx, orgID, before)
	}
}

// reopenTrippedDuring opens the breakers that started holding while a pass was
// between its read and its sync. A gate that refused in that window opened its
// occurrence after the read, so the sync, told to keep only what the read saw,
// resolved it. Anything holding now that the read did not see is one of
// those, or started holding since, where a second open is a bump.
func (r *Router) reopenTrippedDuring(ctx context.Context, orgID string, before []domain.TrippedBreaker) {
	after, err := r.tasks.ListTrippedBreakersSystem(ctx, orgID)
	if err != nil {
		routerLog.Warn("re-read the org's tripped breakers failed; one that tripped during this pass may stay resolved until its gate refuses again",
			"org_id", orgID, "error", err)
		return
	}
	seen := make(map[domain.TrippedBreaker]bool, len(before))
	for _, b := range before {
		seen[b] = true
	}
	for _, b := range after {
		if seen[b] {
			continue
		}
		trigger, err := r.handlers.GetSystem(ctx, orgID, b.TriggerID)
		if err != nil || trigger == nil {
			routerLog.Warn("re-read a breaker's trigger failed; its notification opens when its gate refuses again",
				"org_id", orgID, "trigger", b.TriggerID, "error", err)
			continue
		}
		if _, err := r.breakerHolds(ctx, orgID, *trigger, b.EntityID, ""); err != nil {
			routerLog.Warn("re-read a breaker failed; its notification opens when its gate refuses again",
				"org_id", orgID, "trigger", b.TriggerID, "entity", b.EntityID, "error", err)
		}
	}
}

// breakerKeys is bs as the occurrences Sync keeps, which it reads only the
// key of.
func breakerKeys(bs []domain.TrippedBreaker) []kinds.Breaker {
	out := make([]kinds.Breaker, len(bs))
	for i, b := range bs {
		out[i] = kinds.Breaker{TriggerID: b.TriggerID, EntityID: b.EntityID}
	}
	return out
}
