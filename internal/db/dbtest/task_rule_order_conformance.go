package dbtest

import (
	"context"
	"slices"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TaskRuleOrderFixture is what a backend hands RunTaskRuleOrderConformance:
// the task, handler and team stores, the org and team they write into, a door
// that seeds a queued task of a named event type (the TaskSeeder mints one
// type only), the backend's shipped-handler lookup by slug, and raw doors for
// a second team and a rule on it (SQLite's Create pins every row to the local
// team).
type TaskRuleOrderFixture struct {
	Tasks         db.TaskStore
	Handlers      db.EventHandlerStore
	Teams         db.TeamsStore
	OrgID, TeamID string
	// SeedTask inserts a queued, unclaimed task of eventType on TeamID and
	// returns its id.
	SeedTask func(t *testing.T, eventType string) string
	// HandlerID reads the id of a team's live copy of a shipped handler.
	HandlerID ShippedHandlerIDBySlug
	// SeedTeam inserts another team in OrgID and returns its id.
	SeedTeam func(t *testing.T) string
	// SeedRule inserts an enabled user rule on teamID for eventType at
	// sortOrder and returns its id.
	SeedRule func(t *testing.T, teamID, eventType string, sortOrder int) string
}

// TaskRuleOrderFactory builds a fresh TaskRuleOrderFixture per subtest.
type TaskRuleOrderFactory func(t *testing.T) TaskRuleOrderFixture

// RunTaskRuleOrderConformance pins that the task list's default order ranks a
// task by the rules that would match its event type: a deleted shipped rule,
// which stays behind as a row with deleted_at set and enabled untouched, and
// a rule on an archived team leave the order, and a restored team's rule
// comes back.
func RunTaskRuleOrderConformance(t *testing.T, factory TaskRuleOrderFactory) {
	t.Helper()
	ctx := context.Background()
	// The sort order a task takes when no rule that would match covers its
	// event type.
	const unranked = 999

	// order reads the queue in the default order and returns the ids and
	// each task's ListSortOrder.
	order := func(t *testing.T, fx TaskRuleOrderFixture) ([]string, map[string]int) {
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
	// expect asserts the queue lists first then second, at the given sort
	// orders.
	expect := func(t *testing.T, fx TaskRuleOrderFixture, when, first string, firstOrder int, second string, secondOrder int) {
		t.Helper()
		ids, sortOrders := order(t, fx)
		if want := []string{first, second}; !slices.Equal(ids, want) {
			t.Errorf("%s: order = %v, want %v", when, ids, want)
		}
		if sortOrders[first] != firstOrder || sortOrders[second] != secondOrder {
			t.Errorf("%s: sort orders = %v, want %s=%d %s=%d", when, sortOrders, first, firstOrder, second, secondOrder)
		}
	}

	t.Run("List_default_order_drops_a_deleted_shipped_rule", func(t *testing.T) {
		fx := factory(t)
		if err := fx.Handlers.Sync(ctx, fx.OrgID, fx.TeamID, []db.ShippedEventHandler{
			syncRule("order-ci", domain.EventGitHubPRCICheckFailed, "", "CI failed", 0.5, 0),
			syncRule("order-review", domain.EventGitHubPRReviewRequested, "", "Review requested", 0.5, 1),
		}, nil); err != nil {
			t.Fatalf("Sync shipped rules: %v", err)
		}
		ciTask := fx.SeedTask(t, domain.EventGitHubPRCICheckFailed)
		reviewTask := fx.SeedTask(t, domain.EventGitHubPRReviewRequested)
		expect(t, fx, "before delete", ciTask, 0, reviewTask, 1)

		ruleID := fx.HandlerID(t, fx.TeamID, "order-ci")
		if ruleID == "" {
			t.Fatal("shipped rule order-ci has no live copy after Sync")
		}
		if err := fx.Handlers.Delete(ctx, fx.OrgID, ruleID); err != nil {
			t.Fatalf("Delete shipped rule: %v", err)
		}
		expect(t, fx, "after delete", reviewTask, 1, ciTask, unranked)
	})

	t.Run("List_default_order_drops_an_archived_teams_rule", func(t *testing.T) {
		fx := factory(t)
		other := fx.SeedTeam(t)
		fx.SeedRule(t, other, domain.EventGitHubPRCICheckFailed, 0)
		fx.SeedRule(t, fx.TeamID, domain.EventGitHubPRReviewRequested, 1)
		ciTask := fx.SeedTask(t, domain.EventGitHubPRCICheckFailed)
		reviewTask := fx.SeedTask(t, domain.EventGitHubPRReviewRequested)
		expect(t, fx, "before archive", ciTask, 0, reviewTask, 1)

		if _, err := fx.Teams.Archive(ctx, other); err != nil {
			t.Fatalf("Archive: %v", err)
		}
		expect(t, fx, "after archive", reviewTask, 1, ciTask, unranked)

		if _, err := fx.Teams.Restore(ctx, other); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		expect(t, fx, "after restore", ciTask, 0, reviewTask, 1)
	})
}
