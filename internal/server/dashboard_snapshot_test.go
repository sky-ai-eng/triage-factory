package server

import (
	"context"
	"encoding/json"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// These tests cover patchPRSnapshotDraft directly; the route that calls it
// after the GitHub mutation is covered in dashboard_pr_routes_test.go.

// seedPRSnapshot creates a github entity with the given PRSnapshot and
// returns it as the store reads it back.
func seedPRSnapshot(t *testing.T, s *Server, owner, repo string, number int, snap domain.PRSnapshot) *domain.Entity {
	t.Helper()
	sourceID := owner + "/" + repo + "#" + itoa(number)
	entities := sqlitestore.New(s.db).Entities
	entity, _, err := entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "https://github.com", sourceID, "", "pr", snap.Title, snap.URL)
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if ok, err := entities.UpdateSnapshotCASSystem(context.Background(), runmode.LocalDefaultOrgID, entity.ID, string(data), entity.PollSeq); err != nil || !ok {
		t.Fatalf("seed snapshot: ok=%v err=%v", ok, err)
	}
	stored, err := entities.Get(context.Background(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil || stored == nil {
		t.Fatalf("read seeded entity: %v", err)
	}
	return stored
}

func itoa(n int) string {
	// Avoid importing strconv just for a test helper.
	if n == 0 {
		return "0"
	}
	var out []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	if neg {
		out = append([]byte{'-'}, out...)
	}
	return string(out)
}

// readPRSnapshot fetches and decodes the current snapshot of the entity.
func readPRSnapshot(t *testing.T, s *Server, entityID string) domain.PRSnapshot {
	t.Helper()
	entity, err := sqlitestore.New(s.db).Entities.Get(context.Background(), runmode.LocalDefaultOrgID, entityID)
	if err != nil {
		t.Fatalf("read entity: %v", err)
	}
	if entity == nil {
		t.Fatalf("entity not found: %s", entityID)
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	return snap
}

func TestPatchPRSnapshotDraft_FlipsIsDraft(t *testing.T) {
	s := newTestServer(t)
	entities := sqlitestore.New(s.db).Entities

	entity := seedPRSnapshot(t, s, "sky-ai-eng", "triage-factory", 42, domain.PRSnapshot{
		Number:  42,
		Title:   "Add toast notifications",
		Repo:    "sky-ai-eng/triage-factory",
		URL:     "https://github.com/sky-ai-eng/triage-factory/pull/42",
		State:   "OPEN",
		IsDraft: true, // start as draft
		HeadSHA: "abc123",
	})

	// Draft → ready
	snap, err := patchPRSnapshotDraft(context.Background(), entities, runmode.LocalDefaultOrgID, entity, false)
	if err != nil {
		t.Fatalf("patch draft=false: %v", err)
	}
	if snap.IsDraft {
		t.Errorf("returned snapshot IsDraft=true after patching to false")
	}
	if got := readPRSnapshot(t, s, entity.ID); got.IsDraft {
		t.Errorf("expected IsDraft=false after patch, got true")
	}

	// Ready → draft
	entity, err = entities.Get(context.Background(), runmode.LocalDefaultOrgID, entity.ID)
	if err != nil {
		t.Fatalf("re-read entity: %v", err)
	}
	if _, err := patchPRSnapshotDraft(context.Background(), entities, runmode.LocalDefaultOrgID, entity, true); err != nil {
		t.Fatalf("patch draft=true: %v", err)
	}
	if got := readPRSnapshot(t, s, entity.ID); !got.IsDraft {
		t.Errorf("expected IsDraft=true after patch, got false")
	}
}

func TestPatchPRSnapshotDraft_PreservesOtherFields(t *testing.T) {
	// The patch must be surgical — flipping IsDraft shouldn't clobber
	// anything else in the snapshot. If it did, the next poll's diff
	// would see spurious changes and fire junk events.
	s := newTestServer(t)

	original := domain.PRSnapshot{
		Number:       7,
		Title:        "WIP: something",
		Author:       "aidan",
		Repo:         "sky-ai-eng/triage-factory",
		URL:          "https://github.com/sky-ai-eng/triage-factory/pull/7",
		State:        "OPEN",
		IsDraft:      true,
		HeadSHA:      "deadbeef",
		HeadRef:      "aa/thing",
		BaseRef:      "main",
		Labels:       []string{"enhancement", "wip"},
		ReviewCount:  2,
		CommentCount: 5,
		Additions:    120,
		Deletions:    40,
	}
	entity := seedPRSnapshot(t, s, "sky-ai-eng", "triage-factory", 7, original)

	if _, err := patchPRSnapshotDraft(context.Background(), sqlitestore.New(s.db).Entities, runmode.LocalDefaultOrgID, entity, false); err != nil {
		t.Fatalf("patch: %v", err)
	}

	got := readPRSnapshot(t, s, entity.ID)
	if got.IsDraft != false {
		t.Errorf("IsDraft not updated")
	}
	// Spot-check a handful of fields across the struct to catch accidental
	// zero-value resets.
	if got.Title != original.Title ||
		got.Author != original.Author ||
		got.HeadSHA != original.HeadSHA ||
		got.HeadRef != original.HeadRef ||
		got.BaseRef != original.BaseRef ||
		got.ReviewCount != original.ReviewCount ||
		got.CommentCount != original.CommentCount ||
		got.Additions != original.Additions ||
		got.Deletions != original.Deletions ||
		len(got.Labels) != len(original.Labels) {
		t.Errorf("patch clobbered other fields:\n  before: %+v\n  after:  %+v", original, got)
	}
}

func TestPatchPRSnapshotDraft_MalformedSnapshot_ReturnsError(t *testing.T) {
	// A snapshot we can't decode is surprising — bubble the error up
	// rather than silently swallowing, so the caller logs it. The caller
	// already treats the whole patch as best-effort and won't fail the
	// request, so this doesn't hurt the user-facing path.
	s := newTestServer(t)
	entity := &domain.Entity{ID: "unused", SnapshotJSON: "{not json"}

	if _, err := patchPRSnapshotDraft(context.Background(), sqlitestore.New(s.db).Entities, runmode.LocalDefaultOrgID, entity, true); err == nil {
		t.Error("expected error on malformed snapshot, got nil")
	}
}
