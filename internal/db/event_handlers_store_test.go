package db

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// A trigger's breaker compares the predicate a run recorded with the
// trigger's own as text, and the API stores a predicate in canonical form.
// A shipped predicate in any other form would be rewritten by the first save
// through the API that names it, which the trigger config panel does on every
// save, and the breaker would start over though nothing it matches changed.
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
