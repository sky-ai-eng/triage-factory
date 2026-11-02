package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// TriggerDeleteFixture is what a backend hands RunTriggerDeleteConformance:
// the handler store, the org and team it writes into, and raw doors for the
// row that references a trigger, which no handler-store read reaches.
type TriggerDeleteFixture struct {
	Handlers      db.EventHandlerStore
	OrgID, TeamID string
	// Blueprint seeds a blueprint on TeamID for a trigger to bind to and
	// returns its id.
	Blueprint func(t *testing.T) string
	// Run seeds a conversation whose trigger_id is triggerID, the row a run
	// of that trigger leaves behind, and returns its id.
	Run func(t *testing.T, triggerID string) string
	// RunTriggerID reads the conversation's trigger_id ("" when NULL). It is
	// a raw read because no store read projects the column.
	RunTriggerID func(t *testing.T, conversationID string) string
}

// TriggerDeleteFactory builds a fresh TriggerDeleteFixture per subtest.
type TriggerDeleteFactory func(t *testing.T) TriggerDeleteFixture

// RunTriggerDeleteConformance pins EventHandlerStore.Delete on a trigger that
// has run. The conversation references the trigger with no ON DELETE action,
// so a hard delete would be refused: the delete succeeds, the trigger leaves
// every handler read, and the conversation keeps its trigger_id.
func RunTriggerDeleteConformance(t *testing.T, factory TriggerDeleteFactory) {
	t.Helper()

	t.Run("Delete_TriggerARunReferences", func(t *testing.T) {
		fx := factory(t)
		ctx := context.Background()
		breaker, minAutonomy := 3, 0.0
		blueprintID := fx.Blueprint(t)
		trigger, err := fx.Handlers.Create(ctx, fx.OrgID, fx.TeamID, domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindTrigger,
			BlueprintID: blueprintID, EventType: domain.EventGitHubPRCICheckFailed,
			Enabled: true, BreakerThreshold: &breaker, MinAutonomySuitability: &minAutonomy,
		})
		if err != nil {
			t.Fatalf("Create trigger: %v", err)
		}
		conversationID := fx.Run(t, trigger.ID)

		if err := fx.Handlers.Delete(ctx, fx.OrgID, trigger.ID); err != nil {
			t.Fatalf("Delete trigger a conversation references: %v", err)
		}

		if got, err := fx.Handlers.Get(ctx, fx.OrgID, trigger.ID); err != nil || got != nil {
			t.Errorf("Get after Delete = %+v, %v; want nil, nil", got, err)
		}
		listed, total, err := fx.Handlers.List(ctx, fx.OrgID, db.EventHandlerListFilter{}, db.ListOpts{Limit: 50})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, h := range listed {
			if h.ID == trigger.ID {
				t.Errorf("List after Delete still includes trigger %s (total %d)", trigger.ID, total)
			}
		}
		if bound, err := fx.Handlers.ListForBlueprint(ctx, fx.OrgID, blueprintID); err != nil || len(bound) != 0 {
			t.Errorf("ListForBlueprint after Delete = %d rows, %v; want 0", len(bound), err)
		}
		enabled, err := fx.Handlers.GetEnabledForEvent(ctx, fx.OrgID, domain.EventGitHubPRCICheckFailed)
		if err != nil {
			t.Fatalf("GetEnabledForEvent: %v", err)
		}
		for _, h := range enabled {
			if h.ID == trigger.ID {
				t.Errorf("GetEnabledForEvent after Delete still includes trigger %s", trigger.ID)
			}
		}

		if got := fx.RunTriggerID(t, conversationID); got != trigger.ID {
			t.Errorf("conversation trigger_id after Delete = %q, want %q", got, trigger.ID)
		}

		// A second delete finds no live row and is a no-op.
		if err := fx.Handlers.Delete(ctx, fx.OrgID, trigger.ID); err != nil {
			t.Errorf("second Delete: %v", err)
		}
	})
}
