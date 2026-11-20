package dbtest

import (
	"context"
	"errors"
	"maps"
	"slices"
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
// invokes before any test that creates a handler with a blueprint. A handler
// FKs to blueprints on both (blueprint_id, org_id) AND the same-team
// (blueprint_id, team_id), so the seeded blueprints must live on the
// factory's teamID; each backend wires its own seeding shape. handlerID is
// the raw slug resolver described at ShippedHandlerIDBySlug.
type EventHandlerStoreFactory func(t *testing.T) (store db.EventHandlerStore, orgID, teamID string, seedBlueprints BlueprintSeeder, handlerID ShippedHandlerIDBySlug)

// ShippedHandlerIDBySlug resolves the id of a team's live (not soft-deleted)
// copy of a shipped handler by its system_slug, or "" when the team has
// none. A backend supplies it as a raw read of event_handlers, because
// system_slug is a seed and idempotency key that no request-facing handler
// read projects — there is no store door a suite could observe it through.
// The suites resolve the id here and read the row itself through the
// store's Get, so everything but the slug→id step still goes through the
// live door.
type ShippedHandlerIDBySlug func(t *testing.T, teamID, slug string) string

// handlerCopyBySlug reads a team's copy of a shipped handler: the id via the
// backend's ShippedHandlerIDBySlug, the row via the store's Get. nil when the
// team has no live copy — a soft-deleted one reads as absent, as it does to
// a request.
func handlerCopyBySlug(t *testing.T, store db.EventHandlerStore, handlerID ShippedHandlerIDBySlug, orgID, teamID, slug string) *domain.EventHandler {
	t.Helper()
	id := handlerID(t, teamID, slug)
	if id == "" {
		return nil
	}
	h, err := store.Get(context.Background(), orgID, id)
	if err != nil {
		t.Fatalf("Get(shipped handler %q = %s): %v", slug, id, err)
	}
	return h
}

// BlueprintSeeder seeds blueprints into the harness DB at the given slugs so
// handler rows can reference them, and returns a slug→blueprint-id map. The
// id is a random UUID per copy; callers resolve a handler's blueprint slug to
// the seeded id through this map (mirroring the seed order: prompts →
// blueprints → EventHandlerStore.Seed).
type BlueprintSeeder func(t *testing.T, slugs ...string) map[string]string

// shippedBlueprintSlugs is every blueprint slug ShippedEventHandlers names.
func shippedBlueprintSlugs() []string {
	var out []string
	for _, h := range db.ShippedEventHandlers {
		if h.BlueprintID != "" && !slices.Contains(out, h.BlueprintID) {
			out = append(out, h.BlueprintID)
		}
	}
	return out
}

// newHandler is a valid user handler in mode on eventType, with every field
// at the create route's default.
func newHandler(mode, eventType, name, blueprintID string) domain.EventHandler {
	return domain.EventHandler{
		ID: uuid.New().String(), Mode: mode, EventType: eventType, Name: name,
		DefaultPriority: 0.5, BreakerThreshold: 4, BlueprintID: blueprintID,
	}
}

// RunEventHandlerStoreConformance runs the shared assertion suite for the
// event handler store on one backend.
func RunEventHandlerStoreConformance(t *testing.T, factory EventHandlerStoreFactory) {
	t.Helper()

	t.Run("every_single_row_write_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard applied to each single-row write in turn.
		// The property is one line — what the write handed back is what a point
		// read finds — and AssertWriteReturnedStoredRow's doc covers what that
		// stands in for (RETURNING semantics, RLS visibility on the update arm,
		// column-list drift).
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		read := func(id string) func() (*domain.EventHandler, error) {
			return func() (*domain.EventHandler, error) { return store.Get(ctx, orgID, id) }
		}
		bp := seedBlueprints(t, "system-ci-fix")["system-ci-fix"]

		h := newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "returned", bp)
		h.SortOrder = 1
		created, err := store.Create(ctx, orgID, teamID, h)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Create", created, read(h.ID))
		// Source is forced to "user" by the statement, and the timestamps are
		// server-stamped — three things the input struct never carried.
		if created.Source != "user" || created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
			t.Errorf("Create returned a row missing what only the row knows: %+v", created)
		}

		next := created
		next.Name = "returned v2"
		next.Mode = domain.EventHandlerModeAutomate
		updated, err := store.Update(ctx, orgID, next)
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "Update", updated, read(h.ID))

		// A miss is an error rather than a silent no-op.
		next.ID = uuid.New().String()
		if _, err := store.Update(ctx, orgID, next); !errors.Is(err, db.ErrNoSuchEventHandler) {
			t.Errorf("Update on a missing id: got %v, want db.ErrNoSuchEventHandler", err)
		}
	})

	t.Run("Seed_InsertsEveryShippedHandlerInItsMode", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, handlerID := factory(t)
		ids := seedBlueprints(t, shippedBlueprintSlugs()...)
		if err := store.Seed(context.Background(), orgID, teamID, ids); err != nil {
			t.Fatalf("Seed: %v", err)
		}
		for _, shipped := range db.ShippedEventHandlers {
			got := handlerCopyBySlug(t, store, handlerID, orgID, teamID, shipped.ID)
			if got == nil {
				t.Errorf("Seed left out shipped handler %s", shipped.ID)
				continue
			}
			want := ids[shipped.BlueprintID]
			if got.Mode != shipped.Mode || got.SortOrder != shipped.SortOrder || got.BlueprintID != want ||
				got.Name != shipped.Name || got.DefaultPriority != shipped.DefaultPriority ||
				got.BreakerThreshold != shipped.BreakerThreshold || got.MinAutonomySuitability != shipped.MinAutonomySuitability {
				t.Errorf("seeded %s = %+v; want the shipped row with blueprint %q", shipped.ID, got, want)
			}
		}
	})

	t.Run("Seed_IsIdempotent", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ids := seedBlueprints(t, shippedBlueprintSlugs()...)
		if err := store.Seed(context.Background(), orgID, teamID, ids); err != nil {
			t.Fatalf("Seed #1: %v", err)
		}
		_, first, _ := store.List(context.Background(), orgID, db.EventHandlerListFilter{}, db.ListOpts{CountOnly: true})
		if err := store.Seed(context.Background(), orgID, teamID, ids); err != nil {
			t.Fatalf("Seed #2: %v", err)
		}
		_, second, _ := store.List(context.Background(), orgID, db.EventHandlerListFilter{}, db.ListOpts{CountOnly: true})
		if first != len(db.ShippedEventHandlers) || second != first {
			t.Errorf("handlers after Seed, Seed = %d, %d; want %d both times", first, second, len(db.ShippedEventHandlers))
		}
	})

	t.Run("Create_EachMode", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		bp := seedBlueprints(t, "p-each-mode")["p-each-mode"]
		for _, mode := range domain.EventHandlerModes {
			h := newHandler(mode, domain.EventGitHubPRCICheckFailed, "in "+mode, "")
			if mode == domain.EventHandlerModeAutomate {
				h.BlueprintID = bp
			}
			h.MinAutonomySuitability = 0.3
			created, err := store.Create(ctx, orgID, teamID, h)
			if err != nil {
				t.Fatalf("Create in %s: %v", mode, err)
			}
			got, err := store.Get(ctx, orgID, created.ID)
			if err != nil || got == nil {
				t.Fatalf("Get %s handler: got=%v err=%v", mode, got, err)
			}
			if got.Mode != mode || got.Name != h.Name || got.BlueprintID != h.BlueprintID ||
				got.MinAutonomySuitability != 0.3 || got.AppliesToUnowned {
				t.Errorf("handler created in %s reads back %+v; want %+v", mode, got, h)
			}
		}
	})

	t.Run("Create_RefusesAutomateWithoutBlueprint", func(t *testing.T) {
		store, orgID, teamID, _, _ := factory(t)
		h := newHandler(domain.EventHandlerModeAutomate, domain.EventGitHubPRCICheckFailed, "no blueprint", "")
		if _, err := store.Create(context.Background(), orgID, teamID, h); err == nil {
			t.Fatal("Create in automate with no blueprint succeeded; want an error")
		}
		if got, _ := store.Get(context.Background(), orgID, h.ID); got != nil {
			t.Errorf("a refused Create left a row: %+v", got)
		}
	})

	t.Run("AppliesToUnowned_RoundTripAndUpdate", func(t *testing.T) {
		store, orgID, teamID, _, _ := factory(t)
		ctx := context.Background()
		h := newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "watch-handler", "")
		h.AppliesToUnowned = true
		if _, err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := store.Get(ctx, orgID, h.ID)
		if err != nil || got == nil || !got.AppliesToUnowned {
			t.Fatalf("Get after Create(applies_to_unowned) = %+v, %v; want the flag set", got, err)
		}
		got.AppliesToUnowned = false
		if _, err := store.Update(ctx, orgID, *got); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if got, _ := store.Get(ctx, orgID, h.ID); got == nil || got.AppliesToUnowned {
			t.Errorf("AppliesToUnowned after Update(false) = %+v; want false", got)
		}
	})

	t.Run("Update_ModeOnly_KeepsEveryFieldAndDoesNotStamp", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		bp := seedBlueprints(t, "p-mode-only")["p-mode-only"]
		pred := `{"author_id_in":["alice"]}`
		h := newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "mode only", bp)
		h.ScopePredicateJSON, h.DefaultPriority, h.SortOrder, h.BreakerThreshold, h.MinAutonomySuitability = &pred, 0.8, 3, 2, 0.4
		created, err := store.Create(ctx, orgID, teamID, h)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		next := created
		next.Mode = domain.EventHandlerModeAutomate
		updated, err := store.Update(ctx, orgID, next)
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.Mode != domain.EventHandlerModeAutomate || updated.UserModified {
			t.Errorf("after a mode-only Update: mode %q, user_modified %v; want automate, false", updated.Mode, updated.UserModified)
		}
		if updated.Name != created.Name || updated.BlueprintID != bp || !db.PredicateJSONEqual(*updated.ScopePredicateJSON, pred) ||
			updated.DefaultPriority != 0.8 || updated.SortOrder != 3 || updated.BreakerThreshold != 2 || updated.MinAutonomySuitability != 0.4 {
			t.Errorf("a mode-only Update changed another field: %+v; want %+v but in automate", updated, created)
		}
	})

	t.Run("Update_Blueprint_StampsUserModified", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-stamp-a", "p-stamp-b")
		created, err := store.Create(ctx, orgID, teamID, newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "blueprint stamp", ids["p-stamp-a"]))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		next := created
		next.BlueprintID = ids["p-stamp-b"]
		updated, err := store.Update(ctx, orgID, next)
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.BlueprintID != ids["p-stamp-b"] || !updated.UserModified {
			t.Errorf("after a blueprint Update: blueprint %q, user_modified %v; want %q, true", updated.BlueprintID, updated.UserModified, ids["p-stamp-b"])
		}
	})

	t.Run("Update_StampsUserModifiedOnContentFields", func(t *testing.T) {
		store, orgID, teamID, _, _ := factory(t)
		ctx := context.Background()
		created, err := store.Create(ctx, orgID, teamID, newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "content", ""))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		next := created
		next.SortOrder = 7
		updated, err := store.Update(ctx, orgID, next)
		if err != nil || updated.UserModified {
			t.Fatalf("a sort_order-only Update = %+v, %v; want user_modified false", updated, err)
		}
		next = updated
		next.Name = "content renamed"
		if updated, err = store.Update(ctx, orgID, next); err != nil || !updated.UserModified {
			t.Errorf("a name Update = %+v, %v; want user_modified true", updated, err)
		}
	})

	t.Run("Update_ClearsBlueprint", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		bp := seedBlueprints(t, "p-clear")["p-clear"]
		created, err := store.Create(ctx, orgID, teamID, newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "clear", bp))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		next := created
		next.BlueprintID = ""
		if updated, err := store.Update(ctx, orgID, next); err != nil || updated.BlueprintID != "" {
			t.Errorf("Update clearing the blueprint = %+v, %v; want no blueprint", updated, err)
		}
		next.Mode = domain.EventHandlerModeAutomate
		if _, err := store.Update(ctx, orgID, next); err == nil {
			t.Error("Update into automate with no blueprint succeeded; want an error")
		}
	})

	t.Run("Delete_LeavesARowNoReadReturns", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		bp := seedBlueprints(t, "p-delete")["p-delete"]
		h := newHandler(domain.EventHandlerModeAutomate, domain.EventGitHubPRCICheckFailed, "deleted", bp)
		if _, err := store.Create(ctx, orgID, teamID, h); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := store.Delete(ctx, orgID, h.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if got, err := store.Get(ctx, orgID, h.ID); err != nil || got != nil {
			t.Errorf("Get after Delete = (%v, %v); want (nil, nil)", got, err)
		}
		if got, err := store.GetSystem(ctx, orgID, h.ID); err != nil || got != nil {
			t.Errorf("GetSystem after Delete = (%v, %v); want (nil, nil)", got, err)
		}
		listed, total, err := store.List(ctx, orgID, db.EventHandlerListFilter{}, db.ListOpts{Limit: 200})
		if err != nil || total != 0 || len(listed) != 0 {
			t.Errorf("List after Delete = %d rows, total %d, %v; want none", len(listed), total, err)
		}
		if active, err := store.GetActiveForEvent(ctx, orgID, h.EventType); err != nil || len(active) != 0 {
			t.Errorf("GetActiveForEvent after Delete = %v, %v; want none", active, err)
		}
		if bound, err := store.ListForBlueprint(ctx, orgID, bp); err != nil || len(bound) != 0 {
			t.Errorf("ListForBlueprint after Delete = %v, %v; want none", bound, err)
		}
		next := h
		if _, err := store.Update(ctx, orgID, next); !errors.Is(err, db.ErrNoSuchEventHandler) {
			t.Errorf("Update after Delete: got %v, want db.ErrNoSuchEventHandler", err)
		}
	})

	t.Run("List_OrderAndFilters", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		bp := seedBlueprints(t, "p-list")["p-list"]
		// Created out of order: the list sorts by sort_order, then name.
		rows := []domain.EventHandler{
			newHandler(domain.EventHandlerModeAutomate, domain.EventGitHubPRCICheckFailed, "c", bp),
			newHandler(domain.EventHandlerModeNotify, domain.EventGitHubPRReviewCommented, "b", ""),
			newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "a", ""),
			newHandler(domain.EventHandlerModeOff, domain.EventGitHubPRReviewCommented, "d", bp),
		}
		rows[0].SortOrder, rows[1].SortOrder, rows[2].SortOrder, rows[3].SortOrder = 1, 0, 1, 2
		for _, h := range rows {
			if _, err := store.Create(ctx, orgID, teamID, h); err != nil {
				t.Fatalf("Create %s: %v", h.Name, err)
			}
		}
		names := func(f db.EventHandlerListFilter) []string {
			t.Helper()
			listed, total, err := store.List(ctx, orgID, f, db.ListOpts{Limit: 200})
			if err != nil {
				t.Fatalf("List(%+v): %v", f, err)
			}
			if total != len(listed) {
				t.Errorf("List(%+v) total %d, %d rows", f, total, len(listed))
			}
			var out []string
			for _, h := range listed {
				out = append(out, h.Name)
			}
			return out
		}
		yes, no := true, false
		for _, c := range []struct {
			filter db.EventHandlerListFilter
			want   []string
		}{
			{db.EventHandlerListFilter{}, []string{"b", "a", "c", "d"}},
			{db.EventHandlerListFilter{Modes: []string{domain.EventHandlerModeWatch, domain.EventHandlerModeAutomate}}, []string{"a", "c"}},
			{db.EventHandlerListFilter{EventType: domain.EventGitHubPRReviewCommented}, []string{"b", "d"}},
			{db.EventHandlerListFilter{HasBlueprint: &yes}, []string{"c", "d"}},
			{db.EventHandlerListFilter{HasBlueprint: &no}, []string{"b", "a"}},
		} {
			if got := names(c.filter); !slices.Equal(got, c.want) {
				t.Errorf("List(%+v) = %v; want %v", c.filter, got, c.want)
			}
		}
	})

	t.Run("GetActiveForEvent_LeavesOutOff", func(t *testing.T) {
		store, orgID, teamID, _, _ := factory(t)
		ctx := context.Background()
		want := map[string]bool{}
		for _, mode := range []string{domain.EventHandlerModeOff, domain.EventHandlerModeNotify, domain.EventHandlerModeWatch} {
			h := newHandler(mode, domain.EventGitHubPRCICheckFailed, mode, "")
			if _, err := store.Create(ctx, orgID, teamID, h); err != nil {
				t.Fatalf("Create %s: %v", mode, err)
			}
			if mode != domain.EventHandlerModeOff {
				want[h.ID] = true
			}
		}
		active, err := store.GetActiveForEvent(ctx, orgID, domain.EventGitHubPRCICheckFailed)
		if err != nil {
			t.Fatalf("GetActiveForEvent: %v", err)
		}
		got := map[string]bool{}
		for _, h := range active {
			got[h.ID] = true
		}
		if !maps.Equal(got, want) {
			t.Errorf("GetActiveForEvent = %v; want the notify and watch handlers %v", got, want)
		}
	})

	t.Run("DetachBlueprint_ClearsEveryHandlerAndMovesAutomateToWatch", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, "p-detach", "p-detach-other")
		automating := newHandler(domain.EventHandlerModeAutomate, domain.EventGitHubPRCICheckFailed, "automating", ids["p-detach"])
		watching := newHandler(domain.EventHandlerModeOff, domain.EventGitHubPRReviewCommented, "off", ids["p-detach"])
		other := newHandler(domain.EventHandlerModeAutomate, domain.EventGitHubPRCICheckFailed, "other", ids["p-detach-other"])
		for _, h := range []domain.EventHandler{automating, watching, other} {
			if _, err := store.Create(ctx, orgID, teamID, h); err != nil {
				t.Fatalf("Create %s: %v", h.Name, err)
			}
		}
		if err := store.DetachBlueprint(ctx, orgID, ids["p-detach"]); err != nil {
			t.Fatalf("DetachBlueprint: %v", err)
		}
		for _, c := range []struct {
			id, mode, blueprint string
		}{
			{automating.ID, domain.EventHandlerModeWatch, ""},
			{watching.ID, domain.EventHandlerModeOff, ""},
			{other.ID, domain.EventHandlerModeAutomate, ids["p-detach-other"]},
		} {
			got, err := store.Get(ctx, orgID, c.id)
			if err != nil || got == nil {
				t.Fatalf("Get %s: %v, %v", c.id, got, err)
			}
			if got.Mode != c.mode || got.BlueprintID != c.blueprint || got.UserModified {
				t.Errorf("%s after DetachBlueprint: mode %q, blueprint %q, user_modified %v; want %q, %q, false",
					got.Name, got.Mode, got.BlueprintID, got.UserModified, c.mode, c.blueprint)
			}
		}
	})

	t.Run("Reorder_RenumbersEveryModeWithoutStamping", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		bp := seedBlueprints(t, "p-reorder")["p-reorder"]
		var ids []string
		for i, mode := range domain.EventHandlerModes {
			h := newHandler(mode, domain.EventGitHubPRCICheckFailed, "position-"+strconv.Itoa(i), bp)
			h.SortOrder = i
			if _, err := store.Create(ctx, orgID, teamID, h); err != nil {
				t.Fatalf("Create %s: %v", mode, err)
			}
			ids = append(ids, h.ID)
		}
		reversed := slices.Clone(ids)
		slices.Reverse(reversed)
		if err := store.Reorder(ctx, orgID, reversed); err != nil {
			t.Fatalf("Reorder: %v", err)
		}
		for want, id := range reversed {
			got, err := store.Get(ctx, orgID, id)
			if err != nil || got == nil {
				t.Fatalf("Get %s: %v, %v", id, got, err)
			}
			if got.SortOrder != want || got.UserModified {
				t.Errorf("%s after Reorder: sort_order %d, user_modified %v; want %d, false", got.Name, got.SortOrder, got.UserModified, want)
			}
		}
	})

	t.Run("ListForBlueprint_EveryMode", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, _ := factory(t)
		ctx := context.Background()
		bp := seedBlueprints(t, "p-shared-blueprint")["p-shared-blueprint"]
		want := map[string]bool{}
		for _, mode := range domain.EventHandlerModes {
			h := newHandler(mode, domain.EventGitHubPRCICheckFailed, mode, bp)
			if _, err := store.Create(ctx, orgID, teamID, h); err != nil {
				t.Fatalf("Create %s: %v", mode, err)
			}
			want[h.ID] = true
		}
		if _, err := store.Create(ctx, orgID, teamID, newHandler(domain.EventHandlerModeWatch, domain.EventGitHubPRCICheckFailed, "none", "")); err != nil {
			t.Fatalf("Create without a blueprint: %v", err)
		}
		bound, err := store.ListForBlueprint(ctx, orgID, bp)
		if err != nil {
			t.Fatalf("ListForBlueprint: %v", err)
		}
		got := map[string]bool{}
		for _, h := range bound {
			got[h.ID] = true
		}
		if !maps.Equal(got, want) {
			t.Errorf("ListForBlueprint = %v; want the handler in every mode %v", got, want)
		}
	})

	t.Run("Delete_SoftDeletesSystemRow_SlugStaysOccupiedAndInvisible", func(t *testing.T) {
		store, orgID, teamID, seedBlueprints, handlerID := factory(t)
		ctx := context.Background()
		ids := seedBlueprints(t, shippedBlueprintSlugs()...)
		if err := store.Seed(ctx, orgID, teamID, ids); err != nil {
			t.Fatalf("Seed: %v", err)
		}
		const slug = "system-handler-ci-check-failed"
		before := handlerCopyBySlug(t, store, handlerID, orgID, teamID, slug)
		if before == nil {
			t.Fatalf("shipped handler %q missing before delete", slug)
		}
		if err := store.Delete(ctx, orgID, before.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if got := handlerCopyBySlug(t, store, handlerID, orgID, teamID, slug); got != nil {
			t.Errorf("shipped slug %q still resolves after soft-delete: %+v; want absent", slug, got)
		}
		// Never resurrect: re-seeding must not re-insert a row for the
		// slug's occupied slot.
		if err := store.Seed(ctx, orgID, teamID, ids); err != nil {
			t.Fatalf("re-seed: %v", err)
		}
		if got := handlerCopyBySlug(t, store, handlerID, orgID, teamID, slug); got != nil {
			t.Errorf("shipped slug %q resolves after re-seed: %+v; want absent — re-seed must not resurrect a soft-deleted shipped row", slug, got)
		}
	})
}
