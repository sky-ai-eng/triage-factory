package dbtest

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// ShippedSyncFactory wires the shipped-defaults sync suite against one backend.
// It returns:
//   - a full db.Stores bundle wired to the admin pool (SyncShippedIntoTeam is
//     claims-less admin work), so the suite can seed, mutate, and re-read
//     through the store interfaces alone
//   - the orgID + a provisioned teamID (the teams row must exist —
//     SyncShippedIntoTeam reads teams.shipped_defaults_backfilled_at)
//   - resetBackfill, which clears that marker (raw UPDATE) so the grandfather
//     backfill can be exercised without inventing a pre-stamping-era install
type ShippedSyncFactory func(t *testing.T) (stores db.Stores, orgID, teamID string, resetBackfill func(t *testing.T))

// syncBlueprint builds a shipped blueprint definition from a slug, name, and its
// ordered step prompt slugs.
func syncBlueprint(slug, name string, stepSlugs ...string) domain.SeedBlueprint {
	return domain.SeedBlueprint{SystemSlug: slug, Name: name, StepPromptSlugs: stepSlugs}
}

// syncPrompt builds a shipped prompt with explicit content fields.
func syncPrompt(slug, name, body, model, tools string) domain.Prompt {
	return domain.Prompt{SystemSlug: slug, Name: name, Body: body, Model: model, AllowedTools: tools, Source: "system"}
}

