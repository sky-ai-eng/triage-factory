package dbtest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// syncRule builds a shipped rule fixture for EventHandlerStore.Sync tests.
func syncRule(slug, eventType, predicate, name string, priority float64, sortOrder int) db.ShippedEventHandler {
	return db.ShippedEventHandler{
		ID: slug, Kind: domain.EventHandlerKindRule, EventType: eventType, Predicate: predicate,
		Name: name, DefaultPriority: priority, SortOrder: sortOrder,
	}
}

// syncTrigger builds a shipped trigger fixture, blueprintSlug being the
// shipped blueprint's system_slug (resolved through the test's own
// blueprintIDsBySlug map, exactly like production SyncShippedIntoTeam does).
func syncTrigger(slug, eventType, predicate, blueprintSlug string, breaker int, minAutonomy float64) db.ShippedEventHandler {
	return db.ShippedEventHandler{
		ID: slug, Kind: domain.EventHandlerKindTrigger, EventType: eventType, Predicate: predicate,
		BlueprintID: blueprintSlug, BreakerThreshold: breaker, MinAutonomySuitability: minAutonomy,
	}
}

// RunShippedHandlerSyncConformance exercises the event-handler half of the
// boot-time shipped-defaults sync — EventHandlerStore.Sync directly (with
// fixture shipped lists, so cases don't depend on the real
// db.ShippedEventHandlers content) plus one end-to-end pass through
// ShippedDefaultsStore.SyncShippedIntoTeam (which always uses the real list)
// to prove the production wiring — blueprint pass, then handler pass,
// resolving trigger bindings against post-blueprint-sync state.
func RunShippedHandlerSyncConformance(t *testing.T, factory ShippedSyncFactory) {
	t.Helper()
	ctx := context.Background()

	handlerBySlug := func(t *testing.T, stores db.Stores, orgID, teamID, slug string) *domain.EventHandler {
		t.Helper()
		h, err := stores.EventHandlers.GetBySystemSlug(ctx, orgID, teamID, slug)
		if err != nil {
			t.Fatalf("GetBySystemSlug(handler %q): %v", slug, err)
		}
		return h
	}
	blueprintBySlug := func(t *testing.T, stores db.Stores, orgID, teamID, slug string) *domain.Blueprint {
		t.Helper()
		b, err := stores.Blueprints.GetBySystemSlug(ctx, orgID, teamID, slug)
		if err != nil {
			t.Fatalf("GetBySystemSlug(blueprint %q): %v", slug, err)
		}
		return b
	}

	// seedOneStepBlueprint syncs a single trigger-target blueprint (via the
	// real blueprint sync path) and returns its team-copy id, for tests that
	// only need a valid FK target and don't care about blueprint content.
	seedOneStepBlueprint := func(t *testing.T, stores db.Stores, orgID, teamID, slug string) string {
		t.Helper()
		prompts := []domain.Prompt{syncPrompt(slug+"-p", slug, "body", "", "")}
		bps := []domain.SeedBlueprint{syncBlueprint(slug, slug, slug+"-p")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, prompts, bps); err != nil {
			t.Fatalf("seed blueprint %q: %v", slug, err)
		}
		b := blueprintBySlug(t, stores, orgID, teamID, slug)
		if b == nil {
			t.Fatalf("blueprint %q not found after seed", slug)
		}
		return b.ID
	}

	t.Run("InsertMissing_HonorsEnabledConventions", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		bpID := seedOneStepBlueprint(t, stores, orgID, teamID, "hbp-conv")
		handlers := []db.ShippedEventHandler{
			syncRule("hr-conv", domain.EventGitHubPRCICheckFailed, "", "HRule", 0.5, 0),
			syncTrigger("ht-conv", domain.EventGitHubPRCICheckFailed, "", "hbp-conv", 3, 0.0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlers, map[string]string{"hbp-conv": bpID}); err != nil {
			t.Fatalf("sync: %v", err)
		}
		rule := handlerBySlug(t, stores, orgID, teamID, "hr-conv")
		if rule == nil || !rule.Enabled {
			t.Errorf("inserted rule = %+v; want a row with enabled=true", rule)
		}
		trig := handlerBySlug(t, stores, orgID, teamID, "ht-conv")
		if trig == nil || trig.Enabled {
			t.Errorf("inserted trigger = %+v; want a row with enabled=false", trig)
		}
	})

	t.Run("EqualContent_ZeroWrites", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		handlers := []db.ShippedEventHandler{
			syncRule("hr-eq", domain.EventGitHubPRCICheckFailed, `{"author_in":[]}`, "HEq", 0.5, 0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlers, nil); err != nil {
			t.Fatalf("sync: %v", err)
		}
		before := handlerBySlug(t, stores, orgID, teamID, "hr-eq").UpdatedAt
		time.Sleep(15 * time.Millisecond)
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlers, nil); err != nil {
			t.Fatalf("re-sync: %v", err)
		}
		after := handlerBySlug(t, stores, orgID, teamID, "hr-eq").UpdatedAt
		if !after.Equal(before) {
			t.Errorf("updated_at churned on an equal re-sync: before=%s after=%s", before, after)
		}
	})

	t.Run("StaleContent_UpdatedWithoutTouchingEnabled_WhenDisabled", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		handlersA := []db.ShippedEventHandler{
			syncRule("hr-stale-off", domain.EventGitHubPRCICheckFailed, `{"author_in":[]}`, "HStale", 0.5, 0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, nil); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		r := handlerBySlug(t, stores, orgID, teamID, "hr-stale-off")
		if err := stores.EventHandlers.SetEnabled(ctx, orgID, r.ID, false); err != nil {
			t.Fatalf("SetEnabled: %v", err)
		}

		handlersB := []db.ShippedEventHandler{
			syncRule("hr-stale-off", domain.EventGitHubPRCICheckFailed, `{"author_in":["someone"]}`, "HStale v2", 0.75, 0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, nil); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		got := handlerBySlug(t, stores, orgID, teamID, "hr-stale-off")
		if got == nil {
			t.Fatal("rule vanished after sync")
		}
		if got.Enabled {
			t.Errorf("Enabled=%v after content sync; want false (sync must never write enabled)", got.Enabled)
		}
		if got.Name != "HStale v2" || got.DefaultPriority == nil || *got.DefaultPriority != 0.75 {
			dp := "nil"
			if got.DefaultPriority != nil {
				dp = fmt.Sprintf("%v", *got.DefaultPriority)
			}
			t.Errorf("stale content not restored: name=%q default_priority=%s", got.Name, dp)
		}
		if got.ScopePredicateJSON == nil || !db.PredicateJSONEqual(*got.ScopePredicateJSON, `{"author_in":["someone"]}`) {
			pred := "nil"
			if got.ScopePredicateJSON != nil {
				pred = *got.ScopePredicateJSON
			}
			t.Errorf("stale predicate not restored: %s", pred)
		}
	})

	t.Run("StaleContent_UpdatedWithoutTouchingEnabled_WhenEnabled", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		handlersA := []db.ShippedEventHandler{
			syncRule("hr-stale-on", domain.EventGitHubPRCICheckFailed, "", "HStaleOn", 0.5, 0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, nil); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		handlersB := []db.ShippedEventHandler{
			syncRule("hr-stale-on", domain.EventGitHubPRCICheckFailed, "", "HStaleOn v2", 0.75, 0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, nil); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		got := handlerBySlug(t, stores, orgID, teamID, "hr-stale-on")
		if got == nil || !got.Enabled {
			t.Errorf("Enabled=%v after content sync; want true (shipped-insert convention untouched)", got != nil && got.Enabled)
		}
		if got.Name != "HStaleOn v2" {
			t.Errorf("stale content not restored: %+v", got)
		}
	})

	t.Run("SoftDeletedHandler_SkippedBySync", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		handlersA := []db.ShippedEventHandler{
			syncRule("hr-del", domain.EventGitHubPRCICheckFailed, "", "HDel", 0.5, 0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, nil); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		r := handlerBySlug(t, stores, orgID, teamID, "hr-del")
		if err := stores.EventHandlers.Delete(ctx, orgID, r.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		handlersB := []db.ShippedEventHandler{
			syncRule("hr-del", domain.EventGitHubPRCICheckFailed, "", "HDel v2", 0.75, 0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, nil); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		if got := handlerBySlug(t, stores, orgID, teamID, "hr-del"); got != nil {
			t.Errorf("soft-deleted handler resurrected by sync: %+v", got)
		}
	})

	t.Run("UserModifiedHandler_SkippedBySync", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		handlersA := []db.ShippedEventHandler{
			syncRule("hr-mod", domain.EventGitHubPRCICheckFailed, "", "HMod", 0.5, 0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, nil); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		r := handlerBySlug(t, stores, orgID, teamID, "hr-mod")
		edited := *r
		edited.Name = "User Renamed"
		if err := stores.EventHandlers.Update(ctx, orgID, edited); err != nil {
			t.Fatalf("user update: %v", err)
		}
		handlersB := []db.ShippedEventHandler{
			syncRule("hr-mod", domain.EventGitHubPRCICheckFailed, "", "HMod v2", 0.75, 0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, nil); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		got := handlerBySlug(t, stores, orgID, teamID, "hr-mod")
		if got == nil || got.Name != "User Renamed" {
			t.Errorf("user-modified handler overwritten by sync: %+v", got)
		}
	})

	t.Run("Trigger_BlueprintBinding_ReResolvedOnRebind", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		idA := seedOneStepBlueprint(t, stores, orgID, teamID, "hbp-rebind-a")
		idB := seedOneStepBlueprint(t, stores, orgID, teamID, "hbp-rebind-b")
		bySlug := map[string]string{"hbp-rebind-a": idA, "hbp-rebind-b": idB}

		handlersA := []db.ShippedEventHandler{
			syncTrigger("ht-rebind", domain.EventGitHubPRCICheckFailed, "", "hbp-rebind-a", 3, 0.0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, bySlug); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		if got := handlerBySlug(t, stores, orgID, teamID, "ht-rebind"); got == nil || got.BlueprintID != idA {
			t.Fatalf("trigger not bound to hbp-rebind-a: %+v", got)
		}

		// A shipped restructure re-points the trigger at a different blueprint —
		// this is how it reaches an unmodified team.
		handlersB := []db.ShippedEventHandler{
			syncTrigger("ht-rebind", domain.EventGitHubPRCICheckFailed, "", "hbp-rebind-b", 3, 0.0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, bySlug); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		got := handlerBySlug(t, stores, orgID, teamID, "ht-rebind")
		if got == nil || got.BlueprintID != idB {
			t.Errorf("trigger re-point not applied: got=%+v want blueprint_id=%s", got, idB)
		}
	})

	t.Run("Trigger_UnresolvableBlueprintSlug_Skipped", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		handlers := []db.ShippedEventHandler{
			syncTrigger("ht-orphan", domain.EventGitHubPRCICheckFailed, "", "no-such-blueprint-slug", 3, 0.0),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlers, map[string]string{}); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if got := handlerBySlug(t, stores, orgID, teamID, "ht-orphan"); got != nil {
			t.Errorf("trigger inserted despite an unresolvable blueprint slug: %+v", got)
		}
	})

	t.Run("SyncShippedIntoTeam_InsertsRealShippedHandlers", func(t *testing.T) {
		// End-to-end: the real db.ShippedEventHandlers list, wired through the
		// production entry point, resolving each trigger's blueprint slug
		// against the blueprint-sync pass that ran just before it in the same
		// call.
		stores, orgID, teamID, _ := factory(t)
		prompts := make([]domain.Prompt, 0)
		bps := make([]domain.SeedBlueprint, 0)
		seen := map[string]bool{}
		for _, h := range db.ShippedEventHandlers {
			if h.Kind != domain.EventHandlerKindTrigger || seen[h.BlueprintID] {
				continue
			}
			seen[h.BlueprintID] = true
			promptSlug := h.BlueprintID + "-p"
			prompts = append(prompts, syncPrompt(promptSlug, h.BlueprintID, "body", "", ""))
			bps = append(bps, syncBlueprint(h.BlueprintID, h.BlueprintID, promptSlug))
		}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, prompts, bps); err != nil {
			t.Fatalf("SyncShippedIntoTeam: %v", err)
		}
		for _, h := range db.ShippedEventHandlers {
			got := handlerBySlug(t, stores, orgID, teamID, h.ID)
			if got == nil {
				t.Errorf("shipped handler %s not inserted by SyncShippedIntoTeam", h.ID)
				continue
			}
			wantEnabled := h.Kind == domain.EventHandlerKindRule
			if got.Enabled != wantEnabled {
				t.Errorf("handler %s enabled=%v, want %v", h.ID, got.Enabled, wantEnabled)
			}
			if h.Kind == domain.EventHandlerKindTrigger {
				bp := blueprintBySlug(t, stores, orgID, teamID, h.BlueprintID)
				if bp == nil || got.BlueprintID != bp.ID {
					t.Errorf("handler %s blueprint_id=%q, want the synced %s copy %v", h.ID, got.BlueprintID, h.BlueprintID, bp)
				}
			}
		}

		// Idempotent: a second pass over an already-synced, unmodified team is a
		// pure no-op — no error, nothing skipped, nothing re-inserted.
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, prompts, bps); err != nil {
			t.Fatalf("re-sync: %v", err)
		}
	})
}
