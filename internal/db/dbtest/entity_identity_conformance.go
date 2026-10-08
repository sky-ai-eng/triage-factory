package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EntityKeyFactory hands RunEntityKeyConformance an EntityStore and the org to
// call it with. It is a store and nothing else so a backend can run the suite
// through any wiring — the conformance pool, or a transaction carrying a
// user's claims under RLS.
type EntityKeyFactory func(t *testing.T) (store db.EntityStore, orgID string)

// RunEntityKeyConformance covers the key half of the entity identity model
// through the request-path methods (FindOrCreate, GetBySource):
//
//   - a key lookup returns the active row, otherwise the most recently closed;
//   - keys are unique among active rows only, so a closed row's key can be
//     taken by a new object, and an active row's cannot;
//   - a given external id wins over the key;
//   - the scope keeps two otherwise identical keys and ids apart;
//   - a create with no scope is refused, and a read with none matches nothing.
//
// Every subtest uses keys of its own, so a backend may hand the same store to
// all of them (one claims transaction for the whole suite).
func RunEntityKeyConformance(t *testing.T, mk EntityKeyFactory) {
	t.Helper()
	ctx := context.Background()
	const scope = "ws-a"

	t.Run("key_lookup_prefers_active_then_most_recently_closed", func(t *testing.T) {
		s, orgID := mk(t)
		first, _, err := s.FindOrCreate(ctx, orgID, "linear", scope, "KEY-1", "uuid-k1-a", "issue", "first", "")
		if err != nil {
			t.Fatalf("create first: %v", err)
		}
		if _, err := s.MarkClosed(ctx, orgID, first.ID); err != nil {
			t.Fatalf("close first: %v", err)
		}
		second, created, err := s.FindOrCreate(ctx, orgID, "linear", scope, "KEY-1", "uuid-k1-b", "issue", "second", "")
		if err != nil || !created {
			t.Fatalf("create second under the closed key: created=%v err=%v, want a new row", created, err)
		}
		got, err := s.GetBySource(ctx, orgID, "linear", scope, "KEY-1")
		if err != nil || got == nil || got.ID != second.ID {
			t.Fatalf("GetBySource = %+v err=%v, want the active row %s", got, err, second.ID)
		}
		if _, err := s.MarkClosed(ctx, orgID, second.ID); err != nil {
			t.Fatalf("close second: %v", err)
		}
		got, err = s.GetBySource(ctx, orgID, "linear", scope, "KEY-1")
		if err != nil || got == nil || got.ID != second.ID {
			t.Errorf("GetBySource with both closed = %+v err=%v, want the most recently closed %s", got, err, second.ID)
		}
	})

	t.Run("an_active_key_refuses_a_different_object", func(t *testing.T) {
		s, orgID := mk(t)
		holder, _, err := s.FindOrCreate(ctx, orgID, "linear", scope, "KEY-2", "uuid-k2-a", "issue", "holder", "")
		if err != nil {
			t.Fatalf("create holder: %v", err)
		}
		got, created, err := s.FindOrCreate(ctx, orgID, "linear", scope, "KEY-2", "uuid-k2-b", "issue", "newcomer", "")
		if !errors.Is(err, db.ErrEntityKeyOccupied) || got != nil || created {
			t.Fatalf("FindOrCreate under an occupied key = (%+v, %v, %v), want ErrEntityKeyOccupied", got, created, err)
		}
		// The refusal wrote nothing, and the caller's transaction is still
		// usable: the next statement on the same store succeeds.
		still, err := s.GetBySource(ctx, orgID, "linear", scope, "KEY-2")
		if err != nil || still == nil || still.ID != holder.ID || still.ExternalID != "uuid-k2-a" {
			t.Errorf("GetBySource after the refusal = %+v err=%v, want the holder untouched", still, err)
		}
	})

	t.Run("a_keyless_create_returns_the_row_under_the_key", func(t *testing.T) {
		s, orgID := mk(t)
		row, _, err := s.FindOrCreate(ctx, orgID, "linear", scope, "KEY-3", "uuid-k3", "issue", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		again, created, err := s.FindOrCreate(ctx, orgID, "linear", scope, "KEY-3", "", "issue", "t", "")
		if err != nil || created || again.ID != row.ID {
			t.Errorf("FindOrCreate with no id = (%+v, %v, %v), want the existing row", again, created, err)
		}
	})

	t.Run("a_given_id_wins_over_the_key", func(t *testing.T) {
		s, orgID := mk(t)
		row, _, err := s.FindOrCreate(ctx, orgID, "linear", scope, "KEY-4", "uuid-k4", "issue", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if row.ExternalID != "uuid-k4" || row.Scope != scope {
			t.Errorf("created row = %+v, want external_id and scope written at insert", row)
		}
		got, created, err := s.FindOrCreate(ctx, orgID, "linear", scope, "MOVED-4", "uuid-k4", "issue", "t", "")
		if err != nil || created || got.ID != row.ID || got.SourceID != "KEY-4" {
			t.Errorf("FindOrCreate(new key, same id) = (%+v, %v, %v), want the existing row under its stored key", got, created, err)
		}
	})

	t.Run("scope_separates_identical_keys_and_ids", func(t *testing.T) {
		s, orgID := mk(t)
		a, createdA, err := s.FindOrCreate(ctx, orgID, "linear", "ws-one", "KEY-5", "uuid-k5", "issue", "a", "")
		if err != nil || !createdA {
			t.Fatalf("create in ws-one: created=%v err=%v", createdA, err)
		}
		b, createdB, err := s.FindOrCreate(ctx, orgID, "linear", "ws-two", "KEY-5", "uuid-k5", "issue", "b", "")
		if err != nil || !createdB || b.ID == a.ID {
			t.Fatalf("create in ws-two = (%+v, %v, %v), want a second row", b, createdB, err)
		}
		gotA, _ := s.GetBySource(ctx, orgID, "linear", "ws-one", "KEY-5")
		gotB, _ := s.GetBySource(ctx, orgID, "linear", "ws-two", "KEY-5")
		if gotA == nil || gotB == nil || gotA.ID != a.ID || gotB.ID != b.ID {
			t.Errorf("GetBySource per scope = %+v / %+v, want each scope's own row", gotA, gotB)
		}
		if miss, _ := s.GetBySource(ctx, orgID, "linear", "ws-three", "KEY-5"); miss != nil {
			t.Errorf("GetBySource in a third scope = %+v, want nil", miss)
		}
	})

	t.Run("no_scope_creates_nothing_and_matches_nothing", func(t *testing.T) {
		s, orgID := mk(t)
		if _, _, err := s.FindOrCreate(ctx, orgID, "linear", "", "KEY-6", "uuid-k6", "issue", "t", ""); !errors.Is(err, db.ErrEntityScopeRequired) {
			t.Errorf("FindOrCreate with no scope: err=%v, want ErrEntityScopeRequired", err)
		}
		if got, err := s.GetBySource(ctx, orgID, "linear", "", "KEY-6"); err != nil || got != nil {
			t.Errorf("GetBySource with no scope = %+v err=%v, want (nil, nil)", got, err)
		}
	})
}

// EntityIdentityFactory is what a per-backend test file hands to
// RunEntityIdentityConformance: the whole store bundle, because a rename
// rewrites artifacts and the audit ledger beside the entity, plus the
// fixtures no store mints.
type EntityIdentityFactory func(t *testing.T) (stores db.Stores, orgID string, seed EntityIdentitySeeder)

// EntityIdentitySeeder is the bag of fixtures RunEntityIdentityConformance
// cannot build through a store interface.
type EntityIdentitySeeder struct {
	// TeamID owns the artifacts and actions.
	TeamID string

	// Task inserts a task against entityID and returns its id.
	Task func(t *testing.T, entityID, suffix string) string

	// RawActionURL reads one external_actions row's url and current_url as
	// stored, keyed by dedup_key; current_url is "" when NULL.
	RawActionURL func(t *testing.T, dedupKey string) (url, currentURL string)

	// ForceExternalID writes external_id on a row with the identity index out
	// of the way, so two rows can carry one id. Raw SQL because the schema
	// forbids exactly the state the ambiguity refusal exists for; the backend
	// restores the index when the test ends.
	ForceExternalID func(t *testing.T, entityID, externalID string)
}

// RunEntityIdentityConformance covers the system-only identity writes:
// GetByExternalIDSystem, StampExternalIDSystem and RenameSystem.
func RunEntityIdentityConformance(t *testing.T, mk EntityIdentityFactory) {
	t.Helper()
	ctx := context.Background()
	const scope = "ws-a"

	t.Run("GetByExternalIDSystem_finds_the_row_under_any_key", func(t *testing.T) {
		s, orgID, _ := mk(t)
		row, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-1", "uuid-1", "issue", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		got, err := s.Entities.GetByExternalIDSystem(ctx, orgID, "linear", scope, "uuid-1")
		if err != nil || got == nil || got.ID != row.ID {
			t.Fatalf("GetByExternalIDSystem = %+v err=%v, want %s", got, err, row.ID)
		}
		for _, miss := range []struct{ scope, id string }{{scope, "uuid-404"}, {"ws-b", "uuid-1"}, {scope, ""}} {
			if got, err := s.Entities.GetByExternalIDSystem(ctx, orgID, "linear", miss.scope, miss.id); err != nil || got != nil {
				t.Errorf("GetByExternalIDSystem(%s, %q) = %+v err=%v, want (nil, nil)", miss.scope, miss.id, got, err)
			}
		}
	})

	t.Run("StampExternalIDSystem_fills_a_null_once", func(t *testing.T) {
		s, orgID, _ := mk(t)
		row, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "jira", scope, "SKY-1", "", "issue", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		read := func() (*domain.Entity, error) { return s.Entities.GetSystem(ctx, orgID, row.ID) }
		stamped, err := s.Entities.StampExternalIDSystem(ctx, orgID, row.ID, "10001")
		if err != nil || stamped == nil || stamped.ExternalID != "10001" {
			t.Fatalf("stamp = %+v err=%v, want the row carrying 10001", stamped, err)
		}
		AssertWriteReturnedStoredRow(t, "StampExternalIDSystem", *stamped, read)

		again, err := s.Entities.StampExternalIDSystem(ctx, orgID, row.ID, "10001")
		if err != nil || again == nil || again.ID != row.ID {
			t.Errorf("repeat stamp = %+v err=%v, want the row back", again, err)
		}
		declined, err := s.Entities.StampExternalIDSystem(ctx, orgID, row.ID, "10002")
		if err != nil || declined != nil {
			t.Errorf("stamp of a different id = %+v err=%v, want (nil, nil)", declined, err)
		}
		if got, _ := read(); got == nil || got.ExternalID != "10001" {
			t.Errorf("after a declined stamp the row carries %+v, want 10001 kept", got)
		}

		other, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "jira", scope, "SKY-2", "", "issue", "t", "")
		if err != nil {
			t.Fatalf("create other: %v", err)
		}
		if got, err := s.Entities.StampExternalIDSystem(ctx, orgID, other.ID, "10001"); !errors.Is(err, db.ErrEntityIdentityAmbiguous) || got != nil {
			t.Errorf("stamping an id another row carries = %+v err=%v, want ErrEntityIdentityAmbiguous", got, err)
		}
		if _, err := s.Entities.StampExternalIDSystem(ctx, orgID, "00000000-0000-0000-0000-00000000dead", "10003"); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("stamp on a missing id: err=%v, want sql.ErrNoRows", err)
		}
	})

	t.Run("RenameSystem_rewrites_every_key_derived_value", func(t *testing.T) {
		s, orgID, seed := mk(t)
		const oldURL = "https://linear.app/acme/issue/ENG-4/fix-it"
		const newURL = "https://linear.app/acme/issue/OPS-77/fix-it"
		row, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-4", "uuid-4", "issue", "Fix it", oldURL)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, row.ID, `{"id":"uuid-4"}`, row.PollSeq); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		// The control: a key with the renamed one as a string prefix.
		neighbour, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-41", "uuid-41", "issue", "Neighbour",
			"https://linear.app/acme/issue/ENG-41/neighbour")
		if err != nil {
			t.Fatalf("create neighbour: %v", err)
		}
		// The other control: the same identifier in another workspace, whose
		// UUID has the renamed one as a string prefix.
		const otherURL = "https://linear.app/other/issue/ENG-4/fix-it"
		if _, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", "ws-b", "ENG-4", "uuid-4b", "issue", "Other", otherURL); err != nil {
			t.Fatalf("create the other workspace's ENG-4: %v", err)
		}
		taskID := seed.Task(t, row.ID, "rename")
		before, _ := s.Entities.GetSystem(ctx, orgID, row.ID)

		issueArt := upsertIdentityArtifact(t, s, orgID, seed.TeamID, domain.ArtifactKindIssue, "uuid-4", "ENG-4", "")
		commentArt := upsertIdentityArtifact(t, s, orgID, seed.TeamID, domain.ArtifactKindComment, "uuid-4", "ENG-4", "c-1")
		neighbourArt := upsertIdentityArtifact(t, s, orgID, seed.TeamID, domain.ArtifactKindIssue, "uuid-41", "ENG-41", "")
		otherArt := upsertIdentityArtifact(t, s, orgID, seed.TeamID, domain.ArtifactKindIssue, "uuid-4b", "ENG-4", "")
		recordIdentityAction(t, s, orgID, seed.TeamID, "ENG-4", oldURL+"#comment-c1", "rename-moved")
		recordIdentityAction(t, s, orgID, seed.TeamID, "ENG-41", "https://linear.app/acme/issue/ENG-41/neighbour", "rename-neighbour")
		recordIdentityAction(t, s, orgID, seed.TeamID, "ENG-4", otherURL, "rename-other")

		out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-4", "OPS-77", newURL)
		if err != nil {
			t.Fatalf("RenameSystem: %v", err)
		}
		if !out.Renamed || out.EntityID != row.ID || out.From != "ENG-4" || out.To != "OPS-77" {
			t.Fatalf("outcome = %+v, want ENG-4 -> OPS-77 on %s", out, row.ID)
		}

		moved, _ := s.Entities.GetSystem(ctx, orgID, row.ID)
		if moved.SourceID != "OPS-77" || moved.URL != newURL || moved.ExternalID != "uuid-4" {
			t.Errorf("entity = %+v, want key and url moved, id kept", moved)
		}
		if moved.SnapshotJSON != before.SnapshotJSON || moved.PollSeq != before.PollSeq {
			t.Errorf("rename moved the snapshot or poll_seq: before %+v after %+v", before, moved)
		}
		if old, _ := s.Entities.GetBySourceSystem(ctx, orgID, "linear", scope, "ENG-4"); old != nil {
			t.Errorf("the old key still resolves: %+v", old)
		}
		if task, err := s.Tasks.GetSystem(ctx, orgID, taskID); err != nil || task == nil || task.EntityID != row.ID {
			t.Errorf("task = %+v err=%v, want it still on the entity", task, err)
		}

		// The key carries the UUID, so only the target moves.
		for _, c := range []struct {
			id, wantTarget, wantKey string
		}{
			{issueArt, "OPS-77", "linear:issue:uuid-4"},
			{commentArt, "OPS-77", "linear:comment:uuid-4:c-1"},
			{neighbourArt, "ENG-41", "linear:issue:uuid-41"},
			{otherArt, "ENG-4", "linear:issue:uuid-4b"},
		} {
			got, err := s.Artifacts.Get(ctx, orgID, c.id)
			if err != nil || got == nil {
				t.Fatalf("artifact %s: %v", c.id, err)
			}
			if got.Target != c.wantTarget || got.DedupKey != c.wantKey {
				t.Errorf("artifact = target %q key %q, want %q / %q", got.Target, got.DedupKey, c.wantTarget, c.wantKey)
			}
		}

		if u, cur := seed.RawActionURL(t, "rename-moved"); u != oldURL+"#comment-c1" || cur != newURL+"#comment-c1" {
			t.Errorf("moved action url=%q current_url=%q, want the record kept and the pointer moved", u, cur)
		}
		for _, key := range []string{"rename-neighbour", "rename-other"} {
			if _, cur := seed.RawActionURL(t, key); cur != "" {
				t.Errorf("%s current_url = %q, want untouched", key, cur)
			}
		}
		if n, _ := s.Entities.GetSystem(ctx, orgID, neighbour.ID); n.SourceID != "ENG-41" {
			t.Errorf("neighbour key = %q, want ENG-41", n.SourceID)
		}
	})

	t.Run("RenameSystem_moves_a_non_ascii_link", func(t *testing.T) {
		s, orgID, seed := mk(t)
		const oldURL = "https://linear.app/acme/issue/ENG-9/café-menu"
		const newURL = "https://linear.app/acme/issue/OPS-9/café-menu"
		if _, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-9", "uuid-9", "issue", "Café", oldURL); err != nil {
			t.Fatalf("create: %v", err)
		}
		recordIdentityAction(t, s, orgID, seed.TeamID, "ENG-9", oldURL+"#comment-c9", "rename-non-ascii")
		if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-9", "OPS-9", newURL); err != nil || !out.Renamed {
			t.Fatalf("rename = %+v err=%v", out, err)
		}
		if _, cur := seed.RawActionURL(t, "rename-non-ascii"); cur != newURL+"#comment-c9" {
			t.Errorf("current_url = %q, want the pointer moved", cur)
		}
	})

	t.Run("RekeyOrMergeSystem_merges_only_into_an_active_holder", func(t *testing.T) {
		s, orgID, seed := mk(t)
		const jiraScope = "https://jira.example.com"
		create := func(key, title string) *domain.Entity {
			t.Helper()
			e, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "jira", jiraScope, key, "", "issue", title, "")
			if err != nil {
				t.Fatalf("create %s: %v", key, err)
			}
			return e
		}

		// A closed row under the new key: the moving entity takes the key in
		// place, and the closed row keeps its own history.
		closed := create("OPS-9", "Old OPS-9")
		if _, err := s.Entities.MarkClosed(ctx, orgID, closed.ID); err != nil {
			t.Fatalf("close: %v", err)
		}
		moving := create("ENG-5", "Moving")
		taskID := seed.Task(t, moving.ID, "rekey")
		survivor, merged, err := s.Entities.RekeyOrMergeSystem(ctx, orgID, moving.ID, "OPS-9")
		if err != nil || merged || survivor != moving.ID {
			t.Fatalf("rekey beside a closed holder = (%s, merged=%v, %v), want %s rekeyed in place", survivor, merged, err, moving.ID)
		}
		if got, _ := s.Entities.GetSystem(ctx, orgID, moving.ID); got == nil || got.SourceID != "OPS-9" || got.State != "active" {
			t.Errorf("moving entity = %+v, want active under OPS-9", got)
		}
		if got, _ := s.Entities.GetSystem(ctx, orgID, closed.ID); got == nil || got.State != "closed" || got.Title != "Old OPS-9" {
			t.Errorf("closed holder = %+v, want it untouched", got)
		}
		if task, err := s.Tasks.GetSystem(ctx, orgID, taskID); err != nil || task == nil || task.EntityID != moving.ID {
			t.Errorf("task = %+v err=%v, want it still on the moving entity", task, err)
		}
		if got, _ := s.Entities.GetBySourceSystem(ctx, orgID, "jira", jiraScope, "OPS-9"); got == nil || got.ID != moving.ID {
			t.Errorf("OPS-9 resolves to %+v, want the active row", got)
		}

		// An active row under the new key is the same object found again under
		// it: the moving entity merges into it.
		holder := create("OPS-10", "Duplicate")
		loser := create("ENG-6", "Moving again")
		survivor, merged, err = s.Entities.RekeyOrMergeSystem(ctx, orgID, loser.ID, "OPS-10")
		if err != nil || !merged || survivor != holder.ID {
			t.Fatalf("rekey onto an active holder = (%s, merged=%v, %v), want a merge into %s", survivor, merged, err, holder.ID)
		}
		if got, _ := s.Entities.GetSystem(ctx, orgID, loser.ID); got != nil {
			t.Errorf("merged-away entity still exists: %+v", got)
		}
	})

	t.Run("RenameSystem_is_idempotent", func(t *testing.T) {
		s, orgID, _ := mk(t)
		if _, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-5", "uuid-5", "issue", "t", ""); err != nil {
			t.Fatalf("create: %v", err)
		}
		if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-5", "OPS-5", ""); err != nil || !out.Renamed {
			t.Fatalf("first rename = %+v err=%v", out, err)
		}
		if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-5", "OPS-5", ""); err != nil || out.Renamed {
			t.Errorf("second rename = %+v err=%v, want a no-op", out, err)
		}
		for _, tc := range []struct{ name, scope, id string }{
			{"no id", scope, ""},
			{"no row carries the id", scope, "uuid-404"},
			{"another scope", "ws-b", "uuid-5"},
		} {
			if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", tc.scope, tc.id, "OPS-9", ""); err != nil || out.Renamed {
				t.Errorf("%s: rename = %+v err=%v, want a no-op", tc.name, out, err)
			}
		}
		if _, err := s.Entities.RenameSystem(ctx, orgID, "linear", "", "uuid-5", "OPS-9", ""); !errors.Is(err, db.ErrEntityScopeRequired) {
			t.Errorf("rename with no scope: err=%v, want ErrEntityScopeRequired", err)
		}
	})

	t.Run("RenameSystem_refuses_a_key_an_active_row_holds", func(t *testing.T) {
		s, orgID, _ := mk(t)
		row, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-6", "uuid-6", "issue", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		holder, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "OPS-6", "uuid-other", "issue", "t", "")
		if err != nil {
			t.Fatalf("create holder: %v", err)
		}
		if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-6", "OPS-6", ""); !errors.Is(err, db.ErrEntityKeyOccupied) || out.Renamed {
			t.Fatalf("rename onto an active key = %+v err=%v, want ErrEntityKeyOccupied", out, err)
		}
		if got, _ := s.Entities.GetSystem(ctx, orgID, row.ID); got.SourceID != "ENG-6" {
			t.Errorf("refused rename moved the key to %q", got.SourceID)
		}
		// A closed holder does not occupy the key.
		if _, err := s.Entities.MarkClosed(ctx, orgID, holder.ID); err != nil {
			t.Fatalf("close holder: %v", err)
		}
		if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-6", "OPS-6", ""); err != nil || !out.Renamed {
			t.Errorf("rename onto a closed key = %+v err=%v, want a rename", out, err)
		}
	})

	t.Run("RenameSystem_refuses_an_ambiguous_id", func(t *testing.T) {
		s, orgID, seed := mk(t)
		a, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-7", "uuid-7", "issue", "t", "")
		if err != nil {
			t.Fatalf("create a: %v", err)
		}
		b, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-8", "", "issue", "t", "")
		if err != nil {
			t.Fatalf("create b: %v", err)
		}
		seed.ForceExternalID(t, b.ID, "uuid-7")
		if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-7", "OPS-7", ""); !errors.Is(err, db.ErrEntityIdentityAmbiguous) || out.Renamed {
			t.Fatalf("rename of an id two rows carry = %+v err=%v, want ErrEntityIdentityAmbiguous", out, err)
		}
		for _, id := range []string{a.ID, b.ID} {
			if got, _ := s.Entities.GetSystem(ctx, orgID, id); got.SourceID == "OPS-7" {
				t.Errorf("refused rename moved %s", id)
			}
		}
	})
}

