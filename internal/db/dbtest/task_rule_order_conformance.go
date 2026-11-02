package dbtest

import (
	"context"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskRuleOrderFixture is what a backend hands RunTaskRuleOrderConformance:
// the task and handler stores, the org and team they write into, a door that
// seeds a queued task of a named event type (the TaskSeeder mints one type
// only), and the backend's shipped-handler lookup by slug.
type TaskRuleOrderFixture struct {
	Tasks         db.TaskStore
	Handlers      db.EventHandlerStore
	OrgID, TeamID string
	// SeedTask inserts a queued, unclaimed task of eventType on TeamID and
	// returns its id.
	SeedTask func(t *testing.T, eventType string) string
	// HandlerID reads the id of a team's live copy of a shipped handler.
	HandlerID ShippedHandlerIDBySlug
}

// TaskRuleOrderFactory builds a fresh TaskRuleOrderFixture per subtest.
type TaskRuleOrderFactory func(t *testing.T) TaskRuleOrderFixture

// RunTaskRuleOrderConformance pins that the task list's default order ranks a
// task by the live rules covering its event type. A deleted shipped rule stays
// behind as a row with deleted_at set and enabled untouched, so it must leave
// the order the moment it is deleted.
func RunTaskRuleOrderConformance(t *testing.T, factory TaskRuleOrderFactory) {
	t.Helper()

	t.Run("List_default_order_drops_a_deleted_shipped_rule", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()
		// The sort order a task takes when no live, enabled rule covers its
		// event type.
		const unranked = 999

		if err := fx.Handlers.Sync(ctx, fx.OrgID, fx.TeamID, []db.ShippedEventHandler{
			syncRule("order-ci", domain.EventGitHubPRCICheckFailed, "", "CI failed", 0.5, 0),
			syncRule("order-review", domain.EventGitHubPRReviewRequested, "", "Review requested", 0.5, 1),
		}, nil); err != nil {
			t.Fatalf("Sync shipped rules: %v", err)
		}
		ciTask := fx.SeedTask(t, domain.EventGitHubPRCICheckFailed)
		reviewTask := fx.SeedTask(t, domain.EventGitHubPRReviewRequested)

		order := func() ([]string, map[string]int) {
			t.Helper()
			rows, _, err := fx.Tasks.List(ctx, fx.OrgID, queueFilter(), db.ListOpts{Limit: 50})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			ids := make([]string, len(rows))
			sortOrders := make(map[string]int, len(rows))
			for i, row := range rows {
				ids[i] = row.ID
				sortOrders[row.ID] = row.ListSortOrder
			}
			return ids, sortOrders
		}

		ids, sortOrders := order()
		if want := []string{ciTask, reviewTask}; !slices.Equal(ids, want) {
			t.Fatalf("before delete: order = %v, want %v", ids, want)
		}
		if sortOrders[ciTask] != 0 || sortOrders[reviewTask] != 1 {
			t.Fatalf("before delete: sort orders = %v, want %s=0 %s=1", sortOrders, ciTask, reviewTask)
		}

		ruleID := fx.HandlerID(t, fx.TeamID, "order-ci")
		if ruleID == "" {
			t.Fatal("shipped rule order-ci has no live copy after Sync")
		}
		if err := fx.Handlers.Delete(ctx, fx.OrgID, ruleID); err != nil {
			t.Fatalf("Delete shipped rule: %v", err)
		}

		ids, sortOrders = order()
		if want := []string{reviewTask, ciTask}; !slices.Equal(ids, want) {
			t.Errorf("after delete: order = %v, want %v", ids, want)
		}
		if sortOrders[ciTask] != unranked || sortOrders[reviewTask] != 1 {
			t.Errorf("after delete: sort orders = %v, want %s=%d %s=1", sortOrders, ciTask, unranked, reviewTask)
		}
	})
}
