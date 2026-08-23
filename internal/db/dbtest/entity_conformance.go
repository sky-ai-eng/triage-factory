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
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// EntityStoreFactory is what a per-backend test file hands to
// RunEntityStoreConformance. Returns:
//   - the wired EntityStore impl,
//   - the orgID to pass to every call,
//   - an EntitySeeder for the team rows the owning-team subtests need
//     (entities themselves come from FindOrCreate).
type EntityStoreFactory func(t *testing.T) (
	store db.EntityStore,
	orgID string,
	seed EntitySeeder,
)

// EntitySeeder is a bag of callbacks the conformance suite uses to
// stage non-entity fixture rows.
type EntitySeeder struct {
	// Team inserts a team row and returns its id. The owning-team stamp
	// subtests need two DISTINCT ids that both satisfy the owning_team_id
	// FK — one to stamp, one to prove a second writer cannot displace it.
	Team func(t *testing.T, name string) string
}

// RunEntityStoreConformance covers the entity-store contract every
// backend impl must hold:
//
//   - FindOrCreate inserts then re-reads on the same key, never rewriting
//     kind on an already-known row.
//   - Get / GetBySource return (nil, nil) on miss; GetBySourceSystem
//     mirrors GetBySource.
//   - Update* mutations land on the right column, with UpdateSnapshot
//     also stamping last_polled_at and PatchSnapshot deliberately
//     leaving it alone.
//   - MarkClosed is unconditional; Close only fires when state='active';
//     Reactivate only fires when state='closed'.
//   - ListActive filters on the documented predicates.
//   - ListActiveTerminalCandidatesSystem surfaces active entities whose
//     stored snapshot reads terminal (github exactly, jira against the
//     caller's done-status union) and nothing else.
//   - Descriptions dedupes the input id list and only returns ids
//     whose description is non-empty.
//   - MarkPolledSystem advances last_polled_at without touching the
//     snapshot or poll_seq.

// mustEntity re-reads an entity by its natural key, failing the test if it is
// missing. Assertions about timestamp columns have to compare DB-precision
// values on both sides (see the UpdateSnapshot subtest), which means re-reading
// rather than reusing the struct a mutation returned.
func mustEntity(t *testing.T, s db.EntityStore, ctx context.Context, orgID, source, sourceID string) *domain.Entity {
	t.Helper()
	ent, err := s.GetBySource(ctx, orgID, source, sourceID)
	if err != nil {
		t.Fatalf("re-read %s/%s: %v", source, sourceID, err)
	}
	if ent == nil {
		t.Fatalf("re-read %s/%s: no row", source, sourceID)
	}
	return ent
}

