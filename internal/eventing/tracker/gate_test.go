package tracker

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// freshAge is "well under the staleness floor" — used by every test
// that isn't specifically exercising the floor itself.
const freshAge = 30 * time.Second

func TestShouldSkipRefresh_QuietPRSkips(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{
			{Name: "ci", Status: "completed", Conclusion: "success"},
		},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	if !shouldSkipRefresh(stored, fresh, freshAge) {
		t.Fatalf("expected skip when updatedAt + SHA unchanged and CI is terminal")
	}
}

func TestShouldSkipRefresh_UpdatedAtAdvancedRefreshes(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:05:00Z",
		HeadSHA:   "abc123",
	}
	if shouldSkipRefresh(stored, fresh, freshAge) {
		t.Fatalf("expected refresh when updatedAt advanced")
	}
}

func TestShouldSkipRefresh_HeadSHAChangedRefreshes(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "def456",
	}
	if shouldSkipRefresh(stored, fresh, freshAge) {
		t.Fatalf("expected refresh when head SHA changed even with same updatedAt")
	}
}

func TestShouldSkipRefresh_InFlightCIRefreshes(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{
			{Name: "lint", Status: "completed", Conclusion: "success"},
			{Name: "test", Status: "in_progress"},
		},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	if shouldSkipRefresh(stored, fresh, freshAge) {
		t.Fatalf("expected refresh when stored snapshot has in-flight CI (updatedAt doesn't bump on check completion)")
	}
}

// TestShouldSkipRefresh_NonCompletedStatusesRefresh asserts the
// gate's correctness contract: any check_run.status other than
// "completed" forces a refresh. Important because hasInFlightCI uses
// `!= "completed"` rather than enumerating queued/in_progress, so a
// regression that switched to an enumeration approach would drop
// pending or yet-to-be-defined statuses on the floor.
func TestShouldSkipRefresh_NonCompletedStatusesRefresh(t *testing.T) {
	statuses := []string{
		"queued",
		"in_progress",
		"pending",   // historical / third-party CI variant
		"requested", // GitHub Actions intermediate state
		"waiting",   // deployment-gated workflows
		"",          // missing — treat as unknown, force refresh
		"weird_future_value",
	}
	for _, status := range statuses {
		t.Run("status="+status, func(t *testing.T) {
			stored := domain.PRSnapshot{
				UpdatedAt: "2026-05-03T12:00:00Z",
				HeadSHA:   "abc123",
				CheckRuns: []domain.CheckRun{{Name: "ci", Status: status}},
			}
			fresh := domain.PRSnapshot{
				UpdatedAt: "2026-05-03T12:00:00Z",
				HeadSHA:   "abc123",
			}
			if shouldSkipRefresh(stored, fresh, freshAge) {
				t.Fatalf("expected refresh when stored has non-completed check_run status %q", status)
			}
		})
	}
}

// TestShouldSkipRefresh_StaleAgeForcesRefresh covers the workflow-rerun
// case: prior run terminal, PR otherwise quiet, but a new check_run
// appeared on the same SHA without bumping updatedAt. Without the
// staleness floor the gate would skip forever. With it, we re-fetch
// at staleRefreshFloor cadence as a worst-case bound.
func TestShouldSkipRefresh_StaleAgeForcesRefresh(t *testing.T) {
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{{Name: "ci", Status: "completed", Conclusion: "success"}},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	// Just past the floor → must refresh.
	if shouldSkipRefresh(stored, fresh, staleRefreshFloor+time.Second) {
		t.Fatalf("expected refresh once age exceeds staleRefreshFloor (catches workflow re-runs)")
	}
	// Just under the floor → still skips.
	if !shouldSkipRefresh(stored, fresh, staleRefreshFloor-time.Second) {
		t.Fatalf("expected skip while age is under staleRefreshFloor")
	}
}

func TestShouldSkipRefresh_NilCheckRunsRefreshes(t *testing.T) {
	// Freshly discovered entity: snapshot was seeded from the lightweight
	// discovery fragment which doesn't return check_runs. Stored.CheckRuns
	// is nil. Skipping here would leave the entity with no CI data forever.
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: nil,
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	if shouldSkipRefresh(stored, fresh, freshAge) {
		t.Fatalf("expected refresh on first cycle after discovery (CheckRuns nil)")
	}
}

func TestShouldSkipRefresh_EmptyCheckRunsCanSkip(t *testing.T) {
	// After a full refresh on a PR with zero check suites, CheckRuns is
	// non-nil empty. That's a different state from nil — it means
	// "polled, nothing here" — and is safe to skip on subsequent cycles.
	stored := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
		CheckRuns: []domain.CheckRun{},
	}
	fresh := domain.PRSnapshot{
		UpdatedAt: "2026-05-03T12:00:00Z",
		HeadSHA:   "abc123",
	}
	if !shouldSkipRefresh(stored, fresh, freshAge) {
		t.Fatalf("expected skip with empty (non-nil) check_runs")
	}
}

func TestShouldSkipRefresh_MissingUpdatedAtRefreshes(t *testing.T) {
	// Pre-field snapshot or parse failure — without updatedAt we have no
	// gate signal, so always refresh.
	stored := domain.PRSnapshot{HeadSHA: "abc123", CheckRuns: []domain.CheckRun{}}
	fresh := domain.PRSnapshot{UpdatedAt: "2026-05-03T12:00:00Z", HeadSHA: "abc123"}
	if shouldSkipRefresh(stored, fresh, freshAge) {
		t.Fatalf("expected refresh when stored.UpdatedAt is empty")
	}
}

func TestShouldSkipRefresh_MissingHeadSHARefreshes(t *testing.T) {
	stored := domain.PRSnapshot{UpdatedAt: "2026-05-03T12:00:00Z", CheckRuns: []domain.CheckRun{}}
	fresh := domain.PRSnapshot{UpdatedAt: "2026-05-03T12:00:00Z", HeadSHA: "abc123"}
	if shouldSkipRefresh(stored, fresh, freshAge) {
		t.Fatalf("expected refresh when stored.HeadSHA is empty")
	}
}