// upsertIdentityArtifact records a Linear artifact the way domain.ArtifactDedupKey
// says to key one: on the issue's UUID, with the identifier as its target.
func upsertIdentityArtifact(t *testing.T, s db.Stores, orgID, teamID, kind, issueID, key, anchor string) string {
	t.Helper()
	a, err := s.Artifacts.UpsertSystem(context.Background(), orgID, domain.Artifact{
		TeamID: teamID, Provider: domain.ArtifactProviderLinear, Kind: kind,
		Target: key, ExternalID: issueID + anchor, State: identityArtifactState(kind),
		DedupKey: domain.ArtifactDedupKey(domain.ArtifactProviderLinear, kind, issueID, anchor),
	})
	if err != nil {
		t.Fatalf("seed %s artifact on %s: %v", kind, key, err)
	}
	return a.ID
}

func identityArtifactState(kind string) string {
	if kind == domain.ArtifactKindComment {
		return domain.ArtifactStateCommentPosted
	}
	return domain.ArtifactStateIssueUpdated
}

func recordIdentityAction(t *testing.T, s db.Stores, orgID, teamID, key, url, dedupKey string) {
	t.Helper()
	if err := s.ExternalActions.RecordSystem(context.Background(), orgID, domain.ExternalAction{
		TeamID: teamID, Provider: domain.ArtifactProviderLinear, Action: domain.ActionIssueCommentPosted,
		Target: key, URL: url, Credential: domain.CredentialNone, DedupKey: dedupKey,
	}); err != nil {
		t.Fatalf("seed action %s: %v", dedupKey, err)
	}
}
