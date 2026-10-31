package dbtest

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
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
// all of them (one claims transaction for the whole suite). The rows are
// GitHub rows, the one source this door creates rows of without a container
// the caller has to supply (a GitHub row's repository comes from its key).
func RunEntityKeyConformance(t *testing.T, mk EntityKeyFactory) {
	t.Helper()
	ctx := context.Background()
	const scope = "ws-a"

	t.Run("key_lookup_prefers_active_then_most_recently_closed", func(t *testing.T) {
		s, orgID := mk(t)
		first, _, err := s.FindOrCreate(ctx, orgID, "github", scope, "acme/key#1", "uuid-k1-a", "pr", "first", "")
		if err != nil {
			t.Fatalf("create first: %v", err)
		}
		if _, err := s.MarkClosed(ctx, orgID, first.ID); err != nil {
			t.Fatalf("close first: %v", err)
		}
		second, created, err := s.FindOrCreate(ctx, orgID, "github", scope, "acme/key#1", "uuid-k1-b", "pr", "second", "")
		if err != nil || !created {
			t.Fatalf("create second under the closed key: created=%v err=%v, want a new row", created, err)
		}
		got, err := s.GetBySource(ctx, orgID, "github", scope, "acme/key#1")
		if err != nil || got == nil || got.ID != second.ID {
			t.Fatalf("GetBySource = %+v err=%v, want the active row %s", got, err, second.ID)
		}
		if _, err := s.MarkClosed(ctx, orgID, second.ID); err != nil {
			t.Fatalf("close second: %v", err)
		}
		got, err = s.GetBySource(ctx, orgID, "github", scope, "acme/key#1")
		if err != nil || got == nil || got.ID != second.ID {
			t.Errorf("GetBySource with both closed = %+v err=%v, want the most recently closed %s", got, err, second.ID)
		}
	})

	t.Run("an_active_key_refuses_a_different_object", func(t *testing.T) {
		s, orgID := mk(t)
		holder, _, err := s.FindOrCreate(ctx, orgID, "github", scope, "acme/key#2", "uuid-k2-a", "pr", "holder", "")
		if err != nil {
			t.Fatalf("create holder: %v", err)
		}
		got, created, err := s.FindOrCreate(ctx, orgID, "github", scope, "acme/key#2", "uuid-k2-b", "pr", "newcomer", "")
		if !errors.Is(err, db.ErrEntityKeyOccupied) || got != nil || created {
			t.Fatalf("FindOrCreate under an occupied key = (%+v, %v, %v), want ErrEntityKeyOccupied", got, created, err)
		}
		// The refusal wrote nothing, and the caller's transaction is still
		// usable: the next statement on the same store succeeds.
		still, err := s.GetBySource(ctx, orgID, "github", scope, "acme/key#2")
		if err != nil || still == nil || still.ID != holder.ID || still.ExternalID != "uuid-k2-a" {
			t.Errorf("GetBySource after the refusal = %+v err=%v, want the holder untouched", still, err)
		}
	})

	t.Run("a_keyless_create_returns_the_row_under_the_key", func(t *testing.T) {
		s, orgID := mk(t)
		row, _, err := s.FindOrCreate(ctx, orgID, "github", scope, "acme/key#3", "uuid-k3", "pr", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		again, created, err := s.FindOrCreate(ctx, orgID, "github", scope, "acme/key#3", "", "pr", "t", "")
		if err != nil || created || again.ID != row.ID {
			t.Errorf("FindOrCreate with no id = (%+v, %v, %v), want the existing row", again, created, err)
		}
	})

	t.Run("a_given_id_wins_over_the_key", func(t *testing.T) {
		s, orgID := mk(t)
		row, _, err := s.FindOrCreate(ctx, orgID, "github", scope, "acme/key#4", "uuid-k4", "pr", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if row.ExternalID != "uuid-k4" || row.Scope != scope {
			t.Errorf("created row = %+v, want external_id and scope written at insert", row)
		}
		got, created, err := s.FindOrCreate(ctx, orgID, "github", scope, "acme/moved#4", "uuid-k4", "pr", "t", "")
		if err != nil || created || got.ID != row.ID || got.SourceID != "acme/key#4" {
			t.Errorf("FindOrCreate(new key, same id) = (%+v, %v, %v), want the existing row under its stored key", got, created, err)
		}
	})

	t.Run("scope_separates_identical_keys_and_ids", func(t *testing.T) {
		s, orgID := mk(t)
		a, createdA, err := s.FindOrCreate(ctx, orgID, "github", "ws-one", "acme/key#5", "uuid-k5", "pr", "a", "")
		if err != nil || !createdA {
			t.Fatalf("create in ws-one: created=%v err=%v", createdA, err)
		}
		b, createdB, err := s.FindOrCreate(ctx, orgID, "github", "ws-two", "acme/key#5", "uuid-k5", "pr", "b", "")
		if err != nil || !createdB || b.ID == a.ID {
			t.Fatalf("create in ws-two = (%+v, %v, %v), want a second row", b, createdB, err)
		}
		gotA, _ := s.GetBySource(ctx, orgID, "github", "ws-one", "acme/key#5")
		gotB, _ := s.GetBySource(ctx, orgID, "github", "ws-two", "acme/key#5")
		if gotA == nil || gotB == nil || gotA.ID != a.ID || gotB.ID != b.ID {
			t.Errorf("GetBySource per scope = %+v / %+v, want each scope's own row", gotA, gotB)
		}
		if miss, _ := s.GetBySource(ctx, orgID, "github", "ws-three", "acme/key#5"); miss != nil {
			t.Errorf("GetBySource in a third scope = %+v, want nil", miss)
		}
	})

	t.Run("no_scope_creates_nothing_and_matches_nothing", func(t *testing.T) {
		s, orgID := mk(t)
		if _, _, err := s.FindOrCreate(ctx, orgID, "github", "", "acme/key#6", "uuid-k6", "pr", "t", ""); !errors.Is(err, db.ErrEntityScopeRequired) {
			t.Errorf("FindOrCreate with no scope: err=%v, want ErrEntityScopeRequired", err)
		}
		if got, err := s.GetBySource(ctx, orgID, "github", "", "acme/key#6"); err != nil || got != nil {
			t.Errorf("GetBySource with no scope = %+v err=%v, want (nil, nil)", got, err)
		}
	})
}

// EntityIdentityFactory is what a per-backend test file hands to
// RunEntityIdentityConformance: the whole store bundle, because a rename
// rewrites deliverables and the audit ledger beside the entity, plus the
// fixtures no store mints.
type EntityIdentityFactory func(t *testing.T) (stores db.Stores, orgID string, seed EntityIdentitySeeder)

// EntityIdentitySeeder is the bag of fixtures RunEntityIdentityConformance
// cannot build through a store interface.
type EntityIdentitySeeder struct {
	// TeamID owns the deliverables and actions.
	TeamID string

	// UserID is a member of the org, for an entity's commissioning user.
	UserID string

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

	// ForgetExternalID clears external_id on a Jira row with the schema's
	// requirement out of the way: the shape of a local row the SQLite
	// migration carried over from before the poller recorded issue ids, which
	// the duplicate merge exists to fold. Raw SQL because no write may produce
	// it; the backend restores the requirement when the test ends.
	ForgetExternalID func(t *testing.T, entityID string)

	// Conversation inserts a conversation, on a task and entity of its own,
	// and returns its id, for memory links to name.
	Conversation func(t *testing.T, suffix string) string

	// Link inserts an entity_links row from one entity to another.
	Link func(t *testing.T, fromEntityID, toEntityID string)

	// Referents reads, as stored, every row that names entityID: a count per
	// referencing table, and its memory links' roles by conversation. Raw SQL
	// because no store reads several of these tables by entity.
	Referents func(t *testing.T, entityID string) EntityReferents
}

// EntityReferents is what EntityIdentitySeeder.Referents reads for one entity.
type EntityReferents struct {
	// Rows counts the rows naming the entity in tasks, events, event_queue,
	// pending_firings, conversation_memory_entities and entity_links (either
	// end), keyed by table.
	Rows map[string]int
	// MemoryRoles is the role of each conversation_memory_entities row on the
	// entity, keyed by conversation id.
	MemoryRoles map[string]string
}

// RunEntityIdentityConformance covers the system-only identity writes:
// GetByExternalIDSystem, StampExternalIDSystem, RenameSystem,
// MergeDuplicateEntitiesSystem and FoldStubEntitySystem.
func RunEntityIdentityConformance(t *testing.T, mk EntityIdentityFactory) {
	t.Helper()
	ctx := context.Background()
	const scope = "ws-a"

	t.Run("GetByExternalIDSystem_finds_the_row_under_any_key", func(t *testing.T) {
		s, orgID, _ := mk(t)
		row, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-1", "uuid-1", "team-eng", "t", "")
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

	// A GitHub stub, created from a key alone, is the row with no id that
	// either backend holds: a Jira or Linear row is created with its id.
	t.Run("StampExternalIDSystem_fills_a_null_once", func(t *testing.T) {
		s, orgID, _ := mk(t)
		row, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", scope, "acme/stamp#1", "", "pr", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		read := func() (*domain.Entity, error) { return s.Entities.GetSystem(ctx, orgID, row.ID) }
		stamped, err := s.Entities.StampExternalIDSystem(ctx, orgID, row.ID, "PR_1")
		if err != nil || stamped == nil || stamped.ExternalID != "PR_1" {
			t.Fatalf("stamp = %+v err=%v, want the row carrying PR_1", stamped, err)
		}
		AssertWriteReturnedStoredRow(t, "StampExternalIDSystem", *stamped, read)

		again, err := s.Entities.StampExternalIDSystem(ctx, orgID, row.ID, "PR_1")
		if err != nil || again == nil || again.ID != row.ID {
			t.Errorf("repeat stamp = %+v err=%v, want the row back", again, err)
		}
		declined, err := s.Entities.StampExternalIDSystem(ctx, orgID, row.ID, "PR_2")
		if err != nil || declined != nil {
			t.Errorf("stamp of a different id = %+v err=%v, want (nil, nil)", declined, err)
		}
		if got, _ := read(); got == nil || got.ExternalID != "PR_1" {
			t.Errorf("after a declined stamp the row carries %+v, want PR_1 kept", got)
		}

		other, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", scope, "acme/stamp#2", "", "pr", "t", "")
		if err != nil {
			t.Fatalf("create other: %v", err)
		}
		if got, err := s.Entities.StampExternalIDSystem(ctx, orgID, other.ID, "PR_1"); !errors.Is(err, db.ErrEntityIdentityAmbiguous) || got != nil {
			t.Errorf("stamping an id another row carries = %+v err=%v, want ErrEntityIdentityAmbiguous", got, err)
		}
		if _, err := s.Entities.StampExternalIDSystem(ctx, orgID, "00000000-0000-0000-0000-00000000dead", "PR_3"); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("stamp on a missing id: err=%v, want sql.ErrNoRows", err)
		}
	})

	t.Run("RenameSystem_rewrites_every_key_derived_value", func(t *testing.T) {
		s, orgID, seed := mk(t)
		const oldURL = "https://linear.app/acme/issue/ENG-4/fix-it"
		const newURL = "https://linear.app/acme/issue/OPS-77/fix-it"
		row, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-4", "uuid-4", "team-eng", "Fix it", oldURL)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, row.ID, `{"id":"uuid-4"}`, row.PollSeq); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		// The control: a key with the renamed one as a string prefix.
		neighbour, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-41", "uuid-41", "team-eng", "Neighbour",
			"https://linear.app/acme/issue/ENG-41/neighbour")

		if err != nil {
			t.Fatalf("create neighbour: %v", err)
		}
		// The other control: the same identifier in another workspace, whose
		// UUID has the renamed one as a string prefix.
		const otherURL = "https://linear.app/other/issue/ENG-4/fix-it"
		if _, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, "ws-b", "ENG-4", "uuid-4b", "team-eng", "Other", otherURL); err != nil {
			t.Fatalf("create the other workspace's ENG-4: %v", err)
		}
		taskID := seed.Task(t, row.ID, "rename")
		before, _ := s.Entities.GetSystem(ctx, orgID, row.ID)

		issueArt := upsertIdentityDeliverable(t, s, orgID, seed.TeamID, scope, domain.DeliverableKindIssue, "uuid-4", "ENG-4", "")
		commentArt := upsertIdentityDeliverable(t, s, orgID, seed.TeamID, scope, domain.DeliverableKindComment, "uuid-4", "ENG-4", "c-1")
		neighbourArt := upsertIdentityDeliverable(t, s, orgID, seed.TeamID, scope, domain.DeliverableKindIssue, "uuid-41", "ENG-41", "")
		otherArt := upsertIdentityDeliverable(t, s, orgID, seed.TeamID, "ws-b", domain.DeliverableKindIssue, "uuid-4b", "ENG-4", "")
		// The scope control: the renamed issue's own dedup key, target and link,
		// recorded in another workspace's scope. Only the scope separates it,
		// so it is a second row and the rename leaves it alone.
		foreignArt := upsertIdentityDeliverable(t, s, orgID, seed.TeamID, "ws-b", domain.DeliverableKindIssue, "uuid-4", "ENG-4", "")
		if foreignArt == issueArt {
			t.Fatalf("the ws-b deliverable landed on the ws-a row: the same dedup key in two scopes is two rows")
		}
		otherRow, err := s.Entities.GetBySourceSystem(ctx, orgID, "linear", "ws-b", "ENG-4")
		if err != nil || otherRow == nil {
			t.Fatalf("read the other workspace's ENG-4: %v, %v", otherRow, err)
		}
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderLinear, scope, row.ID, "ENG-4", oldURL+"#comment-c1", "rename-moved")
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderLinear, scope, neighbour.ID, "ENG-41", "https://linear.app/acme/issue/ENG-41/neighbour", "rename-neighbour")
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderLinear, "ws-b", otherRow.ID, "ENG-4", otherURL, "rename-other")
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderLinear, "ws-b", "", "ENG-4", oldURL+"#comment-c1", "rename-foreign")
		// The rows are selected by entity, never by link: an act on the
		// renamed issue's key recorded about no entity, and an act about the
		// neighbour whose link spells the renamed issue's, both keep theirs.
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderLinear, scope, "", "ENG-4", oldURL+"#comment-c2", "rename-unlinked")
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderLinear, scope, neighbour.ID, "ENG-41", oldURL+"#comment-c3", "rename-elsewhere")

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
		if moved.SnapshotJSON != before.SnapshotJSON {
			t.Errorf("rename moved the snapshot: before %+v after %+v", before, moved)
		}
		// A rename is a new version of the row: a cycle that read it before
		// must miss its snapshot CAS, and the renaming caller CASes on the
		// version the outcome hands back.
		if moved.PollSeq != before.PollSeq+1 || out.PollSeq != moved.PollSeq {
			t.Errorf("poll_seq = %d (outcome %d), want %d: the rename bumps it once and returns it", moved.PollSeq, out.PollSeq, before.PollSeq+1)
		}
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, row.ID, `{"id":"uuid-4","stale":true}`, before.PollSeq); err != nil || ok {
			t.Errorf("a snapshot CAS on the version read before the rename = ok %v err %v, want a miss", ok, err)
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
			{foreignArt, "ENG-4", "linear:issue:uuid-4"},
		} {
			got, err := s.Deliverables.Get(ctx, orgID, c.id)
			if err != nil || got == nil {
				t.Fatalf("deliverable %s: %v", c.id, err)
			}
			if got.Target != c.wantTarget || got.DedupKey != c.wantKey {
				t.Errorf("deliverable = target %q key %q, want %q / %q", got.Target, got.DedupKey, c.wantTarget, c.wantKey)
			}
		}

		if u, cur := seed.RawActionURL(t, "rename-moved"); u != oldURL+"#comment-c1" || cur != newURL+"#comment-c1" {
			t.Errorf("moved action url=%q current_url=%q, want the record kept and the pointer moved", u, cur)
		}
		for _, key := range []string{"rename-neighbour", "rename-other", "rename-foreign", "rename-unlinked", "rename-elsewhere"} {
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
		row, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-9", "uuid-9", "team-eng", "Café", oldURL)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderLinear, scope, row.ID, "ENG-9", oldURL+"#comment-c9", "rename-non-ascii")
		if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-9", "OPS-9", newURL); err != nil || !out.Renamed {
			t.Fatalf("rename = %+v err=%v", out, err)
		}
		if _, cur := seed.RawActionURL(t, "rename-non-ascii"); cur != newURL+"#comment-c9" {
			t.Errorf("current_url = %q, want the pointer moved", cur)
		}
	})

	t.Run("RenameSystem_moves_the_renamed_jira_entitys_deliverables", func(t *testing.T) {
		s, orgID, seed := mk(t)
		const site = "https://acme.atlassian.net"
		const otherSite = "https://other.atlassian.net"
		row, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, site, "ENG-5", "10005", TestJiraProject, "t", site+"/browse/ENG-5")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		// The same key and id on another site: a different issue.
		if _, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, otherSite, "ENG-5", "10005", TestJiraProject, "t", otherSite+"/browse/ENG-5"); err != nil {
			t.Fatalf("create the other site's ENG-5: %v", err)
		}
		issueArt := upsertJiraIdentityDeliverable(t, s, orgID, seed.TeamID, domain.DeliverableKindIssue, site, "10005", "ENG-5", "")
		commentArt := upsertJiraIdentityDeliverable(t, s, orgID, seed.TeamID, domain.DeliverableKindComment, site, "10005", "ENG-5", "20001")
		otherArt := upsertJiraIdentityDeliverable(t, s, orgID, seed.TeamID, domain.DeliverableKindIssue, otherSite, "10005", "ENG-5", "")
		// A deliverable linked to no entity is left alone, whatever its target
		// spells.
		legacy, err := s.Deliverables.UpsertSystem(ctx, orgID, domain.Deliverable{
			TeamID: seed.TeamID, Provider: domain.ProviderJira, Kind: domain.DeliverableKindIssue, Scope: site,
			Target: "ENG-5", ExternalID: "ENG-5", State: domain.DeliverableStateIssueUpdated,
			DedupKey: domain.ObjectKey(domain.ProviderJira, domain.DeliverableKindIssue, "ENG-5", ""),
		})
		if err != nil {
			t.Fatalf("seed key-shaped deliverable: %v", err)
		}

		out, err := s.Entities.RenameSystem(ctx, orgID, "jira", site, "10005", "OPS-9", site+"/browse/OPS-9")
		if err != nil || !out.Renamed || out.EntityID != row.ID {
			t.Fatalf("rename = %+v err=%v", out, err)
		}
		for _, c := range []struct{ id, wantTarget string }{
			{issueArt, "OPS-9"},
			{commentArt, "OPS-9"},
			{otherArt, "ENG-5"},
			{legacy.ID, "ENG-5"},
		} {
			got, err := s.Deliverables.Get(ctx, orgID, c.id)
			if err != nil || got == nil {
				t.Fatalf("deliverable %s: %v", c.id, err)
			}
			if got.Target != c.wantTarget {
				t.Errorf("deliverable %s target = %q, want %q", got.DedupKey, got.Target, c.wantTarget)
			}
		}
	})

	t.Run("MergeDuplicateEntitiesSystem_folds_the_newer_row_into_the_older", func(t *testing.T) {
		s, orgID, seed := mk(t)
		const jiraScope = "https://jira.example.com"
		// The older row: the issue's first entity, closed under the key it
		// had before an earlier move, carried over from before ids were
		// recorded.
		older, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, jiraScope, "OLD-1", "10001", TestJiraProject, "Old title", jiraScope+"/browse/OLD-1")
		if err != nil {
			t.Fatalf("create older: %v", err)
		}
		seed.ForgetExternalID(t, older.ID)
		older, _ = s.Entities.GetSystem(ctx, orgID, older.ID)
		olderTask := seed.Task(t, older.ID, "dup")
		if _, err := s.Entities.MarkClosed(ctx, orgID, older.ID); err != nil {
			t.Fatalf("close older: %v", err)
		}
		// The newer row: the same issue found again under its current key,
		// active, carrying the id.
		newer, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, jiraScope, "NEW-1", "10001", TestJiraProject, "New title", jiraScope+"/browse/NEW-1")
		if err != nil {
			t.Fatalf("create newer: %v", err)
		}
		const snap = `{"id":"10001","key":"NEW-1"}`
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, newer.ID, snap, newer.PollSeq); err != nil || !ok {
			t.Fatalf("seed newer snapshot: ok=%v err=%v", ok, err)
		}
		newer, _ = s.Entities.GetSystem(ctx, orgID, newer.ID)
		dupTask := seed.Task(t, newer.ID, "dup")
		soloTask := seed.Task(t, newer.ID, "solo")
		third, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, jiraScope, "THIRD-1", "10003", TestJiraProject, "Third", "")
		if err != nil {
			t.Fatalf("create third: %v", err)
		}
		seed.Link(t, newer.ID, third.ID)
		seed.Link(t, newer.ID, older.ID)
		shared := seed.Conversation(t, "merge-shared")
		onlyNewer := seed.Conversation(t, "merge-newer")
		for _, l := range []struct{ conv, entity, role string }{
			{shared, older.ID, domain.MemoryRoleTouched},
			{shared, newer.ID, domain.MemoryRolePrimary},
			{onlyNewer, newer.ID, domain.MemoryRoleProduced},
		} {
			if err := s.TaskMemory.RecordEntityTouchSystem(ctx, orgID, l.conv, l.entity, l.role); err != nil {
				t.Fatalf("seed memory link: %v", err)
			}
		}
		newerRefs := seed.Referents(t, newer.ID)
		// The newer row's deliverable moves onto the survivor and takes the key
		// it keeps, NEW-1; a deliverable of no entity under the same key stays.
		movedArt := upsertJiraIdentityDeliverable(t, s, orgID, seed.TeamID, domain.DeliverableKindIssue, jiraScope, "10001", "OLD-1", "")
		otherArt := upsertJiraIdentityDeliverable(t, s, orgID, seed.TeamID, domain.DeliverableKindIssue, jiraScope, "10009", "OLD-1", "")
		// The survivor's own act under the key it stops answering to moves its
		// link onto the key it keeps; the newer row's act moves onto the
		// survivor with its link, which already is that key's.
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderJira, jiraScope, older.ID, "OLD-1", jiraScope+"/browse/OLD-1", "merge-moved")
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderJira, jiraScope, newer.ID, "NEW-1", jiraScope+"/browse/NEW-1", "merge-repointed")

		survivor, err := s.Entities.MergeDuplicateEntitiesSystem(ctx, orgID, newer.ID, older.ID)
		if err != nil || survivor != older.ID {
			t.Fatalf("merge = (%s, %v), want the older row %s to survive", survivor, err, older.ID)
		}
		if gone, _ := s.Entities.GetSystem(ctx, orgID, newer.ID); gone != nil {
			t.Fatalf("the newer row still exists: %+v", gone)
		}

		got, _ := s.Entities.GetSystem(ctx, orgID, older.ID)
		if got.State != "active" || got.ClosedAt != nil || got.SourceID != "NEW-1" || got.URL != jiraScope+"/browse/NEW-1" ||
			got.ExternalID != "10001" || got.SnapshotJSON == "" || got.Title != "New title" {
			t.Errorf("survivor = %+v, want the newer row's live state and id on the older row", got)
		}
		if got.PollSeq <= newer.PollSeq || got.PollSeq <= older.PollSeq {
			t.Errorf("survivor poll_seq = %d, want it past both rows' (%d, %d)", got.PollSeq, older.PollSeq, newer.PollSeq)
		}
		if byKey, _ := s.Entities.GetBySourceSystem(ctx, orgID, "jira", jiraScope, "NEW-1"); byKey == nil || byKey.ID != older.ID {
			t.Errorf("NEW-1 resolves to %+v, want the survivor", byKey)
		}
		for _, c := range []struct{ id, wantTarget, wantEntity string }{{movedArt, "NEW-1", older.ID}, {otherArt, "OLD-1", ""}} {
			if a, err := s.Deliverables.Get(ctx, orgID, c.id); err != nil || a == nil || a.Target != c.wantTarget || a.EntityID != c.wantEntity {
				t.Errorf("deliverable %s = %+v err=%v, want target %q on entity %q", c.id, a, err, c.wantTarget, c.wantEntity)
			}
		}
		for key, wantCurrent := range map[string]string{"merge-moved": jiraScope + "/browse/NEW-1", "merge-repointed": ""} {
			a := findActionByKey(t, s, orgID, key)
			if _, cur := seed.RawActionURL(t, key); a.EntityID != older.ID || cur != wantCurrent {
				t.Errorf("action %s = entity %q current_url %q, want it on the survivor %s with current_url %q", key, a.EntityID, cur, older.ID, wantCurrent)
			}
		}

		for _, c := range []struct {
			id, wantStatus, wantReason string
		}{
			{olderTask, "queued", ""},
			{dupTask, "dismissed", db.DuplicateEntityMergedCloseReason},
			{soloTask, "queued", ""},
		} {
			task, err := s.Tasks.GetSystem(ctx, orgID, c.id)
			if err != nil || task == nil {
				t.Fatalf("task %s: %v", c.id, err)
			}
			if task.EntityID != older.ID || task.Status != c.wantStatus || task.CloseReason != c.wantReason {
				t.Errorf("task %s = entity %s status %q reason %q, want on the survivor, %q, %q",
					c.id, task.EntityID, task.Status, task.CloseReason, c.wantStatus, c.wantReason)
			}
		}

		refs := seed.Referents(t, older.ID)
		if refs.Rows["tasks"] != 3 || refs.Rows["events"] < newerRefs.Rows["events"]+1 {
			t.Errorf("survivor referents = %v, want every task and event of both rows", refs.Rows)
		}
		if refs.Rows["entity_links"] != 1 {
			t.Errorf("survivor entity_links = %d, want the link to the third row only (the self-link dropped)", refs.Rows["entity_links"])
		}
		if refs.MemoryRoles[shared] != domain.MemoryRolePrimary || refs.MemoryRoles[onlyNewer] != domain.MemoryRoleProduced {
			t.Errorf("survivor memory roles = %v, want the stronger role of each conversation", refs.MemoryRoles)
		}
		for table, n := range seed.Referents(t, newer.ID).Rows {
			if n != 0 {
				t.Errorf("%d %s rows still name the merged-away row", n, table)
			}
		}
	})

	t.Run("MergeDuplicateEntitiesSystem_keeps_an_active_survivor_when_the_newer_row_is_closed", func(t *testing.T) {
		s, orgID, seed := mk(t)
		const jiraScope = "https://jira.example.com"
		older, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, jiraScope, "OLD-2", "10002", TestJiraProject, "Older", jiraScope+"/browse/OLD-2")
		if err != nil {
			t.Fatalf("create older: %v", err)
		}
		seed.ForgetExternalID(t, older.ID)
		older, _ = s.Entities.GetSystem(ctx, orgID, older.ID)
		newer, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, jiraScope, "NEW-2", "10002", TestJiraProject, "Newer", jiraScope+"/browse/NEW-2")
		if err != nil {
			t.Fatalf("create newer: %v", err)
		}
		if _, err := s.Entities.MarkClosed(ctx, orgID, newer.ID); err != nil {
			t.Fatalf("close newer: %v", err)
		}
		// The survivor keeps OLD-2, so the closed row's deliverable, recorded
		// under NEW-2, moves onto it.
		art := upsertJiraIdentityDeliverable(t, s, orgID, seed.TeamID, domain.DeliverableKindIssue, jiraScope, "10002", "NEW-2", "")
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderJira, jiraScope, newer.ID, "NEW-2", jiraScope+"/browse/NEW-2", "merge-closed")

		survivor, err := s.Entities.MergeDuplicateEntitiesSystem(ctx, orgID, older.ID, newer.ID)
		if err != nil || survivor != older.ID {
			t.Fatalf("merge = (%s, %v), want %s", survivor, err, older.ID)
		}
		got, _ := s.Entities.GetSystem(ctx, orgID, older.ID)
		if got.State != "active" || got.SourceID != "OLD-2" || got.Title != "Older" || got.ExternalID != "10002" {
			t.Errorf("survivor = %+v, want its own state and key, with the newer row's id", got)
		}
		// The merge is a new version of the row even when the survivor keeps
		// its own live state.
		if got.PollSeq != older.PollSeq+1 {
			t.Errorf("survivor poll_seq = %d, want %d", got.PollSeq, older.PollSeq+1)
		}
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, older.ID, `{"key":"OLD-2"}`, older.PollSeq); err != nil || ok {
			t.Errorf("a snapshot CAS on the version read before the merge = ok %v err %v, want a miss", ok, err)
		}
		if a, err := s.Deliverables.Get(ctx, orgID, art); err != nil || a == nil || a.Target != "OLD-2" || a.EntityID != older.ID {
			t.Errorf("deliverable = %+v err=%v, want target OLD-2 on the survivor %s", a, err, older.ID)
		}
		if _, cur := seed.RawActionURL(t, "merge-closed"); cur != jiraScope+"/browse/OLD-2" || findActionByKey(t, s, orgID, "merge-closed").EntityID != older.ID {
			t.Errorf("action current_url = %q, want it on the survivor %s under the survivor's link", cur, older.ID)
		}
	})

	t.Run("MergeDuplicateEntitiesSystem_refuses_rows_that_are_not_one_object", func(t *testing.T) {
		s, orgID, _ := mk(t)
		const jiraScope = "https://jira.example.com"
		a, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, jiraScope, "A-1", "20001", TestJiraProject, "a", "")
		if err != nil {
			t.Fatalf("create a: %v", err)
		}
		b, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, jiraScope, "B-1", "20002", TestJiraProject, "b", "")
		if err != nil {
			t.Fatalf("create b: %v", err)
		}
		elsewhere, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, "https://other.example.com", "C-1", "20001", TestJiraProject, "c", "")
		if err != nil {
			t.Fatalf("create elsewhere: %v", err)
		}
		if _, err := s.Entities.MergeDuplicateEntitiesSystem(ctx, orgID, a.ID, b.ID); !errors.Is(err, db.ErrEntityIdentityAmbiguous) {
			t.Errorf("merge of two ids: err=%v, want ErrEntityIdentityAmbiguous", err)
		}
		if _, err := s.Entities.MergeDuplicateEntitiesSystem(ctx, orgID, a.ID, elsewhere.ID); err == nil {
			t.Errorf("merge across scopes succeeded")
		}
		if _, err := s.Entities.MergeDuplicateEntitiesSystem(ctx, orgID, a.ID, a.ID); err == nil {
			t.Errorf("merge of a row with itself succeeded")
		}
		if _, err := s.Entities.MergeDuplicateEntitiesSystem(ctx, orgID, a.ID, "00000000-0000-0000-0000-00000000dead"); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("merge with a missing row: err=%v, want sql.ErrNoRows", err)
		}
		for _, e := range []*domain.Entity{a, b, elsewhere} {
			if got, _ := s.Entities.GetSystem(ctx, orgID, e.ID); got == nil || got.SourceID != e.SourceID || got.ExternalID != e.ExternalID {
				t.Errorf("a refused merge changed %s: %+v", e.ID, got)
			}
		}
	})

	// A touch under another name of a renamed repository creates a stub beside
	// the pull request's entity. The stub's references move onto the entity,
	// which is otherwise left exactly as it was.
	t.Run("FoldStubEntitySystem_moves_the_stubs_references_and_keeps_the_entity", func(t *testing.T) {
		s, orgID, seed := mk(t)
		into, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", scope, "acme/new#5", "PR_5", "pr", "Real", "https://github.com/acme/new/pull/5")
		if err != nil {
			t.Fatalf("create entity: %v", err)
		}
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, into.ID, `{"node_id":"PR_5","number":5}`, into.PollSeq); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		stub, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", scope, "acme/old#5", "", "pr", "", "https://github.com/acme/old/pull/5")
		if err != nil {
			t.Fatalf("create stub: %v", err)
		}
		if ok, err := s.Entities.StampOwningTeamIfUnsetSystem(ctx, orgID, stub.ID, seed.TeamID); err != nil || !ok {
			t.Fatalf("stamp the stub's owning team: ok=%v err=%v", ok, err)
		}
		if ok, err := s.Entities.StampCommissionedByIfUnsetSystem(ctx, orgID, stub.ID, seed.UserID); err != nil || !ok {
			t.Fatalf("stamp the stub's commissioning user: ok=%v err=%v", ok, err)
		}
		intoTask := seed.Task(t, into.ID, "dup")
		dupTask := seed.Task(t, stub.ID, "dup")
		soloTask := seed.Task(t, stub.ID, "solo")
		third, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", scope, "acme/third#1", "PR_third", "pr", "Third", "")
		if err != nil {
			t.Fatalf("create third: %v", err)
		}
		seed.Link(t, stub.ID, third.ID)
		seed.Link(t, stub.ID, into.ID)
		shared := seed.Conversation(t, "fold-shared")
		onlyStub := seed.Conversation(t, "fold-stub")
		for _, l := range []struct{ conv, entity, role string }{
			{shared, into.ID, domain.MemoryRoleTouched},
			{shared, stub.ID, domain.MemoryRoleProduced},
			{onlyStub, stub.ID, domain.MemoryRoleTouched},
		} {
			if err := s.TaskMemory.RecordEntityTouchSystem(ctx, orgID, l.conv, l.entity, l.role); err != nil {
				t.Fatalf("seed memory link: %v", err)
			}
		}
		// A comment the run recorded against the stub, under the old name.
		comment, err := s.Deliverables.UpsertSystem(ctx, orgID, domain.Deliverable{
			TeamID: seed.TeamID, Provider: domain.ProviderGitHub, Kind: domain.DeliverableKindComment, Scope: scope,
			Target: "acme/old#5", State: domain.DeliverableStateCommentPosted,
			DedupKey: domain.GitHubCommentKey(domain.GitHubIssueComment, 9005), EntityID: stub.ID,
		})
		if err != nil {
			t.Fatalf("seed the stub's comment deliverable: %v", err)
		}
		// The audit row of that comment, about the stub, linked under the old
		// name.
		recordIdentityAction(t, s, orgID, seed.TeamID, domain.ProviderGitHub, scope, stub.ID, "acme/old#5",
			"https://github.com/acme/old/pull/5#issuecomment-9005", "fold-comment")
		before, _ := s.Entities.GetSystem(ctx, orgID, into.ID)

		if err := s.Entities.FoldStubEntitySystem(ctx, orgID, stub.ID, into.ID); err != nil {
			t.Fatalf("FoldStubEntitySystem: %v", err)
		}
		if gone, _ := s.Entities.GetSystem(ctx, orgID, stub.ID); gone != nil {
			t.Fatalf("the stub still exists: %+v", gone)
		}
		got, _ := s.Entities.GetSystem(ctx, orgID, into.ID)
		if !reflect.DeepEqual(got, before) {
			t.Errorf("entity after the fold = %+v\nwant it unchanged: %+v", got, before)
		}
		// The fold is not a new version of the entity: a cycle that read it
		// before still commits its snapshot.
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, into.ID, `{"node_id":"PR_5","number":5,"title":"x"}`, before.PollSeq); err != nil || !ok {
			t.Errorf("a snapshot CAS on the version read before the fold = ok %v err %v, want it to land", ok, err)
		}
		if team, err := s.Entities.OwningTeamForEntitySystem(ctx, orgID, into.ID); err != nil || team != seed.TeamID {
			t.Errorf("owning team = %q err=%v, want the stub's %q", team, err, seed.TeamID)
		}
		if again, err := s.Entities.StampCommissionedByIfUnsetSystem(ctx, orgID, into.ID, seed.UserID); err != nil || again {
			t.Errorf("commissioning-user stamp after the fold = %v err=%v, want it already set from the stub", again, err)
		}

		for _, c := range []struct {
			id, wantStatus, wantReason string
		}{
			{intoTask, "queued", ""},
			{dupTask, "dismissed", db.DuplicateEntityMergedCloseReason},
			{soloTask, "queued", ""},
		} {
			task, err := s.Tasks.GetSystem(ctx, orgID, c.id)
			if err != nil || task == nil {
				t.Fatalf("task %s: %v", c.id, err)
			}
			if task.EntityID != into.ID || task.Status != c.wantStatus || task.CloseReason != c.wantReason {
				t.Errorf("task %s = entity %s status %q reason %q, want on the entity, %q, %q",
					c.id, task.EntityID, task.Status, task.CloseReason, c.wantStatus, c.wantReason)
			}
		}
		if a, err := s.Deliverables.Get(ctx, orgID, comment.ID); err != nil || a == nil || a.EntityID != into.ID || a.Target != "acme/new#5" {
			t.Errorf("comment deliverable = %+v err=%v, want it on the entity %s under its key acme/new#5", a, err, into.ID)
		}
		// Its audit row moves onto the entity, and its link onto the entity's;
		// the record of the act keeps the old name.
		action := findActionByKey(t, s, orgID, "fold-comment")
		if action.EntityID != into.ID || action.Target != "acme/old#5" || action.URL != "https://github.com/acme/new/pull/5#issuecomment-9005" {
			t.Errorf("comment action = entity %q target %q url %q, want it on %s, recorded under acme/old#5, linked under acme/new",
				action.EntityID, action.Target, action.URL, into.ID)
		}
		refs := seed.Referents(t, into.ID)
		if refs.Rows["tasks"] != 3 || refs.Rows["events"] != 3 {
			t.Errorf("entity referents = %v, want every task and event of both rows", refs.Rows)
		}
		if refs.Rows["entity_links"] != 1 {
			t.Errorf("entity_links = %d, want the link to the third row only (the self-link dropped)", refs.Rows["entity_links"])
		}
		if refs.MemoryRoles[shared] != domain.MemoryRoleProduced || refs.MemoryRoles[onlyStub] != domain.MemoryRoleTouched {
			t.Errorf("memory roles = %v, want the stronger role of each conversation", refs.MemoryRoles)
		}
		for table, n := range seed.Referents(t, stub.ID).Rows {
			if n != 0 {
				t.Errorf("%d %s rows still name the folded stub", n, table)
			}
		}
	})

	t.Run("FoldStubEntitySystem_refuses_a_row_that_is_not_a_stub", func(t *testing.T) {
		s, orgID, _ := mk(t)
		into, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", scope, "acme/fold#1", "PR_f1", "pr", "Real", "")
		if err != nil {
			t.Fatalf("create entity: %v", err)
		}
		seeded, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", scope, "acme/fold#2", "", "pr", "", "")
		if err != nil {
			t.Fatalf("create seeded: %v", err)
		}
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, seeded.ID, `{"number":2}`, seeded.PollSeq); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		identified, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", scope, "acme/fold#3", "PR_f3", "pr", "", "")
		if err != nil {
			t.Fatalf("create identified: %v", err)
		}
		elsewhere, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "github", "ws-b", "acme/fold#4", "", "pr", "", "")
		if err != nil {
			t.Fatalf("create elsewhere: %v", err)
		}
		for name, id := range map[string]string{"a row with a snapshot": seeded.ID, "a row with a provider id": identified.ID} {
			if err := s.Entities.FoldStubEntitySystem(ctx, orgID, id, into.ID); !errors.Is(err, db.ErrEntityNotStub) {
				t.Errorf("fold of %s: err=%v, want ErrEntityNotStub", name, err)
			}
		}
		if err := s.Entities.FoldStubEntitySystem(ctx, orgID, elsewhere.ID, into.ID); err == nil {
			t.Errorf("fold across scopes succeeded")
		}
		if err := s.Entities.FoldStubEntitySystem(ctx, orgID, into.ID, into.ID); err == nil {
			t.Errorf("fold of a row into itself succeeded")
		}
		if err := s.Entities.FoldStubEntitySystem(ctx, orgID, "00000000-0000-0000-0000-00000000dead", into.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("fold of a missing row: err=%v, want sql.ErrNoRows", err)
		}
		for _, e := range []*domain.Entity{into, seeded, identified, elsewhere} {
			if got, _ := s.Entities.GetSystem(ctx, orgID, e.ID); got == nil || got.SourceID != e.SourceID {
				t.Errorf("a refused fold removed or moved %s: %+v", e.ID, got)
			}
		}
	})

	t.Run("RenameSystem_is_idempotent", func(t *testing.T) {
		s, orgID, _ := mk(t)
		if _, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-5", "uuid-5", "team-eng", "t", ""); err != nil {
			t.Fatalf("create: %v", err)
		}
		if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-5", "OPS-5", ""); err != nil || !out.Renamed {
			t.Fatalf("first rename = %+v err=%v", out, err)
		}
		renamed, _ := s.Entities.GetBySourceSystem(ctx, orgID, "linear", scope, "OPS-5")
		if out, err := s.Entities.RenameSystem(ctx, orgID, "linear", scope, "uuid-5", "OPS-5", ""); err != nil || out.Renamed {
			t.Errorf("second rename = %+v err=%v, want a no-op", out, err)
		}
		if again, _ := s.Entities.GetBySourceSystem(ctx, orgID, "linear", scope, "OPS-5"); again == nil || again.PollSeq != renamed.PollSeq {
			t.Errorf("a no-op rename moved poll_seq: %+v, want %d", again, renamed.PollSeq)
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
		row, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-6", "uuid-6", "team-eng", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		holder, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "OPS-6", "uuid-other", "team-eng", "t", "")
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
		a, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-7", "uuid-7", "team-eng", "t", "")
		if err != nil {
			t.Fatalf("create a: %v", err)
		}
		b, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-8", "uuid-8", "team-eng", "t", "")
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

	t.Run("a_linear_row_is_created_with_its_team_and_only_with_it", func(t *testing.T) {
		s, orgID, _ := mk(t)
		row, created, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-20", "uuid-20", "team-eng", "t", "")
		if err != nil || !created || row.LinearTeamID != "team-eng" {
			t.Fatalf("FindOrCreateLinearSystem = (%+v, %v, %v), want a new row carrying team-eng", row, created, err)
		}
		if stored, err := s.Entities.GetSystem(ctx, orgID, row.ID); err != nil || stored == nil ||
			stored.LinearTeamID != "team-eng" || stored.ExternalID != "uuid-20" || stored.Kind != "issue" {
			t.Errorf("point read of the created row = %+v err=%v, want team-eng, uuid-20, kind issue", stored, err)
		}

		// A found row is returned as stored: the team is not rewritten from
		// the call, whose team a later snapshot write would have to agree with.
		again, created, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-20", "uuid-20", "team-ops", "t", "")
		if err != nil || created || again.ID != row.ID || again.LinearTeamID != "team-eng" {
			t.Errorf("second FindOrCreateLinearSystem = (%+v, %v, %v), want the row as stored", again, created, err)
		}
		// The generic door finds a Linear row like any other.
		found, created, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-20", "uuid-20", "issue", "t", "")
		if err != nil || created || found.ID != row.ID {
			t.Errorf("FindOrCreateSystem on a known Linear row = (%+v, %v, %v), want it found", found, created, err)
		}

		// ...and creates none.
		if got, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "linear", scope, "ENG-21", "uuid-21", "issue", "t", ""); !errors.Is(err, db.ErrEntityLinearTeamRequired) || got != nil {
			t.Errorf("FindOrCreateSystem creating a Linear row = (%+v, %v), want ErrEntityLinearTeamRequired", got, err)
		}
		if _, _, err := s.Entities.FindOrCreate(ctx, orgID, "linear", scope, "ENG-21", "uuid-21", "issue", "t", ""); !errors.Is(err, db.ErrEntityLinearTeamRequired) {
			t.Errorf("FindOrCreate creating a Linear row: err=%v, want ErrEntityLinearTeamRequired", err)
		}
		if _, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-21", "uuid-21", "", "t", ""); !errors.Is(err, db.ErrEntityLinearTeamRequired) {
			t.Errorf("FindOrCreateLinearSystem with no team: err=%v, want ErrEntityLinearTeamRequired", err)
		}
		if _, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-21", "", "team-eng", "t", ""); err == nil {
			t.Error("FindOrCreateLinearSystem with no issue id: err=nil, want a refusal")
		}
		if got, _ := s.Entities.GetByExternalIDSystem(ctx, orgID, "linear", scope, "uuid-21"); got != nil {
			t.Errorf("a refused create wrote %+v", got)
		}

		other, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, scope, "SKY-20", "20020", TestJiraProject, "t", "")
		if err != nil || other.LinearTeamID != "" {
			t.Errorf("a Jira row = %+v err=%v, want no Linear team", other, err)
		}
	})

	t.Run("snapshot_writes_move_the_linear_team_and_clearing_keeps_it", func(t *testing.T) {
		s, orgID, _ := mk(t)
		team := func(id string) string {
			t.Helper()
			got, err := s.Entities.GetSystem(ctx, orgID, id)
			if err != nil || got == nil {
				t.Fatalf("read %s: %+v err=%v", id, got, err)
			}
			return got.LinearTeamID
		}
		pollSeq := func(id string) int64 {
			t.Helper()
			got, _ := s.Entities.GetSystem(ctx, orgID, id)
			return got.PollSeq
		}
		row, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-22", "uuid-22", "team-eng", "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}

		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, row.ID, `{"id":"uuid-22","team_id":"team-ops"}`, pollSeq(row.ID)); err != nil || !ok {
			t.Fatalf("snapshot write: ok=%v err=%v", ok, err)
		}
		if got := team(row.ID); got != "team-ops" {
			t.Errorf("after UpdateSnapshotCASSystem the team is %q, want the snapshot's team-ops", got)
		}
		// A CAS that misses writes neither half.
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, row.ID, `{"id":"uuid-22","team_id":"team-lost"}`, pollSeq(row.ID)-1); err != nil || ok {
			t.Fatalf("stale snapshot write: ok=%v err=%v, want a miss", ok, err)
		}
		if got := team(row.ID); got != "team-ops" {
			t.Errorf("a missed CAS moved the team to %q", got)
		}
		// A snapshot that names no team leaves the column where it was.
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, row.ID, `{"id":"uuid-22"}`, pollSeq(row.ID)); err != nil || !ok {
			t.Fatalf("teamless snapshot write: ok=%v err=%v", ok, err)
		}
		if got := team(row.ID); got != "team-ops" {
			t.Errorf("a snapshot naming no team moved the team to %q", got)
		}

		if ok, err := s.Entities.CloseWithSnapshotCASSystem(ctx, orgID, row.ID, `{"id":"uuid-22","team_id":"team-close"}`, pollSeq(row.ID)); err != nil || !ok {
			t.Fatalf("close with snapshot: ok=%v err=%v", ok, err)
		}
		if got := team(row.ID); got != "team-close" {
			t.Errorf("after CloseWithSnapshotCASSystem the team is %q, want team-close", got)
		}
		if ok, err := s.Entities.ReactivateWithSnapshotCASSystem(ctx, orgID, row.ID, `{"id":"uuid-22","team_id":"team-reopen"}`, pollSeq(row.ID)); err != nil || !ok {
			t.Fatalf("reactivate with snapshot: ok=%v err=%v", ok, err)
		}
		if got := team(row.ID); got != "team-reopen" {
			t.Errorf("after ReactivateWithSnapshotCASSystem the team is %q, want team-reopen", got)
		}

		// The event queue's snapshot commit is the same CAS statement.
		if ok, _, err := s.EventQueue.EnqueueBatchWithSnapshotCAS(ctx, orgID, row.ID, `{"id":"uuid-22","team_id":"team-queue"}`, pollSeq(row.ID), nil, nil); err != nil || !ok {
			t.Fatalf("EnqueueBatchWithSnapshotCAS: ok=%v err=%v", ok, err)
		}
		if got := team(row.ID); got != "team-queue" {
			t.Errorf("after EnqueueBatchWithSnapshotCAS the team is %q, want team-queue", got)
		}

		if _, err := s.Entities.ClearSnapshotsForSourceSystem(ctx, orgID, "linear"); err != nil {
			t.Fatalf("clear: %v", err)
		}
		got, _ := s.Entities.GetSystem(ctx, orgID, row.ID)
		if got.SnapshotJSON != "" || got.LinearTeamID != "team-queue" {
			t.Errorf("after the clear: snapshot %q, team %q; want no snapshot and team-queue kept", got.SnapshotJSON, got.LinearTeamID)
		}

		// A snapshot write never gives another source's row a Linear team.
		jira, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, scope, "SKY-22", "20022", TestJiraProject, "t", "")
		if err != nil {
			t.Fatalf("create jira: %v", err)
		}
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, jira.ID, `{"key":"SKY-22","team_id":"team-eng"}`, jira.PollSeq); err != nil || !ok {
			t.Fatalf("jira snapshot write: ok=%v err=%v", ok, err)
		}
		if got := team(jira.ID); got != "" {
			t.Errorf("a Jira row took the Linear team %q from its snapshot", got)
		}
	})

	t.Run("a_jira_row_is_created_with_its_project_and_only_with_it", func(t *testing.T) {
		s, orgID, _ := mk(t)
		row, created, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, scope, "SKY-30", "20030", TestJiraProject, "t", "")
		if err != nil || !created || row.JiraProjectID == "" {
			t.Fatalf("FindOrCreateJiraSystem = (%+v, %v, %v), want a new row referencing its project", row, created, err)
		}
		if stored, err := s.Entities.GetSystem(ctx, orgID, row.ID); err != nil || stored == nil ||
			stored.JiraProjectID != row.JiraProjectID || stored.ExternalID != "20030" || stored.Kind != "issue" {
			t.Errorf("point read of the created row = %+v err=%v, want its project, 20030, kind issue", stored, err)
		}
		project, err := s.JiraProjects.GetSystem(ctx, orgID, row.JiraProjectID)
		if err != nil || project == nil || project.Site != scope || project.ExternalID != TestJiraProject.ID || project.Key != TestJiraProject.Key {
			t.Errorf("the referenced registry row = %+v err=%v, want %s under %s on the entity's scope", project, err, TestJiraProject.Key, TestJiraProject.ID)
		}

		// A second issue in the project references the same row; the same
		// project id on another scope is another project.
		sibling, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, scope, "SKY-31", "20031", TestJiraProject, "t", "")
		if err != nil || sibling.JiraProjectID != row.JiraProjectID {
			t.Errorf("a second issue in the project = %+v err=%v, want it on row %s", sibling, err, row.JiraProjectID)
		}
		elsewhere, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, "ws-b", "SKY-30", "20030", TestJiraProject, "t", "")
		if err != nil || elsewhere.JiraProjectID == "" || elsewhere.JiraProjectID == row.JiraProjectID {
			t.Errorf("the project's id on another scope = %+v err=%v, want a registry row of its own", elsewhere, err)
		}

		// A found row is returned as stored: the project is not rewritten from
		// the call, which a later snapshot write would have to agree with.
		again, created, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, scope, "SKY-30", "20030",
			domain.JiraProjectRef{ID: "99999", Key: "OPS"}, "t", "")
		if err != nil || created || again.ID != row.ID || again.JiraProjectID != row.JiraProjectID {
			t.Errorf("second FindOrCreateJiraSystem = (%+v, %v, %v), want the row as stored", again, created, err)
		}
		// The generic doors find a Jira row like any other...
		found, created, err := s.Entities.FindOrCreateSystem(ctx, orgID, "jira", scope, "SKY-30", "20030", "issue", "t", "")
		if err != nil || created || found.ID != row.ID {
			t.Errorf("FindOrCreateSystem on a known Jira row = (%+v, %v, %v), want it found", found, created, err)
		}
		// ...and create none.
		if got, _, err := s.Entities.FindOrCreateSystem(ctx, orgID, "jira", scope, "SKY-32", "20032", "issue", "t", ""); !errors.Is(err, db.ErrEntityJiraProjectRequired) || got != nil {
			t.Errorf("FindOrCreateSystem creating a Jira row = (%+v, %v), want ErrEntityJiraProjectRequired", got, err)
		}
		if _, _, err := s.Entities.FindOrCreate(ctx, orgID, "jira", scope, "SKY-32", "20032", "issue", "t", ""); !errors.Is(err, db.ErrEntityJiraProjectRequired) {
			t.Errorf("FindOrCreate creating a Jira row: err=%v, want ErrEntityJiraProjectRequired", err)
		}
		if _, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, scope, "SKY-32", "", TestJiraProject, "t", ""); err == nil {
			t.Error("FindOrCreateJiraSystem with no issue id: err=nil, want a refusal")
		}
		for name, p := range map[string]domain.JiraProjectRef{"no id": {Key: "SKY"}, "no key": {ID: "10000"}} {
			if _, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, scope, "SKY-32", "20032", p, "t", ""); !errors.Is(err, db.ErrEntityJiraProjectRequired) {
				t.Errorf("FindOrCreateJiraSystem with a project with %s: err=%v, want ErrEntityJiraProjectRequired", name, err)
			}
		}
		if got, _ := s.Entities.GetByExternalIDSystem(ctx, orgID, "jira", scope, "20032"); got != nil {
			t.Errorf("a refused create wrote %+v", got)
		}

		other, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-30", "uuid-30", "team-eng", "t", "")
		if err != nil || other.JiraProjectID != "" {
			t.Errorf("a Linear row = %+v err=%v, want no Jira project", other, err)
		}
	})

	t.Run("snapshot_writes_move_the_jira_project_and_clearing_keeps_it", func(t *testing.T) {
		s, orgID, _ := mk(t)
		projectOf := func(id string) string {
			t.Helper()
			got, err := s.Entities.GetSystem(ctx, orgID, id)
			if err != nil || got == nil {
				t.Fatalf("read %s: %+v err=%v", id, got, err)
			}
			return got.JiraProjectID
		}
		pollSeq := func(id string) int64 {
			t.Helper()
			got, _ := s.Entities.GetSystem(ctx, orgID, id)
			return got.PollSeq
		}
		register := func(scope, externalID, key string) string {
			t.Helper()
			p, err := s.JiraProjects.FindOrCreateSystem(ctx, orgID, scope, externalID, key, "")
			if err != nil {
				t.Fatalf("register %s: %v", key, err)
			}
			return p.ID
		}
		row, _, err := s.Entities.FindOrCreateJiraSystem(ctx, orgID, scope, "SKY-33", "20033", TestJiraProject, "t", "")
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		sky := row.JiraProjectID
		ops := register(scope, "21000", "OPS")
		eng := register(scope, "22000", "ENG")
		register("ws-b", "23000", "FAR")

		snap := func(projectID string) string { return `{"id":"20033","project_id":"` + projectID + `"}` }
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, row.ID, snap("21000"), pollSeq(row.ID)); err != nil || !ok {
			t.Fatalf("snapshot write: ok=%v err=%v", ok, err)
		}
		if got := projectOf(row.ID); got != ops {
			t.Errorf("after UpdateSnapshotCASSystem the project is %q, want OPS's row %q", got, ops)
		}
		// A CAS that misses writes neither half.
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, row.ID, snap("22000"), pollSeq(row.ID)-1); err != nil || ok {
			t.Fatalf("stale snapshot write: ok=%v err=%v, want a miss", ok, err)
		}
		if got := projectOf(row.ID); got != ops {
			t.Errorf("a missed CAS moved the project to %q", got)
		}
		// A snapshot naming no project, one the registry has no row for, or
		// one registered only on another site leaves the column where it was.
		for name, body := range map[string]string{
			"no project":   `{"id":"20033"}`,
			"unregistered": snap("29999"),
			"another site": snap("23000"),
		} {
			if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, row.ID, body, pollSeq(row.ID)); err != nil || !ok {
				t.Fatalf("snapshot write (%s): ok=%v err=%v", name, ok, err)
			}
			if got := projectOf(row.ID); got != ops {
				t.Errorf("a snapshot with %s moved the project to %q", name, got)
			}
		}

		if ok, err := s.Entities.CloseWithSnapshotCASSystem(ctx, orgID, row.ID, snap("22000"), pollSeq(row.ID)); err != nil || !ok {
			t.Fatalf("close with snapshot: ok=%v err=%v", ok, err)
		}
		if got := projectOf(row.ID); got != eng {
			t.Errorf("after CloseWithSnapshotCASSystem the project is %q, want ENG's row", got)
		}
		if ok, err := s.Entities.ReactivateWithSnapshotCASSystem(ctx, orgID, row.ID, snap("10000"), pollSeq(row.ID)); err != nil || !ok {
			t.Fatalf("reactivate with snapshot: ok=%v err=%v", ok, err)
		}
		if got := projectOf(row.ID); got != sky {
			t.Errorf("after ReactivateWithSnapshotCASSystem the project is %q, want SKY's row", got)
		}

		// The event queue's snapshot commit is the same CAS statement.
		if ok, _, err := s.EventQueue.EnqueueBatchWithSnapshotCAS(ctx, orgID, row.ID, snap("22000"), pollSeq(row.ID), nil, nil); err != nil || !ok {
			t.Fatalf("EnqueueBatchWithSnapshotCAS: ok=%v err=%v", ok, err)
		}
		if got := projectOf(row.ID); got != eng {
			t.Errorf("after EnqueueBatchWithSnapshotCAS the project is %q, want ENG's row", got)
		}

		if _, err := s.Entities.ClearSnapshotsForSourceSystem(ctx, orgID, "jira"); err != nil {
			t.Fatalf("clear: %v", err)
		}
		got, _ := s.Entities.GetSystem(ctx, orgID, row.ID)
		if got.SnapshotJSON != "" || got.JiraProjectID != eng {
			t.Errorf("after the clear: snapshot %q, project %q; want no snapshot and ENG's row kept", got.SnapshotJSON, got.JiraProjectID)
		}

		// A snapshot write never gives another source's row a Jira project.
		linear, _, err := s.Entities.FindOrCreateLinearSystem(ctx, orgID, scope, "ENG-33", "uuid-33", "team-eng", "t", "")
		if err != nil {
			t.Fatalf("create linear: %v", err)
		}
		if ok, err := s.Entities.UpdateSnapshotCASSystem(ctx, orgID, linear.ID, `{"id":"uuid-33","team_id":"team-eng","project_id":"21000"}`, linear.PollSeq); err != nil || !ok {
			t.Fatalf("linear snapshot write: ok=%v err=%v", ok, err)
		}
		if got := projectOf(linear.ID); got != "" {
			t.Errorf("a Linear row took the Jira project %q from its snapshot", got)
		}
	})
}

