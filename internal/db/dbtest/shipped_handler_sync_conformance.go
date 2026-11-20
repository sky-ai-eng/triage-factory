package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// syncHandler builds a shipped handler fixture for EventHandlerStore.Sync
// tests, blueprintSlug being the shipped blueprint's system_slug or empty
// (resolved through the test's own blueprintIDsBySlug map, exactly like
// production SyncShippedIntoTeam does).
func syncHandler(slug, mode, eventType, predicate, name string, priority float64, sortOrder int, blueprintSlug string) db.ShippedEventHandler {
	return db.ShippedEventHandler{
		ID: slug, Mode: mode, EventType: eventType, Predicate: predicate,
		Name: name, DefaultPriority: priority, SortOrder: sortOrder,
		BlueprintID: blueprintSlug, BreakerThreshold: 3,
	}
}

// RunShippedHandlerSyncConformance exercises the event-handler half of the
// boot-time shipped-defaults sync — EventHandlerStore.Sync directly (with
// fixture shipped lists, so cases don't depend on the real
// db.ShippedEventHandlers content) plus one end-to-end pass through
// ShippedDefaultsStore.SyncShippedIntoTeam (which always uses the real list)
// to prove the production wiring — blueprint pass, then handler pass,
// resolving blueprint bindings against post-blueprint-sync state.
func RunShippedHandlerSyncConformance(t *testing.T, factory ShippedSyncFactory) {
	t.Helper()
	ctx := context.Background()

	handlerBySlug := func(t *testing.T, stores db.Stores, handlerID ShippedHandlerIDBySlug, orgID, teamID, slug string) *domain.EventHandler {
		t.Helper()
		return handlerCopyBySlug(t, stores.EventHandlers, handlerID, orgID, teamID, slug)
	}
	blueprintBySlug := func(t *testing.T, stores db.Stores, orgID, teamID, slug string) *domain.Blueprint {
		t.Helper()
		rows, _, err := stores.Blueprints.List(ctx, orgID, db.BlueprintListFilter{TeamID: teamID}, db.Unwindowed)
		if err != nil {
			t.Fatalf("Blueprints.List for blueprint %q: %v", slug, err)
		}
		return ShippedCopyBySlug(t, rows, teamID, slug, blueprintKey)
	}

	// seedOneStepBlueprint syncs a single blueprint (via the
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

	t.Run("InsertMissing_InShippedModeAndSortOrder", func(t *testing.T) {
		stores, orgID, teamID, _, handlerID := factory(t)
		bpID := seedOneStepBlueprint(t, stores, orgID, teamID, "hbp-conv")
		handlers := []db.ShippedEventHandler{
			syncHandler("h-conv-watch", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HWatch", 0.5, 4, "hbp-conv"),
			syncHandler("h-conv-off", domain.EventHandlerModeOff, domain.EventGitHubPRCICheckFailed, "", "HOff", 0.5, 5, ""),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlers, map[string]string{"hbp-conv": bpID}); err != nil {
			t.Fatalf("sync: %v", err)
		}
		for _, c := range []struct {
			slug, mode, blueprint string
			sortOrder             int
		}{
			{"h-conv-watch", domain.EventHandlerModeWatch, bpID, 4},
			{"h-conv-off", domain.EventHandlerModeOff, "", 5},
		} {
			got := handlerBySlug(t, stores, handlerID, orgID, teamID, c.slug)
			if got == nil || got.Mode != c.mode || got.SortOrder != c.sortOrder || got.BlueprintID != c.blueprint {
				t.Errorf("inserted %s = %+v; want mode %q, sort_order %d, blueprint %q", c.slug, got, c.mode, c.sortOrder, c.blueprint)
			}
		}
	})

	t.Run("EqualContent_ZeroWrites", func(t *testing.T) {
		stores, orgID, teamID, _, handlerID := factory(t)
		handlers := []db.ShippedEventHandler{
			syncHandler("h-eq", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, `{"author_id_in":[]}`, "HEq", 0.5, 0, ""),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlers, nil); err != nil {
			t.Fatalf("sync: %v", err)
		}
		before := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-eq").UpdatedAt
		time.Sleep(15 * time.Millisecond)
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlers, nil); err != nil {
			t.Fatalf("re-sync: %v", err)
		}
		after := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-eq").UpdatedAt
		if !after.Equal(before) {
			t.Errorf("updated_at churned on an equal re-sync: before=%s after=%s", before, after)
		}
	})

	t.Run("StaleContent_UpdatedWithoutTouchingModeOrSortOrder", func(t *testing.T) {
		stores, orgID, teamID, _, handlerID := factory(t)
		handlersA := []db.ShippedEventHandler{
			syncHandler("h-stale", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, `{"author_id_in":[]}`, "HStale", 0.5, 0, ""),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, nil); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		r := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-stale")
		// Moved the way a team moves it: a mode and sort_order Update, which
		// must not stamp user_modified, or the sync below would skip this row
		// as user-owned and the content assertions would pass for the wrong
		// reason.
		r.Mode, r.SortOrder = domain.EventHandlerModeOff, 6
		if _, err := stores.EventHandlers.Update(ctx, orgID, *r); err != nil {
			t.Fatalf("turn off: %v", err)
		}

		handlersB := []db.ShippedEventHandler{
			syncHandler("h-stale", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, `{"author_id_in":["583231"]}`, "HStale v2", 0.75, 0, ""),
		}
		handlersB[0].BreakerThreshold, handlersB[0].MinAutonomySuitability = 5, 0.2
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, nil); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		got := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-stale")
		if got == nil {
			t.Fatal("handler vanished after sync")
		}
		if got.Mode != domain.EventHandlerModeOff || got.SortOrder != 6 || got.UserModified {
			t.Errorf("after a content sync: mode %q, sort_order %d, user_modified %v; want off, 6, false (sync never writes them)", got.Mode, got.SortOrder, got.UserModified)
		}
		if got.Name != "HStale v2" || got.DefaultPriority != 0.75 || got.BreakerThreshold != 5 || got.MinAutonomySuitability != 0.2 {
			t.Errorf("stale content not restored: %+v", got)
		}
		if got.ScopePredicateJSON == nil || !db.PredicateJSONEqual(*got.ScopePredicateJSON, `{"author_id_in":["583231"]}`) {
			t.Errorf("stale predicate not restored: %v", got.ScopePredicateJSON)
		}
	})

	t.Run("SoftDeletedHandler_SkippedBySync", func(t *testing.T) {
		stores, orgID, teamID, _, handlerID := factory(t)
		handlersA := []db.ShippedEventHandler{
			syncHandler("h-del", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HDel", 0.5, 0, ""),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, nil); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		r := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-del")
		if err := stores.EventHandlers.Delete(ctx, orgID, r.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		handlersB := []db.ShippedEventHandler{
			syncHandler("h-del", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HDel v2", 0.75, 0, ""),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, nil); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		if got := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-del"); got != nil {
			t.Errorf("soft-deleted handler resurrected by sync: %+v", got)
		}
	})

	t.Run("UserModifiedHandler_SkippedBySync", func(t *testing.T) {
		stores, orgID, teamID, _, handlerID := factory(t)
		handlersA := []db.ShippedEventHandler{
			syncHandler("h-mod", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HMod", 0.5, 0, ""),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, nil); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		r := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-mod")
		edited := *r
		edited.Name = "User Renamed"
		if _, err := stores.EventHandlers.Update(ctx, orgID, edited); err != nil {
			t.Fatalf("user update: %v", err)
		}
		handlersB := []db.ShippedEventHandler{
			syncHandler("h-mod", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HMod v2", 0.75, 0, ""),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, nil); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		got := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-mod")
		if got == nil || got.Name != "User Renamed" {
			t.Errorf("user-modified handler overwritten by sync: %+v", got)
		}
	})

	t.Run("BlueprintBinding_ReResolvedOnRebind", func(t *testing.T) {
		stores, orgID, teamID, _, handlerID := factory(t)
		idA := seedOneStepBlueprint(t, stores, orgID, teamID, "hbp-rebind-a")
		idB := seedOneStepBlueprint(t, stores, orgID, teamID, "hbp-rebind-b")
		bySlug := map[string]string{"hbp-rebind-a": idA, "hbp-rebind-b": idB}

		handlersA := []db.ShippedEventHandler{
			syncHandler("h-rebind", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HRebind", 0.5, 0, "hbp-rebind-a"),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, bySlug); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		if got := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-rebind"); got == nil || got.BlueprintID != idA {
			t.Fatalf("handler not bound to hbp-rebind-a: %+v", got)
		}

		// A shipped restructure re-points the handler at a different
		// blueprint — this is how it reaches an unmodified team.
		handlersB := []db.ShippedEventHandler{
			syncHandler("h-rebind", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HRebind", 0.5, 0, "hbp-rebind-b"),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, bySlug); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		got := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-rebind")
		if got == nil || got.BlueprintID != idB {
			t.Errorf("handler re-point not applied: got=%+v want blueprint_id=%s", got, idB)
		}
	})

	t.Run("UnresolvableBlueprintSlug_InsertsWithoutABlueprint", func(t *testing.T) {
		stores, orgID, teamID, _, handlerID := factory(t)
		handlers := []db.ShippedEventHandler{
			syncHandler("h-orphan", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HOrphan", 0.5, 0, "no-such-blueprint-slug"),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlers, map[string]string{}); err != nil {
			t.Fatalf("sync: %v", err)
		}
		got := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-orphan")
		if got == nil || got.BlueprintID != "" || got.Mode != domain.EventHandlerModeWatch {
			t.Errorf("handler with an unresolvable blueprint slug = %+v; want it inserted in watch with no blueprint", got)
		}
	})

	t.Run("UnresolvableBlueprintSlug_LeavesABoundBlueprint", func(t *testing.T) {
		stores, orgID, teamID, _, handlerID := factory(t)
		bpID := seedOneStepBlueprint(t, stores, orgID, teamID, "hbp-keep")
		handlersA := []db.ShippedEventHandler{
			syncHandler("h-keep", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HKeep", 0.5, 0, "hbp-keep"),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersA, map[string]string{"hbp-keep": bpID}); err != nil {
			t.Fatalf("sync A: %v", err)
		}
		handlersB := []db.ShippedEventHandler{
			syncHandler("h-keep", domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "", "HKeep v2", 0.5, 0, "hbp-keep"),
		}
		if err := stores.EventHandlers.Sync(ctx, orgID, teamID, handlersB, map[string]string{}); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		got := handlerBySlug(t, stores, handlerID, orgID, teamID, "h-keep")
		if got == nil || got.Name != "HKeep v2" || got.BlueprintID != bpID {
			t.Errorf("after a sync whose blueprint slug resolves to nothing = %+v; want the content synced and blueprint %q kept", got, bpID)
		}
	})

	t.Run("SyncShippedIntoTeam_InsertsRealShippedHandlers", func(t *testing.T) {
		// End-to-end: the real db.ShippedEventHandlers list, wired through the
		// production entry point, resolving each handler's blueprint slug
		// against the blueprint-sync pass that ran just before it in the same
		// call.
		stores, orgID, teamID, _, handlerID := factory(t)
		prompts := make([]domain.Prompt, 0)
		bps := make([]domain.SeedBlueprint, 0)
		seen := map[string]bool{}
		for _, h := range db.ShippedEventHandlers {
			if h.BlueprintID == "" || seen[h.BlueprintID] {
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
			got := handlerBySlug(t, stores, handlerID, orgID, teamID, h.ID)
			if got == nil {
				t.Errorf("shipped handler %s not inserted by SyncShippedIntoTeam", h.ID)
				continue
			}
			if got.Mode != h.Mode {
				t.Errorf("handler %s mode=%q, want %q", h.ID, got.Mode, h.Mode)
			}
			if h.BlueprintID != "" {
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
