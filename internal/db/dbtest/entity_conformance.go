package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// EntityStoreFactory is what a per-backend test file hands to
// RunEntityStoreConformance. Returns:
//   - the wired EntityStore impl,
//   - the orgID to pass to every call,
//   - an EntitySeeder for the project rows the assign-project subtests
//     need (entities themselves come from FindOrCreate).
type EntityStoreFactory func(t *testing.T) (
	store db.EntityStore,
	orgID string,
	seed EntitySeeder,
)

// EntitySeeder is a bag of callbacks the conformance suite uses to
// stage non-entity fixture rows.
type EntitySeeder struct {
	// Project inserts a project row and returns its id. The
	// AssignProject subtests need a real FK target.
	Project func(t *testing.T, name string) string
}

// RunEntityStoreConformance covers the entity-store contract every
// backend impl must hold:
//
//   - FindOrCreate inserts then re-reads on the same key.
//   - Get / GetBySource return (nil, nil) on miss.
//   - Update* mutations land on the right column, with UpdateSnapshot
//     also stamping last_polled_at and PatchSnapshot deliberately
//     leaving it alone.
//   - MarkClosed is unconditional; Close only fires when state='active';
//     Reactivate only fires when state='closed'.
//   - AssignProject stores both the FK and the rationale, and surfaces
//     sql.ErrNoRows when the entity id doesn't exist.
//   - ListUnclassified / ListActive / ListProjectPanel filter on the
//     documented predicates.
//   - Descriptions dedupes the input id list and only returns ids
//     whose description is non-empty.
//   - ClassificationStatusSystem reports (classified, exists) keyed on
//     classified_at (not project_id), with a missing row as (false,
//     false, nil).
func RunEntityStoreConformance(t *testing.T, mk EntityStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("FindOrCreate_inserts_then_returns_existing", func(t *testing.T) {
		s, orgID, _ := mk(t)

		first, created, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#1", "pr", "Title", "https://example.com/1")
		if err != nil {
			t.Fatalf("first FindOrCreate: %v", err)
		}
		if !created {
			t.Fatalf("expected created=true on first call")
		}
		if first.ID == "" {
			t.Fatalf("first.ID empty")
		}
		if first.Source != "github" || first.SourceID != "owner/repo#1" || first.Kind != "pr" {
			t.Errorf("unexpected entity fields: %+v", first)
		}
		if first.Title != "Title" {
			t.Errorf("title = %q, want %q", first.Title, "Title")
		}
		if first.State != "active" {
			t.Errorf("initial state = %q, want active", first.State)
		}

		second, created, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#1", "pr", "Other", "https://example.com/other")
		if err != nil {
			t.Fatalf("second FindOrCreate: %v", err)
		}
		if created {
			t.Errorf("expected created=false on second call")
		}
		if second.ID != first.ID {
			t.Errorf("second.ID = %s, want %s", second.ID, first.ID)
		}
		// Title is not refreshed by FindOrCreate — pre-existing rows
		// keep their stored value. The tracker calls UpdateTitle
		// explicitly when it detects a drift.
		if second.Title != "Title" {
			t.Errorf("title should be unchanged on re-discover, got %q", second.Title)
		}
	})

	t.Run("Get_and_GetBySource_return_nil_on_miss", func(t *testing.T) {
		s, orgID, _ := mk(t)

		// Use a uuid-shape miss id so the Postgres path's uuid column
		// can bind without rejecting the input on cast.
		got, err := s.Get(ctx, orgID, uuid.New().String())
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != nil {
			t.Errorf("Get on missing id returned %+v, want nil", got)
		}

		gotBySrc, err := s.GetBySource(ctx, orgID, "github", "nonexistent/repo#999")
		if err != nil {
			t.Fatalf("GetBySource: %v", err)
		}
		if gotBySrc != nil {
			t.Errorf("GetBySource on miss returned %+v, want nil", gotBySrc)
		}
	})

	t.Run("UpdateSnapshot_stamps_last_polled_at", func(t *testing.T) {
		s, orgID, _ := mk(t)

		if _, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#2", "pr", "T", ""); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Re-read so the baseline matches the backend's storage
		// precision (Postgres timestamptz truncates to microseconds;
		// FindOrCreate's returned struct carries Go's nanosec time
		// and wouldn't .Equal() the round-tripped value).
		baseline, err := s.GetBySource(ctx, orgID, "github", "owner/repo#2")
		if err != nil || baseline == nil || baseline.LastPolledAt == nil {
			t.Fatalf("baseline re-read: %v", err)
		}
		initialPolled := baseline.LastPolledAt

		// Sleep past the backend's clock resolution before the update
		// so the new stamp lands in a later bucket — without this, a
		// fast Postgres host can store both timestamps in the same
		// microsecond bin and .After() returns false.
		time.Sleep(2 * time.Millisecond)

		if err := s.UpdateSnapshot(ctx, orgID, baseline.ID, `{"k":"v"}`); err != nil {
			t.Fatalf("UpdateSnapshot: %v", err)
		}

		got, err := s.Get(ctx, orgID, baseline.ID)
		if err != nil || got == nil {
			t.Fatalf("re-read: %v", err)
		}
		if !strings.Contains(got.SnapshotJSON, `"k"`) {
			t.Errorf("snapshot_json missing payload: %q", got.SnapshotJSON)
		}
		if got.LastPolledAt == nil || !got.LastPolledAt.After(*initialPolled) {
			t.Errorf("UpdateSnapshot should have advanced last_polled_at — initial=%v after=%v",
				initialPolled, got.LastPolledAt)
		}
	})

	t.Run("PatchSnapshot_does_not_touch_last_polled_at", func(t *testing.T) {
		s, orgID, _ := mk(t)

		if _, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#3", "pr", "T", ""); err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Re-read for a DB-precision baseline — see UpdateSnapshot
		// subtest above for the timestamptz-truncation rationale.
		baseline, err := s.GetBySource(ctx, orgID, "github", "owner/repo#3")
		if err != nil || baseline == nil || baseline.LastPolledAt == nil {
			t.Fatalf("baseline re-read: %v", err)
		}
		initialPolled := baseline.LastPolledAt

		if err := s.PatchSnapshot(ctx, orgID, baseline.ID, `{"patched":true}`); err != nil {
			t.Fatalf("PatchSnapshot: %v", err)
		}

		got, err := s.Get(ctx, orgID, baseline.ID)
		if err != nil || got == nil {
			t.Fatalf("re-read: %v", err)
		}
		if !strings.Contains(got.SnapshotJSON, `"patched"`) {
			t.Errorf("snapshot_json missing patched payload: %q", got.SnapshotJSON)
		}
		// last_polled_at must remain at the baseline timestamp — the
		// helper exists precisely so the poll gate still considers
		// the row stale enough to re-fetch.
		if got.LastPolledAt == nil || !got.LastPolledAt.Equal(*initialPolled) {
			t.Errorf("PatchSnapshot must not advance last_polled_at — initial=%v after=%v",
				initialPolled, got.LastPolledAt)
		}
	})

	t.Run("UpdateTitle_and_UpdateDescription_round_trip", func(t *testing.T) {
		s, orgID, _ := mk(t)

		ent, _, err := s.FindOrCreate(ctx, orgID, "jira", "SKY-100", "issue", "Old Title", "")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}

		if err := s.UpdateTitle(ctx, orgID, ent.ID, "New Title"); err != nil {
			t.Fatalf("UpdateTitle: %v", err)
		}
		if err := s.UpdateDescription(ctx, orgID, ent.ID, "Body paragraph"); err != nil {
			t.Fatalf("UpdateDescription: %v", err)
		}

		got, err := s.Get(ctx, orgID, ent.ID)
		if err != nil || got == nil {
			t.Fatalf("re-read: %v", err)
		}
		if got.Title != "New Title" {
			t.Errorf("title = %q, want New Title", got.Title)
		}
		if got.Description != "Body paragraph" {
			t.Errorf("description = %q, want Body paragraph", got.Description)
		}
	})

	t.Run("Close_only_fires_on_active", func(t *testing.T) {
		s, orgID, _ := mk(t)

		ent, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#close", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}

		if err := s.Close(ctx, orgID, ent.ID); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		got, _ := s.Get(ctx, orgID, ent.ID)
		if got.State != "closed" {
			t.Fatalf("state after Close = %q, want closed", got.State)
		}
		closedAt := got.ClosedAt

		// Close on an already-closed entity must be a no-op (the
		// state='active' guard skips the update).
		if err := s.Close(ctx, orgID, ent.ID); err != nil {
			t.Fatalf("second Close: %v", err)
		}
		again, _ := s.Get(ctx, orgID, ent.ID)
		if closedAt == nil || again.ClosedAt == nil || !again.ClosedAt.Equal(*closedAt) {
			t.Errorf("second Close should not advance closed_at — first=%v second=%v",
				closedAt, again.ClosedAt)
		}
	})

	t.Run("MarkClosed_is_unconditional", func(t *testing.T) {
		s, orgID, _ := mk(t)

		ent, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#mc", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := s.MarkClosed(ctx, orgID, ent.ID); err != nil {
			t.Fatalf("MarkClosed: %v", err)
		}
		got, _ := s.Get(ctx, orgID, ent.ID)
		if got.State != "closed" || got.ClosedAt == nil {
			t.Errorf("MarkClosed didn't terminal-flip — %+v", got)
		}
	})

	t.Run("Reactivate_only_fires_on_closed", func(t *testing.T) {
		s, orgID, _ := mk(t)

		ent, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#reac", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}

		// Active entity: Reactivate is a no-op.
		ok, err := s.Reactivate(ctx, orgID, ent.ID)
		if err != nil {
			t.Fatalf("Reactivate (active): %v", err)
		}
		if ok {
			t.Errorf("Reactivate on active entity should return ok=false")
		}

		// Close, then reactivate.
		if err := s.Close(ctx, orgID, ent.ID); err != nil {
			t.Fatalf("Close: %v", err)
		}
		ok, err = s.Reactivate(ctx, orgID, ent.ID)
		if err != nil {
			t.Fatalf("Reactivate (closed): %v", err)
		}
		if !ok {
			t.Errorf("Reactivate on closed entity should return ok=true")
		}
		got, _ := s.Get(ctx, orgID, ent.ID)
		if got.State != "active" || got.ClosedAt != nil {
			t.Errorf("Reactivate didn't restore state — %+v", got)
		}
	})

	t.Run("AssignProject_round_trips_and_returns_no_rows_on_missing", func(t *testing.T) {
		s, orgID, seed := mk(t)

		ent, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#ap", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}

		pid := seed.Project(t, "Roundtrip")
		if err := s.AssignProject(ctx, orgID, ent.ID, &pid, "winner because X"); err != nil {
			t.Fatalf("AssignProject: %v", err)
		}

		got, _ := s.Get(ctx, orgID, ent.ID)
		if got.ProjectID == nil || *got.ProjectID != pid {
			gotPID := "<nil>"
			if got.ProjectID != nil {
				gotPID = *got.ProjectID
			}
			t.Errorf("project_id = %s, want %s", gotPID, pid)
		}
		if got.ClassificationRationale != "winner because X" {
			t.Errorf("rationale = %q, want %q", got.ClassificationRationale, "winner because X")
		}

		// nil projectID stamps classified_at but clears the FK.
		if err := s.AssignProject(ctx, orgID, ent.ID, nil, ""); err != nil {
			t.Fatalf("AssignProject(nil): %v", err)
		}
		got, _ = s.Get(ctx, orgID, ent.ID)
		if got.ProjectID != nil {
			t.Errorf("project_id should be nil after AssignProject(nil), got %q", *got.ProjectID)
		}

		// Unknown id surfaces sql.ErrNoRows so the backfill handler can
		// report per-row failures. UUID-shape id so Postgres's uuid
		// column can bind.
		if err := s.AssignProject(ctx, orgID, uuid.New().String(), &pid, ""); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("AssignProject on missing entity: err = %v, want sql.ErrNoRows", err)
		}
	})

	// OwningTeamForEntitySystem resolves the structural owner (SKY-372 tiers
	// 1+2). A plain entity has no override and no project → empty; an entity
	// attached to a team-visibility project resolves to that project's team.
	// (The owning_team_id override tier needs a writer this ticket doesn't add,
	// so it's exercised at the router layer; here we cover the project tier and
	// the empty fall-through across both dialects.)
	t.Run("OwningTeamForEntity_resolves_project_team_else_empty", func(t *testing.T) {
		s, orgID, seed := mk(t)

		// No project, no override → empty (the router then falls to its
		// prior-task / author-identity tiers).
		plain, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#owner-plain", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed plain entity: %v", err)
		}
		if team, err := s.OwningTeamForEntitySystem(ctx, orgID, plain.ID); err != nil || team != "" {
			t.Errorf("plain entity: got (%q, %v), want (\"\", nil)", team, err)
		}

		// Attached to a team-visibility project → the project's team.
		ent, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#owner-proj", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		pid := seed.Project(t, "Owned")
		if err := s.AssignProject(ctx, orgID, ent.ID, &pid, ""); err != nil {
			t.Fatalf("AssignProject: %v", err)
		}
		team, err := s.OwningTeamForEntitySystem(ctx, orgID, ent.ID)
		if err != nil {
			t.Fatalf("OwningTeamForEntitySystem: %v", err)
		}
		if team == "" {
			t.Error("project-attached entity resolved no owning team; want the project's team")
		}

		// Missing entity → empty, not an error.
		if team, err := s.OwningTeamForEntitySystem(ctx, orgID, uuid.New().String()); err != nil || team != "" {
			t.Errorf("missing entity: got (%q, %v), want (\"\", nil)", team, err)
		}
	})

	t.Run("ClassificationStatusSystem_keys_on_classified_at", func(t *testing.T) {
		// SKY-392: the delegation wait reads classification state through
		// this dialect-aware store method (not a raw `?`-placeholder
		// query). Pins both the (classified, exists) contract and the
		// load-bearing detail that it keys on classified_at, NOT
		// project_id — so a below-threshold entity (stamped, but no
		// project) still reports classified and the wait can release.
		s, orgID, seed := mk(t)

		ent, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#cs", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}

		// Freshly discovered: classified_at IS NULL → not classified, but
		// the row exists.
		classified, exists, err := s.ClassificationStatusSystem(ctx, orgID, ent.ID)
		if err != nil {
			t.Fatalf("ClassificationStatusSystem(fresh): %v", err)
		}
		if classified {
			t.Errorf("fresh entity reported classified; classified_at should be NULL")
		}
		if !exists {
			t.Errorf("fresh entity reported missing; the row exists")
		}

		// Below-threshold classification: AssignProject(nil) stamps
		// classified_at while leaving project_id NULL. The wait keys on
		// classified_at, so this MUST report classified.
		if err := s.AssignProject(ctx, orgID, ent.ID, nil, ""); err != nil {
			t.Fatalf("AssignProject(nil): %v", err)
		}
		classified, exists, err = s.ClassificationStatusSystem(ctx, orgID, ent.ID)
		if err != nil {
			t.Fatalf("ClassificationStatusSystem(below-threshold): %v", err)
		}
		if !classified {
			t.Errorf("entity with classified_at set but project_id NULL reported unclassified; the wait keys on classified_at, not project_id")
		}
		if !exists {
			t.Errorf("classified entity reported missing")
		}

		// Above-threshold classification (real project FK) also reports
		// classified — sanity that the project_id path isn't special.
		pid := seed.Project(t, "CS")
		other, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#cs2", "pr", "T2", "")
		if err != nil {
			t.Fatalf("seed entity 2: %v", err)
		}
		if err := s.AssignProject(ctx, orgID, other.ID, &pid, "winner"); err != nil {
			t.Fatalf("AssignProject(pid): %v", err)
		}
		classified, _, err = s.ClassificationStatusSystem(ctx, orgID, other.ID)
		if err != nil {
			t.Fatalf("ClassificationStatusSystem(assigned): %v", err)
		}
		if !classified {
			t.Errorf("project-assigned entity reported unclassified")
		}

		// Unknown id is definitively (false, false, nil) — not an error —
		// so WaitFor stops polling a deleted/never-seen entity instead of
		// burning the full timeout. UUID-shape id so Postgres's uuid
		// column can bind.
		classified, exists, err = s.ClassificationStatusSystem(ctx, orgID, uuid.New().String())
		if err != nil {
			t.Fatalf("ClassificationStatusSystem(missing): %v", err)
		}
		if classified || exists {
			t.Errorf("missing entity: classified=%v exists=%v, want false/false", classified, exists)
		}
	})

	t.Run("ListUnclassified_excludes_assigned_and_closed", func(t *testing.T) {
		s, orgID, seed := mk(t)

		unassigned, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#u", "pr", "U", "")
		if err != nil {
			t.Fatalf("seed unassigned: %v", err)
		}
		assigned, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#a", "pr", "A", "")
		closed, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#c", "pr", "C", "")

		pid := seed.Project(t, "P")
		if err := s.AssignProject(ctx, orgID, assigned.ID, &pid, ""); err != nil {
			t.Fatalf("assign: %v", err)
		}
		if err := s.MarkClosed(ctx, orgID, closed.ID); err != nil {
			t.Fatalf("MarkClosed: %v", err)
		}

		got, err := s.ListUnclassified(ctx, orgID)
		if err != nil {
			t.Fatalf("ListUnclassified: %v", err)
		}
		ids := map[string]bool{}
		for _, e := range got {
			ids[e.ID] = true
		}
		if !ids[unassigned.ID] {
			t.Errorf("unassigned entity %s missing from result", unassigned.ID)
		}
		if ids[assigned.ID] {
			t.Errorf("assigned entity %s should be excluded", assigned.ID)
		}
		if ids[closed.ID] {
			t.Errorf("closed entity %s should be excluded", closed.ID)
		}
	})

	t.Run("ListActive_filters_by_source_and_state", func(t *testing.T) {
		s, orgID, _ := mk(t)

		gh, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#la-gh", "pr", "GH", "")
		ji, _, _ := s.FindOrCreate(ctx, orgID, "jira", "SKY-la-1", "issue", "JI", "")
		ghClosed, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#la-closed", "pr", "GC", "")
		if err := s.MarkClosed(ctx, orgID, ghClosed.ID); err != nil {
			t.Fatalf("close: %v", err)
		}

		gotGH, err := s.ListActive(ctx, orgID, "github")
		if err != nil {
			t.Fatalf("ListActive(github): %v", err)
		}
		ghIDs := map[string]bool{}
		for _, e := range gotGH {
			ghIDs[e.ID] = true
		}
		if !ghIDs[gh.ID] {
			t.Errorf("active github entity %s missing", gh.ID)
		}
		if ghIDs[ji.ID] {
			t.Errorf("jira entity %s leaked into github list", ji.ID)
		}
		if ghIDs[ghClosed.ID] {
			t.Errorf("closed github entity %s leaked into active list", ghClosed.ID)
		}
	})

	t.Run("ListProjectPanel_filters_by_project_and_active", func(t *testing.T) {
		s, orgID, seed := mk(t)

		pid := seed.Project(t, "Panel")
		assignedActive, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#pa", "pr", "Active", "")
		if err := s.AssignProject(ctx, orgID, assignedActive.ID, &pid, "r"); err != nil {
			t.Fatalf("assign active: %v", err)
		}
		assignedClosed, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#pc", "pr", "Closed", "")
		if err := s.AssignProject(ctx, orgID, assignedClosed.ID, &pid, ""); err != nil {
			t.Fatalf("assign closed: %v", err)
		}
		if err := s.MarkClosed(ctx, orgID, assignedClosed.ID); err != nil {
			t.Fatalf("close: %v", err)
		}
		other, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#po", "pr", "Other", "")
		otherPid := seed.Project(t, "Other")
		if err := s.AssignProject(ctx, orgID, other.ID, &otherPid, ""); err != nil {
			t.Fatalf("assign other: %v", err)
		}

		got, err := s.ListProjectPanel(ctx, orgID, pid)
		if err != nil {
			t.Fatalf("ListProjectPanel: %v", err)
		}
		ids := map[string]bool{}
		for _, e := range got {
			ids[e.ID] = true
		}
		if !ids[assignedActive.ID] {
			t.Errorf("active panel entity %s missing", assignedActive.ID)
		}
		if ids[assignedClosed.ID] {
			t.Errorf("closed entity %s leaked into panel", assignedClosed.ID)
		}
		if ids[other.ID] {
			t.Errorf("other-project entity %s leaked into panel", other.ID)
		}
	})

	t.Run("Descriptions_dedupes_and_skips_empty", func(t *testing.T) {
		s, orgID, _ := mk(t)

		withDesc, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#d1", "pr", "T", "")
		if err := s.UpdateDescription(ctx, orgID, withDesc.ID, "rich body"); err != nil {
			t.Fatalf("UpdateDescription: %v", err)
		}
		empty, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#d2", "pr", "T", "")

		missing := uuid.New().String()
		ids := []string{withDesc.ID, withDesc.ID, "", empty.ID, missing}
		got, err := s.Descriptions(ctx, orgID, ids)
		if err != nil {
			t.Fatalf("Descriptions: %v", err)
		}
		if got[withDesc.ID] != "rich body" {
			t.Errorf("description for %s = %q, want rich body", withDesc.ID, got[withDesc.ID])
		}
		if _, ok := got[empty.ID]; ok {
			t.Errorf("empty description should be omitted, got %q", got[empty.ID])
		}
		if _, ok := got[missing]; ok {
			t.Errorf("nonexistent id should be absent from result")
		}
	})

	t.Run("Descriptions_empty_input_returns_empty_map", func(t *testing.T) {
		s, orgID, _ := mk(t)
		got, err := s.Descriptions(ctx, orgID, nil)
		if err != nil {
			t.Fatalf("Descriptions(nil): %v", err)
		}
		if len(got) != 0 {
			t.Errorf("Descriptions(nil) = %v, want empty map", got)
		}
	})
}
