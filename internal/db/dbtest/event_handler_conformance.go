package dbtest

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EventHandlerStoreFactory is what a per-backend test file hands to
// RunEventHandlerStoreConformance. The factory returns the wired
// store + the orgID + the teamID Seed should materialize into
// (SKY-295: shipped rules are team-scoped, no longer org-visible) +
// a seedPrompts hook the harness invokes before any test that creates
// trigger rows (triggers FK to prompts on (prompt_id, org_id); each
// backend wires its own prompt-seeding shape against its connection).
type EventHandlerStoreFactory func(t *testing.T) (store db.EventHandlerStore, orgID, teamID string, seedPrompts PromptSeeder)

// PromptSeeder inserts prompts into the harness DB at known IDs so
// trigger rows can reference them. Returns the inserted prompt IDs in
// stable order so the test can pick one when building a kind='trigger'
// fixture.
type PromptSeeder func(t *testing.T, ids ...string)

// RunEventHandlerStoreConformance runs the shared assertion suite for
// the unified rule + trigger store (SKY-259). What it covers:
//
//   - Seed inserts both rule and trigger ShippedEventHandlers rows;
//     re-seed is idempotent (per-row INSERT-OR-IGNORE / ON CONFLICT).
//   - Create rejects mis-shaped writes per kind (rule with prompt_id,
//     trigger missing prompt_id, etc.) — validateForCreate catches
//     before the CHECK constraint does.
//   - List with kind filter returns only matching rows; kind="" returns
//     both.
//   - GetEnabledForEvent returns enabled rows of both kinds ordered
//     rule-before-trigger.
//   - SetEnabled toggles; Delete hard-removes; Update changes mutable
//     fields per kind.
//   - Reorder updates sort_order on rules; silently skips trigger ids.
//   - Promote atomically flips a rule to a trigger, clearing rule
//     fields and populating trigger fields.
func RunEventHandlerStoreConformance(t *testing.T, factory EventHandlerStoreFactory) {
	t.Helper()

	t.Run("Seed_InsertsBothKinds", func(t *testing.T) {
		store, orgID, teamID, seedPrompts := factory(t)
		// Trigger rows in ShippedEventHandlers reference these prompts.
		seedPrompts(t,
			"system-pr-review",
			"system-conflict-resolution",
			"system-ci-fix",
			"system-jira-implement",
			"system-fix-review-feedback",
		)
		if err := store.Seed(context.Background(), orgID, teamID); err != nil {
			t.Fatalf("Seed: %v", err)
		}
		all, err := store.List(context.Background(), orgID, "")
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var rules, triggers int
		for _, h := range all {
			switch h.Kind {
			case domain.EventHandlerKindRule:
				rules++
			case domain.EventHandlerKindTrigger:
				triggers++
			}
		}
		if rules == 0 {
			t.Errorf("Seed produced 0 rule rows; want > 0")
		}
		if triggers == 0 {
			t.Errorf("Seed produced 0 trigger rows; want > 0")
		}
	})

	t.Run("Seed_IsIdempotent", func(t *testing.T) {
		store, orgID, teamID, seedPrompts := factory(t)
		seedPrompts(t,
			"system-pr-review", "system-conflict-resolution", "system-ci-fix",
			"system-jira-implement", "system-fix-review-feedback",
		)
		if err := store.Seed(context.Background(), orgID, teamID); err != nil {
			t.Fatalf("Seed #1: %v", err)
		}
		first, _ := store.List(context.Background(), orgID, "")
		if err := store.Seed(context.Background(), orgID, teamID); err != nil {
			t.Fatalf("Seed #2: %v", err)
		}
		second, _ := store.List(context.Background(), orgID, "")
		if len(first) != len(second) {
			t.Errorf("re-seed changed row count: first=%d second=%d", len(first), len(second))
		}
	})

	t.Run("Create_Rule_RoundTrip", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority := 0.75
		sortOrder := 3
		h := domain.EventHandler{
			ID:              uuid.New().String(),
			Kind:            domain.EventHandlerKindRule,
			EventType:       domain.EventGitHubPRCICheckFailed,
			Enabled:         true,
			Name:            "my-rule",
			DefaultPriority: &priority,
			SortOrder:       &sortOrder,
		}
		if err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create rule: %v", err)
		}
		got, err := store.Get(ctx, orgID, h.ID)
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.Kind != domain.EventHandlerKindRule {
			t.Errorf("Kind=%q want rule", got.Kind)
		}
		if got.Name != "my-rule" {
			t.Errorf("Name=%q want my-rule", got.Name)
		}
		if got.DefaultPriority == nil || *got.DefaultPriority != 0.75 {
			t.Errorf("DefaultPriority=%v want 0.75", got.DefaultPriority)
		}
		if got.PromptID != "" {
			t.Errorf("PromptID=%q; rule rows must have empty PromptID", got.PromptID)
		}
	})

	t.Run("Create_Trigger_RoundTrip", func(t *testing.T) {
		store, orgID, teamID, seedPrompts := factory(t)
		ctx := context.Background()
		seedPrompts(t, "p-trigger-test")
		breaker := 2
		minAutonomy := 0.5
		h := domain.EventHandler{
			ID:                     uuid.New().String(),
			Kind:                   domain.EventHandlerKindTrigger,
			PromptID:               "p-trigger-test",
			EventType:              domain.EventGitHubPRCICheckFailed,
			Enabled:                true,
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy,
		}
		if err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create trigger: %v", err)
		}
		got, err := store.Get(ctx, orgID, h.ID)
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if got.Kind != domain.EventHandlerKindTrigger {
			t.Errorf("Kind=%q want trigger", got.Kind)
		}
		if got.PromptID != "p-trigger-test" {
			t.Errorf("PromptID=%q", got.PromptID)
		}
		if got.BreakerThreshold == nil || *got.BreakerThreshold != 2 {
			t.Errorf("BreakerThreshold=%v want 2", got.BreakerThreshold)
		}
		if got.DefaultPriority != nil {
			t.Errorf("DefaultPriority=%v; trigger rows must have nil DefaultPriority", got.DefaultPriority)
		}
	})

	t.Run("Create_RejectsRuleWithTriggerFields", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority := 0.5
		sortOrder := 0
		breaker := 4
		h := domain.EventHandler{
			ID:               uuid.New().String(),
			Kind:             domain.EventHandlerKindRule,
			EventType:        domain.EventGitHubPRCICheckFailed,
			Name:             "bad-rule",
			DefaultPriority:  &priority,
			SortOrder:        &sortOrder,
			BreakerThreshold: &breaker, // illegal: trigger-only field on a rule
		}
		if err := store.Create(ctx, orgID, teamID, h); err == nil {
			t.Error("Create accepted a rule with trigger-only fields populated; want error")
		}
	})

	t.Run("Create_RejectsTriggerWithoutPromptID", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		breaker := 4
		minAutonomy := 0.0
		h := domain.EventHandler{
			ID:                     uuid.New().String(),
			Kind:                   domain.EventHandlerKindTrigger,
			EventType:              domain.EventGitHubPRCICheckFailed,
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy,
			// PromptID intentionally empty.
		}
		if err := store.Create(context.Background(), orgID, teamID, h); err == nil {
			t.Error("Create accepted a trigger with empty prompt_id; want error")
		}
	})

	t.Run("Create_RejectsTriggerWithName", func(t *testing.T) {
		// Defense-in-depth: the per-kind CHECK constraint forbids
		// kind='trigger' with a non-NULL name. validateForCreate
		// rejects the same shape earlier so the user gets a clearer
		// error than the SQL integrity-violation surface.
		store, orgID, teamID, seedPrompts := factory(t)
		seedPrompts(t, "p-name-on-trigger")
		breaker := 4
		minAutonomy := 0.0
		h := domain.EventHandler{
			ID:                     uuid.New().String(),
			Kind:                   domain.EventHandlerKindTrigger,
			PromptID:               "p-name-on-trigger",
			EventType:              domain.EventGitHubPRCICheckFailed,
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy,
			Name:                   "shouldn't be here", // illegal on a trigger
		}
		if err := store.Create(context.Background(), orgID, teamID, h); err == nil {
			t.Error("Create accepted a trigger with a non-empty name; want error")
		}
	})

	t.Run("List_KindFilter", func(t *testing.T) {
		store, orgID, teamID, seedPrompts := factory(t)
		ctx := context.Background()
		seedPrompts(t, "p-list-trigger")

		priority := 0.5
		sortOrder := 0
		breaker := 4
		minAutonomy := 0.0

		rule := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindRule,
			EventType: domain.EventGitHubPRCICheckFailed,
			Name:      "rule-1", DefaultPriority: &priority, SortOrder: &sortOrder, Enabled: true,
		}
		trig := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindTrigger,
			PromptID:               "p-list-trigger",
			EventType:              domain.EventGitHubPRCICheckFailed,
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy, Enabled: true,
		}
		if err := store.Create(ctx, orgID, teamID, rule); err != nil {
			t.Fatalf("Create rule: %v", err)
		}
		if err := store.Create(ctx, orgID, teamID, trig); err != nil {
			t.Fatalf("Create trig: %v", err)
		}

		rules, err := store.List(ctx, orgID, domain.EventHandlerKindRule)
		if err != nil {
			t.Fatalf("List(rule): %v", err)
		}
		for _, h := range rules {
			if h.Kind != domain.EventHandlerKindRule {
				t.Errorf("List(kind=rule) returned a %q row", h.Kind)
			}
		}
		triggers, err := store.List(ctx, orgID, domain.EventHandlerKindTrigger)
		if err != nil {
			t.Fatalf("List(trigger): %v", err)
		}
		for _, h := range triggers {
			if h.Kind != domain.EventHandlerKindTrigger {
				t.Errorf("List(kind=trigger) returned a %q row", h.Kind)
			}
		}
		all, _ := store.List(ctx, orgID, "")
		if len(all) < len(rules)+len(triggers) {
			t.Errorf("List(\"\") returned %d rows; expected at least %d", len(all), len(rules)+len(triggers))
		}
	})

	t.Run("GetEnabledForEvent_OrdersRulesBeforeTriggers", func(t *testing.T) {
		store, orgID, teamID, seedPrompts := factory(t)
		ctx := context.Background()
		seedPrompts(t, "p-order-test")

		priority := 0.5
		sortOrder := 0
		breaker := 4
		minAutonomy := 0.0
		eventType := domain.EventGitHubPRCICheckFailed

		rule := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindRule,
			EventType: eventType,
			Name:      "r", DefaultPriority: &priority, SortOrder: &sortOrder, Enabled: true,
		}
		trig := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindTrigger,
			PromptID:               "p-order-test",
			EventType:              eventType,
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy, Enabled: true,
		}
		_ = store.Create(ctx, orgID, teamID, trig) // trigger first to prove ordering isn't insert-order
		_ = store.Create(ctx, orgID, teamID, rule)

		got, err := store.GetEnabledForEvent(ctx, orgID, eventType)
		if err != nil {
			t.Fatalf("GetEnabledForEvent: %v", err)
		}
		if len(got) < 2 {
			t.Fatalf("got %d rows; want >= 2", len(got))
		}
		// First rule index must come before first trigger index.
		var firstRule, firstTrigger = -1, -1
		for i, h := range got {
			if h.Kind == domain.EventHandlerKindRule && firstRule == -1 {
				firstRule = i
			}
			if h.Kind == domain.EventHandlerKindTrigger && firstTrigger == -1 {
				firstTrigger = i
			}
		}
		if firstRule == -1 || firstTrigger == -1 {
			t.Fatalf("missing kind in result: firstRule=%d firstTrigger=%d", firstRule, firstTrigger)
		}
		if firstRule >= firstTrigger {
			t.Errorf("rules must come before triggers; firstRule=%d firstTrigger=%d", firstRule, firstTrigger)
		}
	})

	t.Run("SetEnabled_Toggles", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority, sortOrder := 0.5, 0
		h := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindRule,
			EventType: domain.EventGitHubPRCICheckFailed,
			Name:      "toggle-me", DefaultPriority: &priority, SortOrder: &sortOrder, Enabled: true,
		}
		if err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.SetEnabled(ctx, orgID, h.ID, false); err != nil {
			t.Fatalf("SetEnabled false: %v", err)
		}
		got, _ := store.Get(ctx, orgID, h.ID)
		if got.Enabled {
			t.Error("SetEnabled(false) did not disable")
		}
	})

	t.Run("Delete_RemovesRow", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority, sortOrder := 0.5, 0
		h := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindRule,
			EventType: domain.EventGitHubPRCICheckFailed,
			Name:      "delete-me", DefaultPriority: &priority, SortOrder: &sortOrder, Enabled: true,
		}
		if err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Delete(ctx, orgID, h.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		got, _ := store.Get(ctx, orgID, h.ID)
		if got != nil {
			t.Errorf("Get after Delete returned %+v; want nil", got)
		}
	})

	t.Run("Promote_RuleToTrigger", func(t *testing.T) {
		store, orgID, teamID, seedPrompts := factory(t)
		ctx := context.Background()
		seedPrompts(t, "p-promote-target")

		priority, sortOrder := 0.5, 0
		ruleID := uuid.New().String()
		if err := store.Create(ctx, orgID, teamID, domain.EventHandler{
			ID: ruleID, Kind: domain.EventHandlerKindRule,
			EventType: domain.EventGitHubPRCICheckFailed,
			Name:      "promote-me", DefaultPriority: &priority, SortOrder: &sortOrder, Enabled: true,
		}); err != nil {
			t.Fatalf("Create rule: %v", err)
		}

		breaker := 3
		minAutonomy := 0.0
		err := store.Promote(ctx, orgID, ruleID, domain.EventHandler{
			Kind:                   domain.EventHandlerKindTrigger,
			PromptID:               "p-promote-target",
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy,
		})
		if err != nil {
			t.Fatalf("Promote: %v", err)
		}
		got, _ := store.Get(ctx, orgID, ruleID)
		if got == nil || got.Kind != domain.EventHandlerKindTrigger {
			t.Fatalf("Promote did not flip kind: got=%v", got)
		}
		if got.PromptID != "p-promote-target" {
			t.Errorf("PromptID=%q after promote", got.PromptID)
		}
		if got.Name != "" {
			t.Errorf("Name=%q after promote; rule-only field must be cleared", got.Name)
		}
		if got.DefaultPriority != nil {
			t.Errorf("DefaultPriority=%v after promote; want nil", got.DefaultPriority)
		}
	})

	t.Run("Promote_RejectsTriggerSource", func(t *testing.T) {
		store, orgID, teamID, seedPrompts := factory(t)
		ctx := context.Background()
		seedPrompts(t, "p-already-trigger")

		breaker := 4
		minAutonomy := 0.0
		trigID := uuid.New().String()
		if err := store.Create(ctx, orgID, teamID, domain.EventHandler{
			ID:                     trigID,
			Kind:                   domain.EventHandlerKindTrigger,
			PromptID:               "p-already-trigger",
			EventType:              domain.EventGitHubPRCICheckFailed,
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy, Enabled: true,
		}); err != nil {
			t.Fatalf("Create trigger: %v", err)
		}
		err := store.Promote(ctx, orgID, trigID, domain.EventHandler{
			Kind:             domain.EventHandlerKindTrigger,
			PromptID:         "p-already-trigger",
			BreakerThreshold: &breaker, MinAutonomySuitability: &minAutonomy,
		})
		if err == nil {
			t.Error("Promote of a trigger row succeeded; want error")
		}
	})
}