// upsertIdentityDeliverable records a Linear deliverable in workspace scope the way
// domain.ObjectKey says to key one: on the issue's UUID, with the
// identifier as its target, linked to the entity carrying the UUID in the
// scope (identityDeliverableEntity).
func upsertIdentityDeliverable(t *testing.T, s db.Stores, orgID, teamID, scope, kind, issueID, key, anchor string) string {
	t.Helper()
	a, err := s.Deliverables.UpsertSystem(context.Background(), orgID, domain.Deliverable{
		TeamID: teamID, Provider: domain.ProviderLinear, Kind: kind, Scope: scope,
		Target: key, ExternalID: issueID + anchor, State: identityDeliverableState(kind),
		DedupKey: domain.ObjectKey(domain.ProviderLinear, kind, issueID, anchor),
		EntityID: identityDeliverableEntity(t, s, orgID, domain.ProviderLinear, scope, issueID),
	})
	if err != nil {
		t.Fatalf("seed %s deliverable on %s: %v", kind, key, err)
	}
	return a.ID
}

// upsertJiraIdentityDeliverable records a Jira deliverable the way
// domain.ObjectKey says to key one: on the issue's site and id, with the
// key as its target, linked to the entity carrying the id on the site
// (identityDeliverableEntity).
func upsertJiraIdentityDeliverable(t *testing.T, s db.Stores, orgID, teamID, kind, site, issueID, key, anchor string) string {
	t.Helper()
	a, err := s.Deliverables.UpsertSystem(context.Background(), orgID, domain.Deliverable{
		TeamID: teamID, Provider: domain.ProviderJira, Kind: kind, Scope: site,
		Target: key, ExternalID: issueID + anchor, State: identityDeliverableState(kind),
		DedupKey: domain.ObjectKey(domain.ProviderJira, kind, domain.JiraIssueResource(site, issueID), anchor),
		EntityID: identityDeliverableEntity(t, s, orgID, domain.ProviderJira, site, issueID),
	})
	if err != nil {
		t.Fatalf("seed jira %s deliverable on %s: %v", kind, key, err)
	}
	return a.ID
}