// RunShippedSyncConformance exercises ShippedDefaultsStore.SyncShippedIntoTeam
// against any backend. The initial team state is materialized by an insert-path
// sync (a fresh team has no units, so SyncShippedIntoTeam inserts them whole);
// each case then mutates and re-syncs with a different shipped list, asserting
// the normative sync-unit rules.
func RunShippedSyncConformance(t *testing.T, factory ShippedSyncFactory) {
	t.Helper()
	ctx := context.Background()

	promptBySlug := func(t *testing.T, stores db.Stores, orgID, teamID, slug string) *domain.Prompt {
		t.Helper()
		p, err := stores.Prompts.GetBySystemSlug(ctx, orgID, teamID, slug)
		if err != nil {
			t.Fatalf("GetBySystemSlug(prompt %q): %v", slug, err)
		}
		return p
	}
	blueprintBySlug := func(t *testing.T, stores db.Stores, orgID, teamID, slug string) *domain.Blueprint {
		t.Helper()
		b, err := stores.Blueprints.GetBySystemSlug(ctx, orgID, teamID, slug)
		if err != nil {
			t.Fatalf("GetBySystemSlug(blueprint %q): %v", slug, err)
		}
		return b
	}
	stepSlugsOf := func(t *testing.T, stores db.Stores, orgID, teamID, blueprintSlug string) []string {
		t.Helper()
		bp := blueprintBySlug(t, stores, orgID, teamID, blueprintSlug)
		if bp == nil {
			t.Fatalf("blueprint %q not found", blueprintSlug)
		}
		steps, err := stores.Blueprints.ListSteps(ctx, orgID, bp.ID)
		if err != nil {
			t.Fatalf("ListSteps(%q): %v", blueprintSlug, err)
		}
		out := make([]string, 0, len(steps))
		for _, st := range steps {
			p, err := stores.Prompts.GetSystem(ctx, orgID, st.StepPromptID)
			if err != nil || p == nil {
				t.Fatalf("GetSystem(step prompt %s): (%v, %v)", st.StepPromptID, p, err)
			}
			out = append(out, p.SystemSlug)
		}
		return out
	}

	t.Run("StaleUnmodifiedUnit_Updated", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		promptsA := []domain.Prompt{syncPrompt("ci", "CI Fix", "old body", "", "")}
		bpsA := []domain.SeedBlueprint{syncBlueprint("ci", "CI Fix", "ci")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsA, bpsA); err != nil {
			t.Fatalf("seed sync: %v", err)
		}
		promptsB := []domain.Prompt{syncPrompt("ci", "CI Fix v2", "new body", "haiku", "Read")}
		bpsB := []domain.SeedBlueprint{syncBlueprint("ci", "CI Fix v2", "ci")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsB, bpsB); err != nil {
			t.Fatalf("sync: %v", err)
		}
		p := promptBySlug(t, stores, orgID, teamID, "ci")
		if p == nil {
			t.Fatal("prompt ci missing after sync")
		}
		if p.Body != "new body" || p.Name != "CI Fix v2" || p.Model != "haiku" || p.AllowedTools != "Read" {
			t.Errorf("stale prompt not restored: %+v", p)
		}
		if b := blueprintBySlug(t, stores, orgID, teamID, "ci"); b == nil || b.Name != "CI Fix v2" {
			t.Errorf("blueprint header not restored: %+v", b)
		}
	})

	t.Run("UserModifiedStepPrompt_SkipsWholeUnit", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		promptsA := []domain.Prompt{
			syncPrompt("a", "A", "a1", "", ""),
			syncPrompt("b", "B", "b1", "", ""),
			syncPrompt("c", "C", "c1", "", ""),
		}
		bpsA := []domain.SeedBlueprint{syncBlueprint("rev", "Review", "a", "b", "c")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsA, bpsA); err != nil {
			t.Fatalf("seed sync: %v", err)
		}
		// User edits step b — stamps user_modified on that prompt row.
		bID := promptBySlug(t, stores, orgID, teamID, "b").ID
		if err := stores.Prompts.Update(ctx, orgID, bID, "B edited", "user edit", ""); err != nil {
			t.Fatalf("user update b: %v", err)
		}
		promptsB := []domain.Prompt{
			syncPrompt("a", "A2", "a2", "", ""),
			syncPrompt("b", "B2", "b2", "", ""),
			syncPrompt("c", "C2", "c2", "", ""),
		}
		bpsB := []domain.SeedBlueprint{syncBlueprint("rev", "Review v2", "a", "b", "c")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsB, bpsB); err != nil {
			t.Fatalf("sync: %v", err)
		}
		// The whole unit is skipped: neither sibling is updated, and b keeps the edit.
		if p := promptBySlug(t, stores, orgID, teamID, "a"); p.Body != "a1" {
			t.Errorf("sibling a overwritten despite user-modified unit: body=%q", p.Body)
		}
		if p := promptBySlug(t, stores, orgID, teamID, "c"); p.Body != "c1" {
			t.Errorf("sibling c overwritten despite user-modified unit: body=%q", p.Body)
		}
		if p := promptBySlug(t, stores, orgID, teamID, "b"); p.Body != "user edit" {
			t.Errorf("user-modified b overwritten: body=%q", p.Body)
		}
	})

	t.Run("UserModifiedBlueprintHeader_SkipsUnit", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		promptsA := []domain.Prompt{syncPrompt("x", "X", "x1", "", "")}
		bpsA := []domain.SeedBlueprint{syncBlueprint("hdr", "Header", "x")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsA, bpsA); err != nil {
			t.Fatalf("seed sync: %v", err)
		}
		// User renames the blueprint header — stamps user_modified on the header.
		bpID := blueprintBySlug(t, stores, orgID, teamID, "hdr").ID
		if err := stores.Blueprints.Rename(ctx, orgID, bpID, "Renamed"); err != nil {
			t.Fatalf("rename: %v", err)
		}
		promptsB := []domain.Prompt{syncPrompt("x", "X2", "x2", "", "")}
		bpsB := []domain.SeedBlueprint{syncBlueprint("hdr", "Header v2", "x")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsB, bpsB); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if p := promptBySlug(t, stores, orgID, teamID, "x"); p.Body != "x1" {
			t.Errorf("step prompt overwritten despite user-modified header: body=%q", p.Body)
		}
		if b := blueprintBySlug(t, stores, orgID, teamID, "hdr"); b.Name != "Renamed" {
			t.Errorf("header name = %q, want the user's Renamed (unit must be skipped)", b.Name)
		}
	})

	t.Run("EqualContent_ZeroWrites", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		prompts := []domain.Prompt{syncPrompt("eq", "Eq", "same", "opus", "Read")}
		bps := []domain.SeedBlueprint{syncBlueprint("eq", "Eq", "eq")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, prompts, bps); err != nil {
			t.Fatalf("seed sync: %v", err)
		}
		beforeP := promptBySlug(t, stores, orgID, teamID, "eq").UpdatedAt
		beforeB := blueprintBySlug(t, stores, orgID, teamID, "eq").UpdatedAt
		time.Sleep(15 * time.Millisecond)
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, prompts, bps); err != nil {
			t.Fatalf("re-sync: %v", err)
		}
		if after := promptBySlug(t, stores, orgID, teamID, "eq").UpdatedAt; !after.Equal(beforeP) {
			t.Errorf("prompt updated_at churned on equal re-sync: before=%s after=%s", beforeP, after)
		}
		if after := blueprintBySlug(t, stores, orgID, teamID, "eq").UpdatedAt; !after.Equal(beforeB) {
			t.Errorf("blueprint updated_at churned on equal re-sync: before=%s after=%s", beforeB, after)
		}
	})

	t.Run("SoftDeletedBlueprint_Skipped", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		promptsA := []domain.Prompt{syncPrompt("dx", "DX", "dx1", "", "")}
		bpsA := []domain.SeedBlueprint{syncBlueprint("del", "Del", "dx")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsA, bpsA); err != nil {
			t.Fatalf("seed sync: %v", err)
		}
		bpID := blueprintBySlug(t, stores, orgID, teamID, "del").ID
		if err := stores.Blueprints.Delete(ctx, orgID, bpID); err != nil {
			t.Fatalf("delete blueprint: %v", err)
		}
		promptsB := []domain.Prompt{syncPrompt("dx", "DX2", "dx2", "", "")}
		bpsB := []domain.SeedBlueprint{syncBlueprint("del", "Del v2", "dx")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsB, bpsB); err != nil {
			t.Fatalf("sync: %v", err)
		}
		// Not resurrected (the slug stays occupied by the soft-deleted row) and the
		// step prompt is left as-is because the unit was skipped.
		if b := blueprintBySlug(t, stores, orgID, teamID, "del"); b != nil {
			t.Errorf("soft-deleted blueprint resurrected: %+v", b)
		}
		if p := promptBySlug(t, stores, orgID, teamID, "dx"); p.Body != "dx1" {
			t.Errorf("step prompt of a soft-deleted unit was updated: body=%q", p.Body)
		}
	})

	t.Run("NewShippedSlug_Inserted", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		promptsA := []domain.Prompt{syncPrompt("pa", "PA", "pa1", "", "")}
		bpsA := []domain.SeedBlueprint{syncBlueprint("ua", "UA", "pa")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsA, bpsA); err != nil {
			t.Fatalf("seed sync: %v", err)
		}
		promptsB := []domain.Prompt{
			syncPrompt("pa", "PA", "pa1", "", ""),
			syncPrompt("pnew", "PNew", "pnew body", "haiku", "Write"),
		}
		bpsB := []domain.SeedBlueprint{
			syncBlueprint("ua", "UA", "pa"),
			syncBlueprint("unew", "UNew", "pnew"),
		}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsB, bpsB); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if b := blueprintBySlug(t, stores, orgID, teamID, "unew"); b == nil {
			t.Fatal("new shipped blueprint not inserted")
		}
		np := promptBySlug(t, stores, orgID, teamID, "pnew")
		if np == nil || np.Body != "pnew body" || np.Model != "haiku" || np.AllowedTools != "Write" {
			t.Errorf("new shipped prompt not inserted correctly: %+v", np)
		}
		if slugs := stepSlugsOf(t, stores, orgID, teamID, "unew"); len(slugs) != 1 || slugs[0] != "pnew" {
			t.Errorf("new unit steps = %v, want [pnew]", slugs)
		}
	})

	t.Run("Restructure_AddsAndDropsStep", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		promptsA := []domain.Prompt{
			syncPrompt("pa", "PA", "pa1", "", ""),
			syncPrompt("pb", "PB", "pb1", "", ""),
		}
		bpsA := []domain.SeedBlueprint{syncBlueprint("rs", "RS", "pa", "pb")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsA, bpsA); err != nil {
			t.Fatalf("seed sync: %v", err)
		}
		pbID := promptBySlug(t, stores, orgID, teamID, "pb").ID

		promptsB := []domain.Prompt{
			syncPrompt("pa", "PA", "pa1", "", ""),
			syncPrompt("pc", "PC", "pc body", "", ""),
		}
		bpsB := []domain.SeedBlueprint{syncBlueprint("rs", "RS", "pa", "pc")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsB, bpsB); err != nil {
			t.Fatalf("sync: %v", err)
		}
		if slugs := stepSlugsOf(t, stores, orgID, teamID, "rs"); len(slugs) != 2 || slugs[0] != "pa" || slugs[1] != "pc" {
			t.Errorf("restructured steps = %v, want [pa pc]", slugs)
		}
		if p := promptBySlug(t, stores, orgID, teamID, "pc"); p == nil || p.Body != "pc body" {
			t.Errorf("added step prompt not inserted: %+v", p)
		}
		// The dropped step prompt is soft-deleted: request reads hide it, but the
		// row survives (GetSystem resolves it) as the audit trail.
		if p := promptBySlug(t, stores, orgID, teamID, "pb"); p != nil {
			t.Errorf("dropped step prompt still request-visible: %+v", p)
		}
		if p, err := stores.Prompts.GetSystem(ctx, orgID, pbID); err != nil || p == nil {
			t.Errorf("dropped step prompt hard-deleted; GetSystem = (%v, %v), want the row", p, err)
		}
	})

	t.Run("InconsistentShippedList_Errors", func(t *testing.T) {
		stores, orgID, teamID, _ := factory(t)
		// A shipped blueprint references a step slug with no matching shipped
		// prompt (a release-authoring mistake). The sync must fail loudly rather
		// than resolve it to empty content and clobber/insert an empty row.
		prompts := []domain.Prompt{syncPrompt("real", "Real", "r", "", "")}
		bps := []domain.SeedBlueprint{syncBlueprint("u", "U", "real", "ghost")}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, prompts, bps); err == nil {
			t.Fatal("SyncShippedIntoTeam accepted a blueprint referencing a slug absent from the shipped prompt list; want an error")
		}
	})

	t.Run("Backfill_StampsDivergedUnflagged_Once", func(t *testing.T) {
		stores, orgID, teamID, resetBackfill := factory(t)
		promptsA := []domain.Prompt{
			syncPrompt("a", "A", "a1", "", ""),
			syncPrompt("b", "B", "b1", "", ""),
			syncPrompt("c", "C", "c1", "", ""),
		}
		bpsA := []domain.SeedBlueprint{
			syncBlueprint("s1", "S1", "a", "b"),
			syncBlueprint("s2", "S2", "c"),
		}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsA, bpsA); err != nil {
			t.Fatalf("seed sync: %v", err)
		}
		// Simulate a pre-stamping-era install: the units are unflagged and the
		// backfill marker is unset.
		resetBackfill(t)

		// First real sync: shipped S1 dropped a step, so the team's [a,b] structure
		// diverges from shipped [a] → backfill stamps user_modified on S1 (and only
		// S1). S1 is then skipped; its structure is grandfathered, not restructured.
		promptsB := []domain.Prompt{
			syncPrompt("a", "A", "a1", "", ""),
			syncPrompt("c", "C", "c1", "", ""),
		}
		bpsB := []domain.SeedBlueprint{
			syncBlueprint("s1", "S1", "a"),
			syncBlueprint("s2", "S2", "c"),
		}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsB, bpsB); err != nil {
			t.Fatalf("sync B: %v", err)
		}
		if b := blueprintBySlug(t, stores, orgID, teamID, "s1"); b == nil || !b.UserModified {
			t.Errorf("diverged unflagged S1 was not grandfather-stamped: %+v", b)
		}
		if slugs := stepSlugsOf(t, stores, orgID, teamID, "s1"); len(slugs) != 2 || slugs[0] != "a" || slugs[1] != "b" {
			t.Errorf("grandfathered S1 structure = %v, want the preserved [a b]", slugs)
		}
		if b := blueprintBySlug(t, stores, orgID, teamID, "s2"); b == nil || b.UserModified {
			t.Errorf("equal S2 wrongly stamped by backfill: %+v", b)
		}

		// Second sync: S2 now diverges (shipped adds d). The marker is set, so the
		// backfill does NOT re-run — S2 is not stamped; instead the ordinary sync
		// restructures it. This is what proves the backfill ran exactly once.
		promptsC := []domain.Prompt{
			syncPrompt("a", "A", "a1", "", ""),
			syncPrompt("c", "C", "c1", "", ""),
			syncPrompt("d", "D", "d body", "", ""),
		}
		bpsC := []domain.SeedBlueprint{
			syncBlueprint("s1", "S1", "a"),
			syncBlueprint("s2", "S2", "c", "d"),
		}
		if err := stores.ShippedDefaults.SyncShippedIntoTeam(ctx, orgID, teamID, promptsC, bpsC); err != nil {
			t.Fatalf("sync C: %v", err)
		}
		if b := blueprintBySlug(t, stores, orgID, teamID, "s2"); b == nil || b.UserModified {
			t.Errorf("S2 was stamped on the second sync — backfill re-ran (want exactly once): %+v", b)
		}
		if slugs := stepSlugsOf(t, stores, orgID, teamID, "s2"); len(slugs) != 2 || slugs[0] != "c" || slugs[1] != "d" {
			t.Errorf("S2 not restructured on second sync = %v, want [c d]", slugs)
		}
	})
}
