package db

import (
	"errors"
	"fmt"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ValidateEventHandlerForCreate ensures the incoming handler satisfies its
// kind's shape contract before the DB sees it. Catching the violation here
// surfaces a precise error like "trigger requires blueprint_id" instead of the
// generic integrity-violation that the per-kind CHECK constraints would
// otherwise produce.
//
// Used by both the SQLite and Postgres EventHandlerStore.Create and Update paths.
// Mutates h.TriggerType to normalize an empty value to the v1 default.
func ValidateEventHandlerForCreate(h *domain.EventHandler) error {
	switch h.Kind {
	case domain.EventHandlerKindRule:
		if h.Name == "" {
			return errors.New("event_handlers Create: rule requires name")
		}
		if h.DefaultPriority == nil || h.SortOrder == nil {
			return errors.New("event_handlers Create: rule requires default_priority and sort_order")
		}
		if h.BlueprintID != "" || h.BreakerThreshold != nil || h.MinAutonomySuitability != nil {
			return errors.New("event_handlers Create: rule must not populate trigger-only fields")
		}
	case domain.EventHandlerKindTrigger:
		if h.BlueprintID == "" {
			return errors.New("event_handlers Create: trigger requires blueprint_id")
		}
		if h.BreakerThreshold == nil || h.MinAutonomySuitability == nil {
			return errors.New("event_handlers Create: trigger requires breaker_threshold and min_autonomy_suitability")
		}
		if h.DefaultPriority != nil || h.SortOrder != nil || h.Name != "" {
			return errors.New("event_handlers Create: trigger must not populate rule-only fields")
		}
		if h.TriggerType == "" {
			h.TriggerType = domain.TriggerTypeEvent
		}
		if h.TriggerType != domain.TriggerTypeEvent {
			return fmt.Errorf("event_handlers Create: unsupported trigger_type %q", h.TriggerType)
		}
	default:
		return fmt.Errorf("event_handlers Create: unknown kind %q", h.Kind)
	}
	return nil
}
