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

// TestPendingFiringsValidates pins the same for the firing kind, its key,
// and the exact text of its claim filter on each dialect.
func TestPendingFiringsValidates(t *testing.T) {
	for _, d := range []workitem.Dialect{workitem.SQLite, workitem.Postgres} {
		if err := PendingFirings(d).Validate(); err != nil {
			t.Errorf("%s: %v", d, err)
		}
	}
	if got := PendingFiringKey("task-1", "trig-1"); got != "task-1:trig-1" {
		t.Errorf("firing key = %q", got)
	}
	const pg = "NOT EXISTS (SELECT 1 FROM conversations r WHERE r.org_id = t.org_id AND r.task_id = t.task_id AND r.ended_at IS NULL AND r.parent_conversation_id IS NULL AND (r.status IS NULL OR r.status NOT IN ('completed','failed')))"
	const lite = "NOT EXISTS (SELECT 1 FROM conversations r WHERE r.task_id = t.task_id AND r.ended_at IS NULL AND r.parent_conversation_id IS NULL AND (r.status IS NULL OR r.status NOT IN ('completed','failed')))"
	if got := PendingFiringsClaimFilter(workitem.Postgres); got != pg {
		t.Errorf("postgres claim filter = %q", got)
	}
	if got := PendingFiringsClaimFilter(workitem.SQLite); got != lite {
		t.Errorf("sqlite claim filter = %q", got)
	}
}
