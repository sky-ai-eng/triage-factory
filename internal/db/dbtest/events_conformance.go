package dbtest

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
)

// jsonEqual compares two JSON strings for semantic equality.
// Postgres stores metadata_json as JSONB and re-serializes it with
// canonical whitespace on read (`{"k": "v"}` instead of the
// caller-supplied `{"k":"v"}`); SQLite returns the bytes verbatim.
// The store contract is "metadata_json round-trips as JSON," not
// "byte-identical." Use this for every metadata assertion in the
// conformance suite so both backends pass the same checks.
func jsonEqual(t *testing.T, got, want string) bool {
	t.Helper()
	if got == want {
		return true
	}
	var gotV, wantV any
	if err := json.Unmarshal([]byte(got), &gotV); err != nil {
		t.Errorf("json.Unmarshal got=%q: %v", got, err)
		return false
	}
	if err := json.Unmarshal([]byte(want), &wantV); err != nil {
		t.Errorf("json.Unmarshal want=%q: %v", want, err)
		return false
	}
	return reflect.DeepEqual(gotV, wantV)
}

// EventStoreFactory is what a per-backend test file hands to
// RunEventStoreConformance. Returns:
//   - the wired EventStore impl,
//   - the orgID to pass to every call,
//   - an EventStoreSeeder the harness uses to drop entity rows
//     (events FK to entities and the backends seed the entity graph
//     differently — SQLite is open, Postgres needs the full org +
//     auth user FK chain).
type EventStoreFactory func(t *testing.T) (store db.EventStore, orgID string, seed EventStoreSeeder)

// EventStoreSeeder is a bag of callbacks the conformance suite uses
// to stage fixture rows the EventStore doesn't own. Each backend
// implements them against its own SQL.
type EventStoreSeeder struct {
	// Entity inserts an active GitHub PR entity and returns its ID.
	// suffix is appended to source_id so multiple seeds per subtest
	// don't collide on the (source, source_id) unique index.
	Entity func(t *testing.T, suffix string) string
}

