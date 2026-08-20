package dbtest

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EventHandlerStoreFactory is what a per-backend test file hands to
// RunEventHandlerStoreConformance. The factory returns the wired
// store + the orgID + the teamID Seed/Create should materialize into
// (shipped handlers are team-scoped) + a seedBlueprints hook the harness
// invokes before any test that creates trigger rows. Triggers FK to
// blueprints on both (blueprint_id, org_id) AND the same-team
// (blueprint_id, team_id), so the seeded blueprints must live on the
// factory's teamID; each backend wires its own seeding shape.
type EventHandlerStoreFactory func(t *testing.T) (store db.EventHandlerStore, orgID, teamID string, seedBlueprints BlueprintSeeder)

// BlueprintSeeder seeds blueprints into the harness DB at the given slugs so
// trigger rows can reference them, and returns a slug→blueprint-id map. The
// id is a random UUID per copy; callers resolve a trigger's blueprint slug to
// the seeded id through this map (mirroring the seed order: prompts →
// blueprints → EventHandlerStore.Seed).
type BlueprintSeeder func(t *testing.T, slugs ...string) map[string]string

// RunEventHandlerStoreConformance runs the shared assertion suite for
// the unified rule + trigger store. What it covers:
//
//   - Seed inserts both rule and trigger ShippedEventHandlers rows,
//     resolving each trigger's blueprint slug through the seeded map;
//     re-seed is idempotent (ON CONFLICT on (org_id, team_id, system_slug)).
//   - Create rejects mis-shaped writes per kind (rule with blueprint_id,
//     trigger missing blueprint_id, etc.) — ValidateEventHandlerForCreate catches
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
		store, orgID, teamID, seedBlueprints := factory(t)
		// Trigger rows in ShippedEventHandlers reference these blueprints.
		ids := seedBlueprints(t,
			"system-pr-review",
			"system-conflict-resolution",
			"system-ci-fix",
			"system-jira-implement",
			"system-fix-review-feedback",
		)
		if err := store.Seed(context.Background(), orgID, teamID, ids); err != nil {
			t.Fatalf("Seed: %v", err)
		}
		all, _, err := store.List(context.Background(), orgID, db.EventHandlerListFilter{}, db.ListOpts{Limit: 200})
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
		store, orgID, teamID, seedBlueprints := factory(t)
		ids := seedBlueprints(t,
			"system-pr-review", "system-conflict-resolution", "system-ci-fix",
			"system-jira-implement", "system-fix-review-feedback",
		)
		if err := store.Seed(context.Background(), orgID, teamID, ids); err != nil {
			t.Fatalf("Seed #1: %v", err)
		}
		first, _, _ := store.List(context.Background(), orgID, db.EventHandlerListFilter{}, db.ListOpts{Limit: 200})
		if err := store.Seed(context.Background(), orgID, teamID, ids); err != nil {
			t.Fatalf("Seed #2: %v", err)
		}
		second, _, _ := store.List(context.Background(), orgID, db.EventHandlerListFilter{}, db.ListOpts{Limit: 200})
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
		if got.BlueprintID != "" {
			t.Errorf("BlueprintID=%q; rule rows must have empty BlueprintID", got.BlueprintID)
		}
	})

	t.Run("AppliesToUnowned_RoundTripAndUpdate", func(t *testing.T) {
		// The TFAC-517 routing-scope flag round-trips through Create + Get and
		// flips through Update, on both backends. Default-false is asserted by
		// the Create_Rule_RoundTrip row above (it never sets the field), so here
		// we prove the true value persists and that Update can toggle it back.
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority := 0.5
		sortOrder := 0
		h := domain.EventHandler{
			ID:               uuid.New().String(),
			Kind:             domain.EventHandlerKindRule,
			EventType:        domain.EventGitHubPRCICheckFailed,
			Enabled:          true,
			Name:             "watch-rule",
			DefaultPriority:  &priority,
			SortOrder:        &sortOrder,
			AppliesToUnowned: true,
		}
		if err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create rule: %v", err)
		}
		got, err := store.Get(ctx, orgID, h.ID)
		if err != nil || got == nil {
			t.Fatalf("Get: got=%v err=%v", got, err)
		}
		if !got.AppliesToUnowned {
			t.Errorf("AppliesToUnowned=%v after Create(true), want true", got.AppliesToUnowned)
		}

		// Update flips it off; the round-trip must reflect the new value.
		got.AppliesToUnowned = false
		if err := store.Update(ctx, orgID, *got); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got2, err := store.Get(ctx, orgID, h.ID)
		if err != nil || got2 == nil {
			t.Fatalf("Get after Update: got=%v err=%v", got2, err)
		}
		if got2.AppliesToUnowned {
			t.Errorf("AppliesToUnowned=%v after Update(false), want false", got2.AppliesToUnowned)
		}
	})

	t.Run("Create_RuleDefaultsAppliesToUnownedFalse", func(t *testing.T) {
		// A rule created without setting the flag must read back false — the
		// "blanket reset for everyone" IS the column default (TFAC-517).
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority := 0.5
		sortOrder := 0
		h := domain.EventHandler{
			ID:              uuid.New().String(),
			Kind:            domain.EventHandlerKindRule,
			EventType:       domain.EventGitHubPRCICheckFailed,
			Enabled:         true,
			Name:            "default-rule",
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
		if got.AppliesToUnowned {
			t.Errorf("AppliesToUnowned=%v on a rule that never set it, want false (default)", got.AppliesToUnowned)
		}
	})

	t.Run("Create_Trigger_RoundTrip", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-trigger-test")
		blueprintID := ids["p-trigger-test"]
		breaker := 2
		minAutonomy := 0.5
		h := domain.EventHandler{
			ID:                     uuid.New().String(),
			Kind:                   domain.EventHandlerKindTrigger,
			BlueprintID:            blueprintID,
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
		if got.BlueprintID != blueprintID {
			t.Errorf("BlueprintID=%q want %q", got.BlueprintID, blueprintID)
		}
		if got.BreakerThreshold == nil || *got.BreakerThreshold != 2 {
			t.Errorf("BreakerThreshold=%v want 2", got.BreakerThreshold)
		}
		if got.DefaultPriority != nil {
			t.Errorf("DefaultPriority=%v; trigger rows must have nil DefaultPriority", got.DefaultPriority)
		}
	})

	t.Run("RetargetBlueprint_MovesTriggerPreservingRow", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-retarget-from", "p-retarget-to")
		from, to := ids["p-retarget-from"], ids["p-retarget-to"]
		breaker := 3
		minAutonomy := 0.0
		h := domain.EventHandler{
			ID:                     uuid.New().String(),
			Kind:                   domain.EventHandlerKindTrigger,
			BlueprintID:            from,
			EventType:              domain.EventGitHubPRCICheckFailed,
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy,
		}
		if err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create trigger: %v", err)
		}
		if err := store.RetargetBlueprint(ctx, orgID, h.ID, to); err != nil {
			t.Fatalf("RetargetBlueprint: %v", err)
		}
		got, err := store.Get(ctx, orgID, h.ID)
		if err != nil || got == nil {
			t.Fatalf("Get after retarget: got=%v err=%v", got, err)
		}
		// Same row (id preserved), now pointing at the new blueprint, still a trigger.
		if got.BlueprintID != to {
			t.Errorf("BlueprintID=%q want %q (retarget must move it)", got.BlueprintID, to)
		}
		if got.Kind != domain.EventHandlerKindTrigger {
			t.Errorf("Kind=%q want trigger (retarget must not change kind)", got.Kind)
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

	t.Run("Create_RejectsTriggerWithoutBlueprintID", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		breaker := 4
		minAutonomy := 0.0
		h := domain.EventHandler{
			ID:                     uuid.New().String(),
			Kind:                   domain.EventHandlerKindTrigger,
			EventType:              domain.EventGitHubPRCICheckFailed,
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy,
			// BlueprintID intentionally empty.
		}
		if err := store.Create(context.Background(), orgID, teamID, h); err == nil {
			t.Error("Create accepted a trigger with empty blueprint_id; want error")
		}
	})

	t.Run("Create_RejectsTriggerWithName", func(t *testing.T) {
		// Defense-in-depth: the per-kind CHECK constraint forbids
		// kind='trigger' with a non-NULL name. ValidateEventHandlerForCreate
		// rejects the same shape earlier so the user gets a clearer
		// error than the SQL integrity-violation surface.
		store, orgID, teamID, seedBlueprints := factory(t)
		ids := seedBlueprints(t, "p-name-on-trigger")
		breaker := 4
		minAutonomy := 0.0
		h := domain.EventHandler{
			ID:                     uuid.New().String(),
			Kind:                   domain.EventHandlerKindTrigger,
			BlueprintID:            ids["p-name-on-trigger"],
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
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-list-trigger")

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
			BlueprintID:            ids["p-list-trigger"],
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

		rules, _, err := store.List(ctx, orgID, db.EventHandlerListFilter{Kind: domain.EventHandlerKindRule}, db.ListOpts{Limit: 200})
		if err != nil {
			t.Fatalf("List(rule): %v", err)
		}
		for _, h := range rules {
			if h.Kind != domain.EventHandlerKindRule {
				t.Errorf("List(kind=rule) returned a %q row", h.Kind)
			}
		}
		triggers, _, err := store.List(ctx, orgID, db.EventHandlerListFilter{Kind: domain.EventHandlerKindTrigger}, db.ListOpts{Limit: 200})
		if err != nil {
			t.Fatalf("List(trigger): %v", err)
		}
		for _, h := range triggers {
			if h.Kind != domain.EventHandlerKindTrigger {
				t.Errorf("List(kind=trigger) returned a %q row", h.Kind)
			}
		}
		all, allTotal, _ := store.List(ctx, orgID, db.EventHandlerListFilter{}, db.ListOpts{Limit: 200})
		if allTotal != len(all) {
			t.Errorf("total = %d but the page held %d rows", allTotal, len(all))
		}
		if len(all) < len(rules)+len(triggers) {
			t.Errorf("List(\"\") returned %d rows; expected at least %d", len(all), len(rules)+len(triggers))
		}
	})

	t.Run("GetEnabledForEvent_OrdersRulesBeforeTriggers", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-order-test")

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
			BlueprintID:            ids["p-order-test"],
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
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-promote-target")

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
		promoteTarget := ids["p-promote-target"]
		err := store.Promote(ctx, orgID, ruleID, domain.EventHandler{
			Kind:                   domain.EventHandlerKindTrigger,
			BlueprintID:            promoteTarget,
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
		if got.BlueprintID != promoteTarget {
			t.Errorf("BlueprintID=%q after promote", got.BlueprintID)
		}
		if got.Name != "" {
			t.Errorf("Name=%q after promote; rule-only field must be cleared", got.Name)
		}
		if got.DefaultPriority != nil {
			t.Errorf("DefaultPriority=%v after promote; want nil", got.DefaultPriority)
		}
	})

	t.Run("Promote_RejectsTriggerSource", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-already-trigger")
		blueprintID := ids["p-already-trigger"]

		breaker := 4
		minAutonomy := 0.0
		trigID := uuid.New().String()
		if err := store.Create(ctx, orgID, teamID, domain.EventHandler{
			ID:                     trigID,
			Kind:                   domain.EventHandlerKindTrigger,
			BlueprintID:            blueprintID,
			EventType:              domain.EventGitHubPRCICheckFailed,
			BreakerThreshold:       &breaker,
			MinAutonomySuitability: &minAutonomy, Enabled: true,
		}); err != nil {
			t.Fatalf("Create trigger: %v", err)
		}
		err := store.Promote(ctx, orgID, trigID, domain.EventHandler{
			Kind:             domain.EventHandlerKindTrigger,
			BlueprintID:      blueprintID,
			BreakerThreshold: &breaker, MinAutonomySuitability: &minAutonomy,
		})
		if err == nil {
			t.Error("Promote of a trigger row succeeded; want error")
		}
	})

	t.Run("SetEnabled_DoesNotStampUserModified", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority, sortOrder := 0.5, 0
		h := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindRule,
			EventType: domain.EventGitHubPRCICheckFailed,
			Name:      "toggle-no-stamp", DefaultPriority: &priority, SortOrder: &sortOrder, Enabled: true,
		}
		if err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.SetEnabled(ctx, orgID, h.ID, false); err != nil {
			t.Fatalf("SetEnabled: %v", err)
		}
		got, _ := store.Get(ctx, orgID, h.ID)
		if got == nil || got.UserModified {
			t.Errorf("UserModified=%v after SetEnabled; want false (activation state is not content)", got != nil && got.UserModified)
		}
	})

	t.Run("Reorder_AppliesPositions", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority := 0.5
		ids := make([]string, 3)
		for i := range ids {
			sortOrder := i
			h := domain.EventHandler{
				ID: uuid.New().String(), Kind: domain.EventHandlerKindRule,
				EventType: domain.EventGitHubPRCICheckFailed,
				Name:      "position-" + strconv.Itoa(i), DefaultPriority: &priority,
				SortOrder: &sortOrder, Enabled: true,
			}
			if err := store.Create(ctx, orgID, teamID, h); err != nil {
				t.Fatalf("Create %d: %v", i, err)
			}
			ids[i] = h.ID
		}

		// Reverse the list: each id's new sort_order is its index in what was
		// submitted, so the whole order moves, not just the pair that swapped.
		reversed := []string{ids[2], ids[1], ids[0]}
		if err := store.Reorder(ctx, orgID, reversed); err != nil {
			t.Fatalf("Reorder: %v", err)
		}
		for want, id := range reversed {
			got, err := store.Get(ctx, orgID, id)
			if err != nil {
				t.Fatalf("Get %s: %v", id, err)
			}
			if got == nil || got.SortOrder == nil || *got.SortOrder != want {
				t.Errorf("id=%s sort_order=%v, want %d", id, got.SortOrder, want)
			}
		}
	})

	t.Run("Reorder_DoesNotStampUserModified", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority := 0.5
		s0, s1 := 0, 1
		h1 := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindRule,
			EventType: domain.EventGitHubPRCICheckFailed,
			Name:      "reorder-a", DefaultPriority: &priority, SortOrder: &s0, Enabled: true,
		}
		h2 := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindRule,
			EventType: domain.EventGitHubPRCICheckFailed,
			Name:      "reorder-b", DefaultPriority: &priority, SortOrder: &s1, Enabled: true,
		}
		if err := store.Create(ctx, orgID, teamID, h1); err != nil {
			t.Fatalf("Create h1: %v", err)
		}
		if err := store.Create(ctx, orgID, teamID, h2); err != nil {
			t.Fatalf("Create h2: %v", err)
		}
		if err := store.Reorder(ctx, orgID, []string{h2.ID, h1.ID}); err != nil {
			t.Fatalf("Reorder: %v", err)
		}
		for _, id := range []string{h1.ID, h2.ID} {
			got, _ := store.Get(ctx, orgID, id)
			if got == nil || got.UserModified {
				t.Errorf("id=%s UserModified=%v after Reorder; want false (sort_order is presentation order the user owns)", id, got != nil && got.UserModified)
			}
		}
	})

	t.Run("Update_StampsUserModifiedOnlyWhenContentChanges", func(t *testing.T) {
		store, orgID, teamID, _ := factory(t)
		ctx := context.Background()
		priority, sortOrder := 0.5, 0
		h := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindRule,
			EventType: domain.EventGitHubPRCICheckFailed,
			Name:      "update-stamp", DefaultPriority: &priority, SortOrder: &sortOrder, Enabled: true,
		}
		if err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create: %v", err)
		}

		// Update carrying enabled alongside unchanged content: no stamp.
		got, _ := store.Get(ctx, orgID, h.ID)
		got.Enabled = false
		if err := store.Update(ctx, orgID, *got); err != nil {
			t.Fatalf("Update (enabled-only): %v", err)
		}
		afterEnabledOnly, _ := store.Get(ctx, orgID, h.ID)
		if afterEnabledOnly == nil || afterEnabledOnly.UserModified {
			t.Fatalf("UserModified=%v after an enabled-only Update; want false", afterEnabledOnly != nil && afterEnabledOnly.UserModified)
		}

		// Update changing a content field: stamps.
		afterEnabledOnly.Name = "update-stamp-renamed"
		if err := store.Update(ctx, orgID, *afterEnabledOnly); err != nil {
			t.Fatalf("Update (content change): %v", err)
		}
		afterContent, _ := store.Get(ctx, orgID, h.ID)
		if afterContent == nil || !afterContent.UserModified {
			t.Errorf("UserModified=%v after a content-changing Update; want true", afterContent != nil && afterContent.UserModified)
		}
	})

	t.Run("Promote_StampsUserModified", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-promote-stamp")
		priority, sortOrder := 0.5, 0
		ruleID := uuid.New().String()
		if err := store.Create(ctx, orgID, teamID, domain.EventHandler{
			ID: ruleID, Kind: domain.EventHandlerKindRule,
			EventType: domain.EventGitHubPRCICheckFailed,
			Name:      "promote-stamp", DefaultPriority: &priority, SortOrder: &sortOrder, Enabled: true,
		}); err != nil {
			t.Fatalf("Create rule: %v", err)
		}
		breaker := 3
		minAutonomy := 0.0
		if err := store.Promote(ctx, orgID, ruleID, domain.EventHandler{
			Kind: domain.EventHandlerKindTrigger, BlueprintID: ids["p-promote-stamp"],
			BreakerThreshold: &breaker, MinAutonomySuitability: &minAutonomy,
		}); err != nil {
			t.Fatalf("Promote: %v", err)
		}
		got, _ := store.Get(ctx, orgID, ruleID)
		if got == nil || !got.UserModified {
			t.Errorf("UserModified=%v after Promote; want true (a kind change is never sync-revertible)", got != nil && got.UserModified)
		}
	})

	t.Run("RetargetBlueprint_StampsUserModified", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-retarget-stamp-from", "p-retarget-stamp-to")
		breaker := 3
		minAutonomy := 0.0
		h := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindTrigger,
			BlueprintID: ids["p-retarget-stamp-from"], EventType: domain.EventGitHubPRCICheckFailed,
			BreakerThreshold: &breaker, MinAutonomySuitability: &minAutonomy,
		}
		if err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create trigger: %v", err)
		}
		if err := store.RetargetBlueprint(ctx, orgID, h.ID, ids["p-retarget-stamp-to"]); err != nil {
			t.Fatalf("RetargetBlueprint: %v", err)
		}
		got, _ := store.Get(ctx, orgID, h.ID)
		if got == nil || !got.UserModified {
			t.Errorf("UserModified=%v after RetargetBlueprint; want true (a user-initiated retarget must survive sync)", got != nil && got.UserModified)
		}
	})

	t.Run("Delete_SoftDeletesSystemRow_SlugStaysOccupiedAndInvisible", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t,
			"system-pr-review", "system-conflict-resolution", "system-ci-fix",
			"system-jira-implement", "system-fix-review-feedback",
		)
		if err := store.Seed(ctx, orgID, teamID, ids); err != nil {
			t.Fatalf("Seed: %v", err)
		}
		const slug = "system-rule-ci-check-failed"
		before, err := store.GetBySystemSlug(ctx, orgID, teamID, slug)
		if err != nil || before == nil {
			t.Fatalf("GetBySystemSlug before delete: (%v, %v)", before, err)
		}
		if err := store.Delete(ctx, orgID, before.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}

		if got, err := store.Get(ctx, orgID, before.ID); err != nil || got != nil {
			t.Errorf("Get after soft-delete = (%v, %v); want (nil, nil)", got, err)
		}
		if got, err := store.GetSystem(ctx, orgID, before.ID); err != nil || got != nil {
			t.Errorf("GetSystem after soft-delete = (%v, %v); want (nil, nil) — a soft-deleted trigger/rule must never resolve as live", got, err)
		}
		if got, err := store.GetBySystemSlug(ctx, orgID, teamID, slug); err != nil || got != nil {
			t.Errorf("GetBySystemSlug after soft-delete = (%v, %v); want (nil, nil)", got, err)
		}
		all, _, err := store.List(ctx, orgID, db.EventHandlerListFilter{}, db.ListOpts{Limit: 200})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, h := range all {
			if h.ID == before.ID {
				t.Errorf("soft-deleted handler %s leaked into List", h.ID)
			}
		}
		enabled, err := store.GetEnabledForEvent(ctx, orgID, domain.EventGitHubPRCICheckFailed)
		if err != nil {
			t.Fatalf("GetEnabledForEvent: %v", err)
		}
		for _, h := range enabled {
			if h.ID == before.ID {
				t.Errorf("soft-deleted handler %s leaked into GetEnabledForEvent — it would still fire", h.ID)
			}
		}

		// Never resurrect: re-seeding must not re-insert a row for the
		// slug's occupied slot.
		if err := store.Seed(ctx, orgID, teamID, ids); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
		if got, err := store.GetBySystemSlug(ctx, orgID, teamID, slug); err != nil || got != nil {
			t.Errorf("GetBySystemSlug after re-seed = (%v, %v); want (nil, nil) — re-seed must not resurrect a soft-deleted shipped row", got, err)
		}
	})

	t.Run("Delete_SoftDeletedSystemTrigger_FreesBlueprintSlotForANewTrigger", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t,
			"system-pr-review", "system-conflict-resolution", "system-ci-fix",
			"system-jira-implement", "system-fix-review-feedback",
		)
		if err := store.Seed(ctx, orgID, teamID, ids); err != nil {
			t.Fatalf("Seed: %v", err)
		}
		blueprintID := ids["system-ci-fix"]
		shipped, err := store.GetBySystemSlug(ctx, orgID, teamID, "system-trigger-ci-fix")
		if err != nil || shipped == nil {
			t.Fatalf("GetBySystemSlug(system-trigger-ci-fix) before delete: (%v, %v)", shipped, err)
		}

		// Deleting the shipped (system_slug) trigger soft-deletes it — verify
		// the dedup index doesn't keep the blueprint permanently claimed by the
		// now-invisible row.
		if err := store.Delete(ctx, orgID, shipped.ID); err != nil {
			t.Fatalf("Delete shipped trigger: %v", err)
		}
		existing, err := store.ListForBlueprint(ctx, orgID, blueprintID)
		if err != nil {
			t.Fatalf("ListForBlueprint: %v", err)
		}
		if len(existing) != 0 {
			t.Fatalf("ListForBlueprint after soft-delete = %d rows; want 0 (a spurious \"already triggered\" 409 otherwise)", len(existing))
		}
		breaker := 3
		minAutonomy := 0.0
		replacement := domain.EventHandler{
			ID: uuid.New().String(), Kind: domain.EventHandlerKindTrigger,
			BlueprintID: blueprintID, EventType: domain.EventGitHubPRCICheckFailed,
			BreakerThreshold: &breaker, MinAutonomySuitability: &minAutonomy,
		}
		if err := store.Create(ctx, orgID, teamID, replacement); err != nil {
			t.Fatalf("Create replacement trigger on the freed blueprint: %v", err)
		}
	})
}
