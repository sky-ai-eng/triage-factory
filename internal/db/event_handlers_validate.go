package db

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ValidateEventHandler ensures a handler about to be written is one the table
// can hold and the router can act on: a known mode, a name, a priority and an
// autonomy floor in 0–1, a sort order and a positive breaker threshold that
// fit 32 bits, and a blueprint when the mode is automate. Catching a
// violation here surfaces a precise error instead of the generic integrity
// violation a CHECK would produce, and covers the ranges no CHECK states.
//
// Both dialects' Create and Update call it. The API refuses the same values
// earlier with the field named, so a caller only meets this error by writing
// through the store directly.
func ValidateEventHandler(h domain.EventHandler) error {
	var faults []string
	if !domain.ValidEventHandlerMode(h.Mode) {
		faults = append(faults, fmt.Sprintf("unknown mode %q", h.Mode))
	}
	if strings.TrimSpace(h.Name) == "" {
		faults = append(faults, "name is required")
	}
	if h.DefaultPriority < 0 || h.DefaultPriority > 1 {
		faults = append(faults, "default_priority must be between 0 and 1")
	}
	if h.SortOrder < math.MinInt32 || h.SortOrder > math.MaxInt32 {
		faults = append(faults, "sort_order is out of range")
	}
	if h.BreakerThreshold <= 0 || h.BreakerThreshold > math.MaxInt32 {
		faults = append(faults, "breaker_threshold must be between 1 and 2147483647")
	}
	if h.MinAutonomySuitability < 0 || h.MinAutonomySuitability > 1 {
		faults = append(faults, "min_autonomy_suitability must be between 0 and 1")
	}
	if h.Mode == domain.EventHandlerModeAutomate && h.BlueprintID == "" {
		faults = append(faults, "automate requires a blueprint")
	}
	if len(faults) > 0 {
		return errors.New("event handler: " + strings.Join(faults, "; "))
	}
	return nil
}

// HandlerContentChanged reports whether h's content fields differ from a
// stored row's — the user_modified signal both dialects' Update stamp from.
// Content is the predicate, name, priority, blueprint, breaker threshold and
// autonomy floor; mode and sort_order are the team's to own and never count.
func HandlerContentChanged(cur TeamHandlerRow, h domain.EventHandler) bool {
	var pred string
	if h.ScopePredicateJSON != nil {
		pred = *h.ScopePredicateJSON
	}
	return !PredicateJSONEqual(cur.Predicate, pred) ||
		cur.Name != h.Name ||
		cur.DefaultPriority != h.DefaultPriority ||
		cur.BlueprintID != h.BlueprintID ||
		cur.BreakerThreshold != h.BreakerThreshold ||
		cur.MinAutonomySuitability != h.MinAutonomySuitability
}