func RunEntityStoreConformance(t *testing.T, mk EntityStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("every_single_row_write_returns_the_stored_row", func(t *testing.T) {
		// The returned-row standard applied to each converted method in turn.
		// The property is one line — what the write handed back is what a point
		// read finds — and AssertWriteReturnedStoredRow's doc covers what that
		// stands in for (RETURNING semantics, RLS visibility on the update arm,
		// column-list drift).
		s, orgID, _ := mk(t)
		created, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#900", "pr", "Returned", "https://example.com/900")
		if err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		read := func() (*domain.Entity, error) { return s.Get(ctx, orgID, created.ID) }

		snapped, err := s.UpdateSnapshot(ctx, orgID, created.ID, `{"state":"open"}`)
		if err != nil {
			t.Fatalf("UpdateSnapshot: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateSnapshot", snapped, read)
		// last_polled_at is the statement's, not the caller's — the input is
		// one JSON string and the row carries a stamp alongside it.
		if snapped.LastPolledAt == nil {
			t.Error("UpdateSnapshot returned a row with no last_polled_at, the column it stamps")
		}

		patched, err := s.PatchSnapshot(ctx, orgID, created.ID, `{"state":"draft"}`)
		if err != nil {
			t.Fatalf("PatchSnapshot: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "PatchSnapshot", patched, read)

		titled, err := s.UpdateTitle(ctx, orgID, created.ID, "Returned v2")
		if err != nil {
			t.Fatalf("UpdateTitle: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateTitle", titled, read)

		described, err := s.UpdateDescription(ctx, orgID, created.ID, "body text")
		if err != nil {
			t.Fatalf("UpdateDescription: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateDescription", described, read)

		urled, err := s.UpdateURLSystem(ctx, orgID, created.ID, "https://example.com/900-moved")
		if err != nil {
			t.Fatalf("UpdateURLSystem: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "UpdateURLSystem", urled, read)

		// The guarded close: it fires once and declines the second time, and
		// nil is how the caller tells those apart.
		closed, err := s.Close(ctx, orgID, created.ID)
		if err != nil || closed == nil {
			t.Fatalf("Close: got=%v err=%v", closed, err)
		}
		AssertWriteReturnedStoredRow(t, "Close", *closed, read)
		if closed.State != "closed" || closed.ClosedAt == nil {
			t.Errorf("Close returned %+v, want state=closed with a closed_at stamp", closed)
		}
		again, err := s.Close(ctx, orgID, created.ID)
		if err != nil || again != nil {
			t.Errorf("second Close: got=%v err=%v, want (nil, nil) — the guard declined", again, err)
		}

		marked, err := s.MarkClosed(ctx, orgID, created.ID)
		if err != nil {
			t.Fatalf("MarkClosed: %v", err)
		}
		AssertWriteReturnedStoredRow(t, "MarkClosed", marked, read)

		// A miss is an error rather than a silent no-op, on every id-keyed
		// write — that is what retires the rows-affected probes.
		missing := uuid.New().String()
		for _, tc := range []struct {
			name string
			run  func() error
		}{
			{"UpdateSnapshot", func() error { _, e := s.UpdateSnapshot(ctx, orgID, missing, "{}"); return e }},
			{"PatchSnapshot", func() error { _, e := s.PatchSnapshot(ctx, orgID, missing, "{}"); return e }},
			{"UpdateTitle", func() error { _, e := s.UpdateTitle(ctx, orgID, missing, "x"); return e }},
			{"UpdateDescription", func() error { _, e := s.UpdateDescription(ctx, orgID, missing, "x"); return e }},
			{"UpdateURLSystem", func() error { _, e := s.UpdateURLSystem(ctx, orgID, missing, "x"); return e }},
			{"MarkClosed", func() error { _, e := s.MarkClosed(ctx, orgID, missing); return e }},
		} {
			if err := tc.run(); !errors.Is(err, sql.ErrNoRows) {
				t.Errorf("%s on a missing id: got %v, want sql.ErrNoRows", tc.name, err)
			}
		}
		// Close is the exception in shape: its guard makes "no such active
		// entity" an answer, so a missing id is (nil, nil) like GetBySource.
		if got, err := s.Close(ctx, orgID, missing); err != nil || got != nil {
			t.Errorf("Close on a missing id: got=%v err=%v, want (nil, nil)", got, err)
		}
	})

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

	t.Run("FindOrCreate_supports_slack_message_entities", func(t *testing.T) {
		// TFAC-513: Slack threads resolve through the same canonical resolver
		// (source='slack', kind='message', source_id='<channel>/<thread_ts>').
		// There is no CHECK on source/kind in either dialect, so this needs no
		// migration — pin that both backends round-trip it and the natural key
		// dedups a re-resolved thread.
		s, orgID, _ := mk(t)

		const sid = "C0125/1700000000.000100"
		ent, created, err := s.FindOrCreate(ctx, orgID, "slack", sid, "message",
			"first message text", "https://slack.example/archives/C0125/p1700000000000100")
		if err != nil {
			t.Fatalf("FindOrCreate(slack): %v", err)
		}
		if !created {
			t.Fatalf("expected created=true on first slack resolve")
		}
		if ent.Source != "slack" || ent.Kind != "message" || ent.SourceID != sid {
			t.Errorf("unexpected slack entity: %+v", ent)
		}
		// Slack entities are complete-at-create — title + permalink, no snapshot.
		if ent.Title != "first message text" {
			t.Errorf("title = %q, want the first message text", ent.Title)
		}

		again, created2, err := s.FindOrCreate(ctx, orgID, "slack", sid, "message", "ignored", "")
		if err != nil {
			t.Fatalf("FindOrCreate(slack) re-resolve: %v", err)
		}
		if created2 {
			t.Errorf("re-resolving the same thread must return created=false")
		}
		if again.ID != ent.ID {
			t.Errorf("re-resolve id = %s, want %s", again.ID, ent.ID)
		}
	})

	t.Run("FindOrCreate_never_rewrites_kind_on_an_existing_row", func(t *testing.T) {
		// Slack's two ingest paths (a root mention vs. a run's own root post)
		// each mint kind="thread"; a mid-thread summons and the generic
		// touched-entity resolver default to kind="message". Whichever
		// resolves the entity first must stick — a later resolve under a
		// different kind (e.g. a reply/edit landing on an already-"thread"
		// entity) must not downgrade it back to "message".
		s, orgID, _ := mk(t)

		const sid = "C0777/1700000000.000400"
		first, created, err := s.FindOrCreate(ctx, orgID, "slack", sid, "thread", "root text", "")
		if err != nil {
			t.Fatalf("FindOrCreate(thread): %v", err)
		}
		if !created {
			t.Fatalf("expected created=true on first resolve")
		}

		second, created2, err := s.FindOrCreate(ctx, orgID, "slack", sid, "message", "ignored", "")
		if err != nil {
			t.Fatalf("FindOrCreate(message, re-resolve): %v", err)
		}
		if created2 {
			t.Errorf("re-resolving the same entity must return created=false")
		}
		if second.ID != first.ID {
			t.Errorf("re-resolve id = %s, want %s", second.ID, first.ID)
		}
		if second.Kind != "thread" {
			t.Errorf("kind = %q, want thread (unchanged by the later message-kind resolve)", second.Kind)
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

	t.Run("GetBySourceSystem_mirrors_GetBySource", func(t *testing.T) {
		s, orgID, _ := mk(t)

		want, _, err := s.FindOrCreate(ctx, orgID, "slack", "C0999/1700000000.000300", "thread", "root text", "")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}

		got, err := s.GetBySourceSystem(ctx, orgID, "slack", "C0999/1700000000.000300")
		if err != nil {
			t.Fatalf("GetBySourceSystem: %v", err)
		}
		if got == nil || got.ID != want.ID || got.Kind != "thread" {
			t.Errorf("GetBySourceSystem = %+v, want the seeded entity %+v", got, want)
		}

		miss, err := s.GetBySourceSystem(ctx, orgID, "slack", "C0999/nonexistent")
		if err != nil {
			t.Fatalf("GetBySourceSystem(miss): %v", err)
		}
		if miss != nil {
			t.Errorf("GetBySourceSystem on miss returned %+v, want nil", miss)
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

		if _, err := s.UpdateSnapshot(ctx, orgID, baseline.ID, `{"k":"v"}`); err != nil {
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

	t.Run("UpdateSnapshotCASSystem_stale_seq_loses_cleanly", func(t *testing.T) {
		s, orgID, _ := mk(t)
		if _, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#cas", "pr", "T", ""); err != nil {
			t.Fatalf("FindOrCreate: %v", err)
		}
		ent, err := s.GetBySource(ctx, orgID, "github", "owner/repo#cas")
		if err != nil || ent == nil {
			t.Fatalf("GetBySource: ent=%v err=%v", ent, err)
		}

		// Winner: writes at the seq it read → lands, seq bumps by 1.
		ok, err := s.UpdateSnapshotCASSystem(ctx, orgID, ent.ID, `{"winner":true}`, ent.PollSeq)
		if err != nil || !ok {
			t.Fatalf("CAS at current seq: ok=%v err=%v, want true/nil", ok, err)
		}

		// Straggler: writes at the seq it read BEFORE the winner landed →
		// zero rows, no error, snapshot untouched (the straggler-ex-leader
		// contract: a late write is a no-op, never a regression).
		ok, err = s.UpdateSnapshotCASSystem(ctx, orgID, ent.ID, `{"stale":true}`, ent.PollSeq)
		if err != nil {
			t.Fatalf("CAS at stale seq errored: %v", err)
		}
		if ok {
			t.Fatal("CAS at a stale poll_seq must report ok=false")
		}
		got, err := s.Get(ctx, orgID, ent.ID)
		if err != nil || got == nil {
			t.Fatalf("re-read: %v", err)
		}
		if strings.Contains(got.SnapshotJSON, "stale") || !strings.Contains(got.SnapshotJSON, "winner") {
			t.Errorf("stale CAS overwrote the winning snapshot: %q", got.SnapshotJSON)
		}
		if got.PollSeq != ent.PollSeq+1 {
			t.Errorf("poll_seq = %d, want %d (exactly one successful write)", got.PollSeq, ent.PollSeq+1)
		}

		// The next reader CASes at the fresh seq and wins normally.
		if ok, err := s.UpdateSnapshotCASSystem(ctx, orgID, ent.ID, `{"next":true}`, got.PollSeq); err != nil || !ok {
			t.Errorf("CAS at the advanced seq: ok=%v err=%v, want true/nil", ok, err)
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

		if _, err := s.PatchSnapshot(ctx, orgID, baseline.ID, `{"patched":true}`); err != nil {
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

	t.Run("MarkPolledSystem_advances_last_polled_at_and_nothing_else", func(t *testing.T) {
		s, orgID, _ := mk(t)

		if _, _, err := s.FindOrCreate(ctx, orgID, "jira", "SKY-POLLED", "issue", "T", ""); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if ok, err := s.UpdateSnapshotCASSystem(ctx, orgID, mustEntity(t, s, ctx, orgID, "jira", "SKY-POLLED").ID, `{"status":"To Do"}`, 0); err != nil || !ok {
			t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
		}
		// Re-read for a DB-precision baseline — see the UpdateSnapshot
		// subtest above for the timestamptz-truncation rationale.
		baseline := mustEntity(t, s, ctx, orgID, "jira", "SKY-POLLED")
		if baseline.LastPolledAt == nil {
			t.Fatal("baseline has no last_polled_at")
		}

		if err := s.MarkPolledSystem(ctx, orgID, baseline.ID); err != nil {
			t.Fatalf("MarkPolledSystem: %v", err)
		}

		got := mustEntity(t, s, ctx, orgID, "jira", "SKY-POLLED")
		if got.LastPolledAt == nil || !got.LastPolledAt.After(*baseline.LastPolledAt) {
			t.Errorf("last_polled_at must advance — baseline=%v after=%v; a caller that selects candidates by this column's age would re-pick the row forever",
				baseline.LastPolledAt, got.LastPolledAt)
		}
		// The snapshot is untouched, and poll_seq does not move: this
		// records a read, not a diff, so it must not consume the CAS
		// token a concurrent snapshot write is holding.
		if got.SnapshotJSON != baseline.SnapshotJSON {
			t.Errorf("snapshot_json changed: %q → %q", baseline.SnapshotJSON, got.SnapshotJSON)
		}
		if got.PollSeq != baseline.PollSeq {
			t.Errorf("poll_seq moved %d → %d", baseline.PollSeq, got.PollSeq)
		}
	})

	t.Run("UpdateTitle_and_UpdateDescription_round_trip", func(t *testing.T) {
		s, orgID, _ := mk(t)

		ent, _, err := s.FindOrCreate(ctx, orgID, "jira", "SKY-100", "issue", "Old Title", "")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}

		if _, err := s.UpdateTitle(ctx, orgID, ent.ID, "New Title"); err != nil {
			t.Fatalf("UpdateTitle: %v", err)
		}
		if _, err := s.UpdateDescription(ctx, orgID, ent.ID, "Body paragraph"); err != nil {
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

	t.Run("UpdateURLSystem_round_trips_and_is_a_noop_on_missing", func(t *testing.T) {
		s, orgID, _ := mk(t)

		ent, _, err := s.FindOrCreate(ctx, orgID, "slack", "C0125/1700000000.000200", "message", "T", "")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		if ent.URL != "" {
			t.Fatalf("seeded entity url = %q, want empty (permalink not yet resolved)", ent.URL)
		}

		const permalink = "https://acme.slack.com/archives/C0125/p1700000000000200"
		if _, err := s.UpdateURLSystem(ctx, orgID, ent.ID, permalink); err != nil {
			t.Fatalf("UpdateURLSystem: %v", err)
		}
		got, err := s.Get(ctx, orgID, ent.ID)
		if err != nil || got == nil {
			t.Fatalf("re-read: %v", err)
		}
		if got.URL != permalink {
			t.Errorf("url = %q, want %q", got.URL, permalink)
		}

		// A missing id is sql.ErrNoRows, like every other id-keyed write on
		// this store. The caller resolved the entity moments earlier, so the
		// only way to reach this is the row going away underneath it — which
		// is worth a log line rather than a silent success.
		if _, err := s.UpdateURLSystem(ctx, orgID, uuid.New().String(), permalink); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("UpdateURLSystem on missing entity: %v, want sql.ErrNoRows", err)
		}
	})

	t.Run("Close_only_fires_on_active", func(t *testing.T) {
		s, orgID, _ := mk(t)

		ent, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#close", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed: %v", err)
		}

		if _, err := s.Close(ctx, orgID, ent.ID); err != nil {
			t.Fatalf("first Close: %v", err)
		}
		got, _ := s.Get(ctx, orgID, ent.ID)
		if got.State != "closed" {
			t.Fatalf("state after Close = %q, want closed", got.State)
		}
		closedAt := got.ClosedAt

		// Close on an already-closed entity must be a no-op (the
		// state='active' guard skips the update).
		if _, err := s.Close(ctx, orgID, ent.ID); err != nil {
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
		if _, err := s.MarkClosed(ctx, orgID, ent.ID); err != nil {
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
		if _, err := s.Close(ctx, orgID, ent.ID); err != nil {
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

	// OwningTeamForEntitySystem resolves the structural owner: the
	// owning_team_id override, or empty when unset. The writer half is
	// covered by the stamp subtest below; here we cover the plain read and
	// the empty fall-through across both dialects.
	t.Run("OwningTeamForEntity_resolves_override_else_empty", func(t *testing.T) {
		s, orgID, seed := mk(t)

		// No override → empty (the router then falls to its prior-task /
		// author-identity tiers).
		plain, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#owner-plain", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed plain entity: %v", err)
		}
		if team, err := s.OwningTeamForEntitySystem(ctx, orgID, plain.ID); err != nil || team != "" {
			t.Errorf("plain entity: got (%q, %v), want (\"\", nil)", team, err)
		}

		// An explicit override resolves to that team.
		ent, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#owner-stamped", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		teamID := seed.Team(t, "Owner")
		if stamped, err := s.StampOwningTeamIfUnsetSystem(ctx, orgID, ent.ID, teamID); err != nil || !stamped {
			t.Fatalf("StampOwningTeamIfUnsetSystem: stamped=%v err=%v", stamped, err)
		}
		team, err := s.OwningTeamForEntitySystem(ctx, orgID, ent.ID)
		if err != nil {
			t.Fatalf("OwningTeamForEntitySystem: %v", err)
		}
		if team != teamID {
			t.Errorf("stamped entity resolved team %q, want %q", team, teamID)
		}

		// Missing entity → empty, not an error.
		if team, err := s.OwningTeamForEntitySystem(ctx, orgID, uuid.New().String()); err != nil || team != "" {
			t.Errorf("missing entity: got (%q, %v), want (\"\", nil)", team, err)
		}
	})

	// StampOwningTeamIfUnsetSystem is the write half of tier 1 — the path that
	// records the commissioning team on a PR the bot opened. The contract it
	// has to hold is "if unset", because two unordered writers (the run that
	// opened the PR, the poller that discovers it) converge only if neither can
	// overwrite the other's answer.
	t.Run("StampOwningTeamIfUnset_stamps_once_then_refuses", func(t *testing.T) {
		s, orgID, seed := mk(t)
		teamA := seed.Team(t, "Commissioning")
		teamB := seed.Team(t, "Interloper")

		ent, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#stamp", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}

		// A NULL owner takes the stamp, and tier 1 reads it straight back.
		stamped, err := s.StampOwningTeamIfUnsetSystem(ctx, orgID, ent.ID, teamA)
		if err != nil || !stamped {
			t.Fatalf("first stamp: got (%v, %v), want (true, nil)", stamped, err)
		}
		if team, err := s.OwningTeamForEntitySystem(ctx, orgID, ent.ID); err != nil || team != teamA {
			t.Errorf("after stamp: got (%q, %v), want (%q, nil)", team, err, teamA)
		}

		// A second writer with a different answer is refused, not applied —
		// this is what makes an operator's transfer permanent and re-delivery
		// of the same PR-open write a no-op.
		stamped, err = s.StampOwningTeamIfUnsetSystem(ctx, orgID, ent.ID, teamB)
		if err != nil || stamped {
			t.Errorf("stamp over an owned entity: got (%v, %v), want (false, nil)", stamped, err)
		}
		if team, _ := s.OwningTeamForEntitySystem(ctx, orgID, ent.ID); team != teamA {
			t.Errorf("owner changed under a second stamp: got %q, want %q", team, teamA)
		}

		// An empty team is the defensive case (a run with no team): no stamp,
		// no error, and the entity keeps its NULL for a later writer.
		fresh, _, err := s.FindOrCreate(ctx, orgID, "github", "owner/repo#stamp-noteam", "pr", "T", "")
		if err != nil {
			t.Fatalf("seed entity: %v", err)
		}
		if stamped, err := s.StampOwningTeamIfUnsetSystem(ctx, orgID, fresh.ID, ""); err != nil || stamped {
			t.Errorf("empty team: got (%v, %v), want (false, nil)", stamped, err)
		}
		if team, _ := s.OwningTeamForEntitySystem(ctx, orgID, fresh.ID); team != "" {
			t.Errorf("empty team left an owner: got %q", team)
		}

		// A missing entity is (false, nil) too — the caller is best-effort and
		// has nothing to do about a row that isn't there.
		if stamped, err := s.StampOwningTeamIfUnsetSystem(ctx, orgID, uuid.New().String(), teamA); err != nil || stamped {
			t.Errorf("missing entity: got (%v, %v), want (false, nil)", stamped, err)
		}
	})

	t.Run("ListActive_filters_by_source_and_state", func(t *testing.T) {
		s, orgID, _ := mk(t)

		gh, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#la-gh", "pr", "GH", "")
		ji, _, _ := s.FindOrCreate(ctx, orgID, "jira", "SKY-la-1", "issue", "JI", "")
		ghClosed, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#la-closed", "pr", "GC", "")
		if _, err := s.MarkClosed(ctx, orgID, ghClosed.ID); err != nil {
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

	t.Run("ClearSnapshotsForSourceSystem_blanks_active_rows_of_one_source", func(t *testing.T) {
		// The event-source pause's half of the re-enable story. Clearing puts
		// each active row back on the tracker's quiet-seed path, so the first
		// cycle after a pause seeds instead of diffing a sprint-old snapshot
		// into a burst of transitions.
		s, orgID, _ := mk(t)

		gh, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#clr-gh", "pr", "GH", "")
		ji, _, _ := s.FindOrCreate(ctx, orgID, "jira", "SKY-clr-1", "issue", "JI", "")
		closed, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#clr-closed", "pr", "GC", "")
		for _, e := range []string{gh.ID, ji.ID, closed.ID} {
			if _, err := s.UpdateSnapshot(ctx, orgID, e, `{"number":7}`); err != nil {
				t.Fatalf("seed snapshot: %v", err)
			}
		}
		if _, err := s.MarkClosed(ctx, orgID, closed.ID); err != nil {
			t.Fatalf("close: %v", err)
		}
		before := mustEntity(t, s, ctx, orgID, "github", "owner/repo#clr-gh")

		cleared, err := s.ClearSnapshotsForSourceSystem(ctx, orgID, "github")
		if err != nil {
			t.Fatalf("ClearSnapshotsForSourceSystem: %v", err)
		}
		if cleared != 1 {
			t.Errorf("cleared %d rows, want 1 — only the ACTIVE github entity", cleared)
		}

		if got := mustEntity(t, s, ctx, orgID, "github", "owner/repo#clr-gh"); got.SnapshotJSON != "" {
			t.Errorf("active github snapshot = %q, want empty", got.SnapshotJSON)
		} else if got.PollSeq != before.PollSeq+1 {
			// The bump is what makes an in-flight cycle's CAS miss instead of
			// restoring the snapshot behind the clear.
			t.Errorf("poll_seq = %d, want %d (one past the pre-clear value)", got.PollSeq, before.PollSeq+1)
		}
		if got := mustEntity(t, s, ctx, orgID, "jira", "SKY-clr-1"); got.SnapshotJSON == "" {
			t.Error("jira snapshot cleared, want untouched — a pause names one source")
		}
		if got := mustEntity(t, s, ctx, orgID, "github", "owner/repo#clr-closed"); got.SnapshotJSON == "" {
			t.Error("closed entity's snapshot cleared, want untouched — it is never diffed again, and the dashboard renders it")
		}
	})

	t.Run("ListActiveTerminalCandidatesSystem_selects_terminal_snapshots_only", func(t *testing.T) {
		s, orgID, _ := mk(t)

		// The whole matrix the reconciliation sweep depends on: each
		// shape of terminal snapshot, each shape of non-terminal one,
		// and the two states that must never surface (already-closed,
		// no snapshot at all).
		seedSnap := func(sourceID, source, snapshot string) string {
			t.Helper()
			e, _, err := s.FindOrCreate(ctx, orgID, source, sourceID, "pr", sourceID, "")
			if err != nil {
				t.Fatalf("create %s: %v", sourceID, err)
			}
			if snapshot != "" {
				if _, err := s.UpdateSnapshot(ctx, orgID, e.ID, snapshot); err != nil {
					t.Fatalf("snapshot %s: %v", sourceID, err)
				}
			}
			return e.ID
		}
		merged := seedSnap("owner/repo#tc-merged", "github", `{"state":"MERGED","merged":true}`)
		closedState := seedSnap("owner/repo#tc-closed", "github", `{"state":"CLOSED","merged":false}`)
		open := seedSnap("owner/repo#tc-open", "github", `{"state":"OPEN","merged":false}`)
		noSnapshot := seedSnap("owner/repo#tc-bare", "github", "")
		// Three Jira shapes, one per arm of the ref match. jiraDone is a
		// snapshot with no status_id (captured before ids were recorded) that
		// the NAME arm has to reach; jiraRenamed carries an id whose status was
		// renamed in Jira since, so only the ID arm reaches it; jiraLive
		// carries both and matches neither.
		jiraDone := seedSnap("PROJ-tc-done", "jira", `{"key":"PROJ-tc-done","status":"Done"}`)
		jiraRenamed := seedSnap("PROJ-tc-renamed", "jira", `{"key":"PROJ-tc-renamed","status":"Complete","status_id":"10001"}`)
		jiraLive := seedSnap("PROJ-tc-live", "jira", `{"key":"PROJ-tc-live","status":"In Progress","status_id":"10003"}`)
		alreadyClosed := seedSnap("owner/repo#tc-gone", "github", `{"state":"MERGED","merged":true}`)
		if _, err := s.MarkClosed(ctx, orgID, alreadyClosed); err != nil {
			t.Fatalf("close: %v", err)
		}

		doneRefs := []domain.JiraStatusRef{{ID: "10001", Name: "Done"}, {Name: "Won't Do"}}
		got, err := s.ListActiveTerminalCandidatesSystem(ctx, orgID, doneRefs, 0)
		if err != nil {
			t.Fatalf("ListActiveTerminalCandidatesSystem: %v", err)
		}
		ids := map[string]bool{}
		for _, e := range got {
			ids[e.ID] = true
		}
		for _, want := range []struct {
			id, why string
		}{
			{merged, "a merged PR"},
			{closedState, "a CLOSED PR"},
			{jiraDone, "a Jira issue in a done status, matched by name because its snapshot predates status ids"},
			{jiraRenamed, "a Jira issue whose done status was renamed, matched by id"},
		} {
			if !ids[want.id] {
				t.Errorf("%s is missing; its entity row is stranded active and nothing else repairs it", want.why)
			}
		}
		for _, skip := range []struct {
			id, why string
		}{
			{open, "an open PR"},
			{noSnapshot, "an entity with no stored snapshot"},
			{jiraLive, "a Jira issue in a live status"},
			{alreadyClosed, "an entity already closed"},
		} {
			if ids[skip.id] {
				t.Errorf("%s surfaced as a terminal candidate; the sweep would close live work", skip.why)
			}
		}

		// No configured done statuses means no Jira row can be terminal —
		// and must not become a syntax error on the way to saying so.
		got, err = s.ListActiveTerminalCandidatesSystem(ctx, orgID, nil, 0)
		if err != nil {
			t.Fatalf("ListActiveTerminalCandidatesSystem(no jira statuses): %v", err)
		}
		for _, e := range got {
			if e.Source == "jira" {
				t.Errorf("jira entity %s surfaced with no configured done statuses", e.ID)
			}
		}

		// A ref set with only ids emits no name arm, and one with only names
		// emits no id arm. Neither may become a syntax error, and each must
		// still reach the rows its own half addresses.
		for _, half := range []struct {
			name string
			refs []domain.JiraStatusRef
			want string
		}{
			{"ids only", []domain.JiraStatusRef{{ID: "10001"}}, jiraRenamed},
			{"names only", []domain.JiraStatusRef{{Name: "Done"}}, jiraDone},
		} {
			got, err := s.ListActiveTerminalCandidatesSystem(ctx, orgID, half.refs, 0)
			if err != nil {
				t.Fatalf("ListActiveTerminalCandidatesSystem(%s): %v", half.name, err)
			}
			found := false
			for _, e := range got {
				if e.ID == half.want {
					found = true
				}
			}
			if !found {
				t.Errorf("%s ref set did not reach the row it addresses", half.name)
			}
		}

		// The limit bounds the batch; the rest is reached on later passes.
		got, err = s.ListActiveTerminalCandidatesSystem(ctx, orgID, []domain.JiraStatusRef{{Name: "Done"}}, 1)
		if err != nil {
			t.Fatalf("ListActiveTerminalCandidatesSystem(limit): %v", err)
		}
		if len(got) != 1 {
			t.Errorf("limit 1 returned %d rows, want 1", len(got))
		}
	})

	t.Run("Descriptions_dedupes_and_skips_empty", func(t *testing.T) {
		s, orgID, _ := mk(t)

		withDesc, _, _ := s.FindOrCreate(ctx, orgID, "github", "owner/repo#d1", "pr", "T", "")
		if _, err := s.UpdateDescription(ctx, orgID, withDesc.ID, "rich body"); err != nil {
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