// identityDeliverableEntity is the entity a deliverable about the issue issueID is
// linked to, as the exec funnel links it: the one carrying the id in scope, or
// "" when none does.
func identityDeliverableEntity(t *testing.T, s db.Stores, orgID, source, scope, issueID string) string {
	t.Helper()
	e, err := s.Entities.GetByExternalIDSystem(context.Background(), orgID, source, scope, issueID)
	if err != nil {
		t.Fatalf("read the entity carrying %s in %s: %v", issueID, scope, err)
	}
	if e == nil {
		return ""
	}
	return e.ID
}

func identityDeliverableState(kind string) string {
	if kind == domain.DeliverableKindComment {
		return domain.DeliverableStateCommentPosted
	}
	return domain.DeliverableStateIssueUpdated
}

func recordIdentityAction(t *testing.T, s db.Stores, orgID, teamID, provider, scope, entityID, key, url, dedupKey string) {
	t.Helper()
	if err := s.ExternalActions.RecordSystem(context.Background(), orgID, domain.ExternalAction{
		TeamID: teamID, Provider: provider, Scope: scope, Action: domain.ActionIssueCommentPosted,
		Target: key, EntityID: entityID, URL: url, Credential: domain.CredentialNone, DedupKey: dedupKey,
		PerformedBy: domain.PerformedByAgent,
	}); err != nil {
		t.Fatalf("seed action %s: %v", dedupKey, err)
	}
}
