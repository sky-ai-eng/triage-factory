package postgres

import (
	"os"
	"strings"
	"testing"
)

// TestTaskMemoryWaitHoldsTheOwedPredicate pins tf.task_memory_wait's WHERE to
// taskMemoryOwedRowSQL, and so to db.ConversationHasRunSQL inside it. The
// function body is SQL in the baseline; the predicate is Go that the memory
// sweep and the failing-memory read still interpolate. If the two drifted, the
// task read and the claim gate would wait on one rule while the sweep that
// settles the wait listed another.
func TestTaskMemoryWaitHoldsTheOwedPredicate(t *testing.T) {
	raw, err := os.ReadFile("../migrations-postgres/202605130001_pg_baseline.sql")
	if err != nil {
		t.Fatalf("read the baseline: %v", err)
	}
	baseline := string(raw)
	const head = "CREATE FUNCTION tf.task_memory_wait("
	if n := strings.Count(baseline, head); n != 1 {
		t.Fatalf("the baseline defines tf.task_memory_wait %d times, want once", n)
	}
	start := strings.Index(baseline, head)
	end := strings.Index(baseline[start:], "\n$$;")
	if end < 0 {
		t.Fatal("tf.task_memory_wait has no closing $$;")
	}
	body := squashSpace(baseline[start : start+end])
	want := squashSpace(taskMemoryOwedRowSQL("p_org_id", "p_task_id"))
	if !strings.Contains(body, want) {
		t.Errorf("tf.task_memory_wait does not hold taskMemoryOwedRowSQL over its parameters.\nwant, whitespace collapsed:\n%s\nbody:\n%s", want, body)
	}
}

// squashSpace collapses every run of whitespace to one space, so the pin
// compares the predicate's tokens and not its indentation.
func squashSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
