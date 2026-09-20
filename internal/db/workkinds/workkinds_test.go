package workkinds

import (
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db/workitem"
)

// TestEventQueueValidates pins that the declared kind is one the package
// accepts on both dialects, so a policy edit that breaks the lease ordering
// fails here rather than at the first claim.
func TestEventQueueValidates(t *testing.T) {
	for _, d := range []workitem.Dialect{workitem.SQLite, workitem.Postgres} {
		if err := EventQueue(d).Validate(); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	if got := EventQueueCloseOwedKey("e1"); got != "close_owed:e1" {
		t.Errorf("close obligation key = %q", got)
	}
}
