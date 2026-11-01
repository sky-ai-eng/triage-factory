package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// referenceCheckEventType is the event type the tests below hang a stub check
// on. Its predicate's has_label field stands in for a reference: the stub
// refuses the label "forbidden".
const referenceCheckEventType = "github:pr:opened"

// checkCall is one invocation of the stub check.
type checkCall struct {
	teamID           string
	predicate, prior string
}

// stubPredicateReferenceCheck installs a check for referenceCheckEventType
// that refuses a predicate naming "forbidden", returning the calls it
// receives. Whatever was registered before is restored on cleanup.
func stubPredicateReferenceCheck(t *testing.T) *[]checkCall {
	t.Helper()
	calls := &[]checkCall{}
	predicateReferenceMu.Lock()
	prev, had := predicateReferenceChecks[referenceCheckEventType]
	predicateReferenceChecks[referenceCheckEventType] = func(_ context.Context, _ db.TxStores, _, teamID string, predicate, prior *string) ([]PredicateReferenceFault, error) {
		*calls = append(*calls, checkCall{teamID: teamID, predicate: orNil(predicate), prior: orNil(prior)})
		if predicate != nil && strings.Contains(*predicate, "forbidden") {
			return []PredicateReferenceFault{{Key: "has_label", Message: `has_label: "forbidden" is refused`}}, nil
		}
		return nil, nil
	}
	predicateReferenceMu.Unlock()
	t.Cleanup(func() {
		predicateReferenceMu.Lock()
		defer predicateReferenceMu.Unlock()
		if had {
			predicateReferenceChecks[referenceCheckEventType] = prev
		} else {
			delete(predicateReferenceChecks, referenceCheckEventType)
		}
	})
	return calls
}

func orNil(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// labelRule creates a rule on referenceCheckEventType scoped to label.
func labelRule(t *testing.T, s *Server, label string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSON(t, s, http.MethodPost, "/api/event-handlers/rules", map[string]any{
		"event_type":      referenceCheckEventType,
		"name":            "labelled",
		"scope_predicate": map[string]any{"has_label": label},
	})
}