// RunEventStoreConformance covers the EventStore contract every
// backend impl must hold:
//
//   - Record + read-back round-trips: caller-supplied IDs stay,
//     generated IDs are non-empty, OccurredAt and EntityID flow
//     through.
//   - LatestForEntityTypeAndDedupKey returns the most recent
//     matching row and discriminates correctly on dedup_key.
//   - LatestForEntityTypeAndDedupKey returns (nil, nil) on miss.
//   - GetMetadataSystem returns "" on missing event (NULL/no-row).
//   - GetMetadataSystem round-trips a real metadata string.
func RunEventStoreConformance(t *testing.T, mk EventStoreFactory) {
	t.Helper()
	ctx := context.Background()

	t.Run("Record_then_Latest_round_trips_supplied_ID", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-roundtrip")
		eid := entityID
		evt := domain.Event{
			ID:           "11111111-1111-1111-1111-111111111111",
			EntityID:     &eid,
			EventType:    domain.EventGitHubPRCICheckPassed,
			MetadataJSON: `{"check_name":"build"}`,
		}
		got, err := s.Record(ctx, orgID, evt)
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		if got != evt.ID {
			t.Errorf("Record returned id=%q, want caller-supplied %q", got, evt.ID)
		}
		latest, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRCICheckPassed, "")
		if err != nil || latest == nil {
			t.Fatalf("Latest: got=%v err=%v", latest, err)
		}
		if latest.ID != evt.ID {
			t.Errorf("Latest.ID = %q, want %q", latest.ID, evt.ID)
		}
		if !jsonEqual(t, latest.MetadataJSON, evt.MetadataJSON) {
			t.Errorf("Latest.MetadataJSON = %q, want JSON-equivalent to %q", latest.MetadataJSON, evt.MetadataJSON)
		}
		if latest.EntityID == nil || *latest.EntityID != entityID {
			t.Errorf("Latest.EntityID = %v, want %q", latest.EntityID, entityID)
		}
	})

	t.Run("Record_generates_UUID_when_ID_empty", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-gen-id")
		eid := entityID
		got, err := s.Record(ctx, orgID, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		if got == "" {
			t.Error("Record returned empty id; want generated UUID")
		}
	})

	t.Run("Record_with_zero_OccurredAt_persists_null", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-zero-occurred")
		eid := entityID
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
			// OccurredAt left zero
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		latest, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPROpened, "")
		if err != nil || latest == nil {
			t.Fatalf("Latest: got=%v err=%v", latest, err)
		}
		if !latest.OccurredAt.IsZero() {
			t.Errorf("OccurredAt = %v, want zero (NULL column)", latest.OccurredAt)
		}
	})

	t.Run("Record_with_OccurredAt_round_trips", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "record-with-occurred")
		eid := entityID
		when := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Hour)
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID:   &eid,
			EventType:  domain.EventGitHubPROpened,
			OccurredAt: when,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		latest, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPROpened, "")
		if err != nil || latest == nil {
			t.Fatalf("Latest: got=%v err=%v", latest, err)
		}
		if !latest.OccurredAt.Equal(when) {
			t.Errorf("OccurredAt = %v, want %v", latest.OccurredAt, when)
		}
	})

	t.Run("Latest_orders_by_insertion_without_sleep", func(t *testing.T) {
		// Regression for the same-tx tiebreaker bug: the Postgres
		// schema defaults created_at to now() which is the tx start
		// timestamp, so two INSERTs in one tx tie on the column and
		// the LIMIT-1 ORDER BY falls through to the UUID tiebreaker
		// (random). SQLite has the analogous problem at second
		// resolution. Bind from Go-side time.Now() so back-to-back
		// records always sort by insertion order without needing a
		// sleep between them.
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "latest-no-sleep")
		eid := entityID
		// 8 back-to-back records with no sleep. With monotonic ns
		// clocks the latest always wins regardless of tiebreaker
		// shape.
		var lastID string
		for i := 0; i < 8; i++ {
			id, err := s.Record(ctx, orgID, domain.Event{
				EntityID: &eid, EventType: domain.EventGitHubPRCICheckPassed,
			})
			if err != nil {
				t.Fatalf("Record %d: %v", i, err)
			}
			lastID = id
		}
		got, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRCICheckPassed, "")
		if err != nil || got == nil {
			t.Fatalf("Latest: got=%v err=%v", got, err)
		}
		if got.ID != lastID {
			t.Errorf("Latest.ID = %q, want %q (last inserted of 8 same-burst records)", got.ID, lastID)
		}
	})

	t.Run("Latest_prefers_occurred_at_when_set", func(t *testing.T) {
		// occurred_at is the source-reported event time (check
		// completion, review submission); when populated it's
		// strictly better than created_at for ordering because it
		// reflects underlying event order rather than our
		// observation order. Pin that the impl honors it: insert
		// an event whose occurred_at is in the past last so it
		// would win on insertion order, but a second event with
		// the further-in-the-past occurred_at must lose to a first
		// event whose occurred_at is newer.
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "latest-occurred")
		eid := entityID
		newer := time.Now().UTC().Truncate(time.Second).Add(-1 * time.Hour)
		older := newer.Add(-1 * time.Hour)
		// Insert the "newer occurrence" event first.
		newerID, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRCICheckPassed,
			OccurredAt: newer,
		})
		if err != nil {
			t.Fatalf("Record newer: %v", err)
		}
		// Insert the "older occurrence" event second — wins on
		// created_at (insertion order) but must lose on occurred_at.
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRCICheckPassed,
			OccurredAt: older,
		}); err != nil {
			t.Fatalf("Record older: %v", err)
		}
		got, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRCICheckPassed, "")
		if err != nil || got == nil {
			t.Fatalf("Latest: got=%v err=%v", got, err)
		}
		if got.ID != newerID {
			t.Errorf("Latest.ID = %q, want %q (newer occurred_at should win regardless of insertion order)", got.ID, newerID)
		}
	})

	t.Run("Latest_returns_most_recent_match", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "latest-most-recent")
		eid := entityID

		// Older matching event.
		olderID, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRCICheckPassed,
		})
		if err != nil {
			t.Fatalf("record older: %v", err)
		}
		// Different type — must not be returned.
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRReviewApproved,
		}); err != nil {
			t.Fatalf("record other-type: %v", err)
		}
		// Sleep so created_at advances past 1s resolution backends.
		time.Sleep(20 * time.Millisecond)
		latestID, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRCICheckPassed,
		})
		if err != nil {
			t.Fatalf("record latest: %v", err)
		}

		got, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRCICheckPassed, "")
		if err != nil || got == nil {
			t.Fatalf("Latest: got=%v err=%v", got, err)
		}
		if got.ID != latestID {
			t.Errorf("Latest.ID = %q, want %q (older was %q)", got.ID, latestID, olderID)
		}
	})

	t.Run("Latest_discriminates_on_dedup_key", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "latest-dedup-key")
		eid := entityID

		bugID, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRLabelAdded, DedupKey: "bug",
		})
		if err != nil {
			t.Fatalf("record bug: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
		// More recent event on a sibling discriminator — must NOT
		// be returned when filtering by dedup_key="bug".
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPRLabelAdded, DedupKey: "help wanted",
		}); err != nil {
			t.Fatalf("record help-wanted: %v", err)
		}

		got, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRLabelAdded, "bug")
		if err != nil || got == nil {
			t.Fatalf("Latest: got=%v err=%v", got, err)
		}
		if got.ID != bugID {
			t.Errorf("Latest.ID = %q, want %q (bug-key match)", got.ID, bugID)
		}
	})

	t.Run("Latest_returns_nil_on_miss", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "latest-miss")
		eid := entityID
		if _, err := s.Record(ctx, orgID, domain.Event{
			EntityID: &eid, EventType: domain.EventGitHubPROpened,
		}); err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := s.LatestForEntityTypeAndDedupKey(ctx, orgID, entityID, domain.EventGitHubPRMerged, "")
		if err != nil {
			t.Fatalf("Latest: %v", err)
		}
		if got != nil {
			t.Errorf("Latest on miss should be nil, got %+v", got)
		}
	})

	t.Run("GetMetadataSystem_returns_metadata", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "get-metadata")
		eid := entityID
		const want = `{"check_name":"build"}`
		eventID, err := s.Record(ctx, orgID, domain.Event{
			EntityID:     &eid,
			EventType:    domain.EventGitHubPRCICheckFailed,
			MetadataJSON: want,
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := s.GetMetadataSystem(ctx, orgID, eventID)
		if err != nil {
			t.Fatalf("GetMetadataSystem: %v", err)
		}
		if !jsonEqual(t, got, want) {
			t.Errorf("GetMetadataSystem = %q, want JSON-equivalent to %q", got, want)
		}
	})

	t.Run("GetMetadataSystem_returns_empty_on_miss", func(t *testing.T) {
		s, orgID, _ := mk(t)
		got, err := s.GetMetadataSystem(ctx, orgID, "00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatalf("GetMetadataSystem: %v", err)
		}
		if got != "" {
			t.Errorf("GetMetadataSystem on miss should be empty, got %q", got)
		}
	})

	t.Run("GetMetadataSystem_empty_on_null_column", func(t *testing.T) {
		// Record an event with empty MetadataJSON — both impls treat
		// "" as a NULL column. GetMetadataSystem returns "" for both
		// "no row" and "NULL metadata" (caller can't distinguish, and
		// the caller's contract is "no metadata to match against").
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "get-metadata-null")
		eid := entityID
		eventID, err := s.Record(ctx, orgID, domain.Event{
			EntityID:  &eid,
			EventType: domain.EventGitHubPROpened,
			// MetadataJSON left empty
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := s.GetMetadataSystem(ctx, orgID, eventID)
		if err != nil {
			t.Fatalf("GetMetadataSystem: %v", err)
		}
		if got != "" {
			t.Errorf("GetMetadataSystem on NULL metadata should be empty, got %q", got)
		}
	})

	t.Run("GetSystem_round_trips_full_event", func(t *testing.T) {
		s, orgID, seed := mk(t)
		entityID := seed.Entity(t, "get-system")
		eid := entityID
		when := time.Now().UTC().Truncate(time.Second).Add(-3 * time.Hour)
		const meta = `{"check_name":"build"}`
		eventID, err := s.Record(ctx, orgID, domain.Event{
			EntityID:     &eid,
			EventType:    domain.EventGitHubPRCICheckFailed,
			DedupKey:     "build",
			MetadataJSON: meta,
			OccurredAt:   when,
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
		got, err := s.GetSystem(ctx, orgID, eventID)
		if err != nil || got == nil {
			t.Fatalf("GetSystem: got=%v err=%v", got, err)
		}
		if got.ID != eventID {
			t.Errorf("ID = %q, want %q", got.ID, eventID)
		}
		if got.EventType != domain.EventGitHubPRCICheckFailed {
			t.Errorf("EventType = %q, want %q", got.EventType, domain.EventGitHubPRCICheckFailed)
		}
		if got.DedupKey != "build" {
			t.Errorf("DedupKey = %q, want %q", got.DedupKey, "build")
		}
		if got.EntityID == nil || *got.EntityID != entityID {
			t.Errorf("EntityID = %v, want %q", got.EntityID, entityID)
		}
		if !got.OccurredAt.Equal(when) {
			t.Errorf("OccurredAt = %v, want %v", got.OccurredAt, when)
		}
		if !jsonEqual(t, got.MetadataJSON, meta) {
			t.Errorf("MetadataJSON = %q, want JSON-equivalent to %q", got.MetadataJSON, meta)
		}
		// The worker relies on OrgID being stamped so HandleEvent has the
		// tenant context.
		if got.OrgID != orgID {
			t.Errorf("OrgID = %q, want %q (stamped for the worker)", got.OrgID, orgID)
		}
	})

	t.Run("GetSystem_returns_nil_on_miss", func(t *testing.T) {
		s, orgID, _ := mk(t)
		got, err := s.GetSystem(ctx, orgID, "00000000-0000-0000-0000-000000000000")
		if err != nil {
			t.Fatalf("GetSystem: %v", err)
		}
		if got != nil {
			t.Errorf("GetSystem on miss should be nil, got %+v", got)
		}
	})
}
