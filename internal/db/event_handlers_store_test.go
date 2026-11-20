package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// A trigger's breaker compares the predicate a run recorded with the
// trigger's own as text. A shipped predicate that differs from its canonical
// form differs from what any save of the same predicate writes, so that save
// would start the breaker over though nothing the trigger matches changed.
func TestShippedEventHandlers_PredicatesAreCanonical(t *testing.T) {
	for _, h := range ShippedEventHandlers {
		canonical, err := events.ValidatePredicateJSON(h.EventType, h.Predicate)
		if err != nil {
			t.Errorf("%s: predicate %q is refused: %v", h.ID, h.Predicate, err)
			continue
		}
		if canonical != h.Predicate {
			t.Errorf("%s: predicate %q, want its canonical form %q", h.ID, h.Predicate, canonical)
		}
	}
}
