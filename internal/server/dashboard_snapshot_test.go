package server

import (
	"context"
	"encoding/json"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// These tests cover patchPRSnapshotDraft directly rather than the HTTP
// handler, because the handler's GitHub mutation isn't mockable without
// plumbing client injection. The actual new behavior — "after a successful
// draft toggle, the entity's snapshot reflects the new state" — lives in
// this helper; the handler just calls it.

// seedPRSnapshot creates a github entity with the given PRSnapshot and
// returns the source_id the handler uses to look it up.
func seedPRSnapshot(t *testing.T, s *Server, owner, repo string, number int, snap domain.PRSnapshot) string {
	t.Helper()
	sourceID := owner + "/" + repo + "#" + itoa(number)
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", sourceID, "pr", snap.Title, snap.URL)
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	if err := sqlitestore.New(s.db).Entities.UpdateSnapshot(context.Background(), runmode.LocalDefaultOrgID, entity.ID, string(data)); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	return sourceID
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

// readPRSnapshot fetches and decodes the current snapshot for a given
// source_id. Fails the test on any error — the entity should always exist
// after seedPRSnapshot.
func readPRSnapshot(t *testing.T, s *Server, sourceID string) domain.PRSnapshot {
	t.Helper()
	entity, err := sqlitestore.New(s.db).Entities.GetBySource(context.Background(), runmode.LocalDefaultOrgID, "github", sourceID)
	if err != nil {
		t.Fatalf("read entity: %v", err)
	}
	if entity == nil {
		t.Fatalf("entity not found: %s", sourceID)
	}
	var snap domain.PRSnapshot
	if err := json.Unmarshal([]byte(entity.SnapshotJSON), &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	return snap
}

func TestPatchPRSnapshotDraft_FlipsIsDraft(t *testing.T) {
	s := newTestServer(t)

	sourceID := seedPRSnapshot(t, s, "sky-ai-eng", "triage-factory", 42, domain.PRSnapshot{
		Number:  42,
		Title:   "Add toast notifications",
		Repo:    "sky-ai-eng/triage-factory",
		URL:     "https://github.com/sky-ai-eng/triage-factory/pull/42",
		State:   "OPEN",
		IsDraft: true, // start as draft
		HeadSHA: "abc123",
	})

	// Draft → ready
	if err := patchPRSnapshotDraft(context.Background(), sqlitestore.New(s.db).Entities, runmode.LocalDefaultOrgID, sourceID, false); err != nil {
		t.Fatalf("patch draft=false: %v", err)
	}
	if got := readPRSnapshot(t, s, sourceID); got.IsDraft {
		t.Errorf("expected IsDraft=false after patch, got true")
	}

	// Ready → draft
	if err := patchPRSnapshotDraft(context.Background(), sqlitestore.New(s.db).Entities, runmode.LocalDefaultOrgID, sourceID, true); err != nil {
		t.Fatalf("patch draft=true: %v", err)
	}
	if got := readPRSnapshot(t, s, sourceID); !got.IsDraft {
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
	sourceID := seedPRSnapshot(t, s, "sky-ai-eng", "triage-factory", 7, original)

	if err := patchPRSnapshotDraft(context.Background(), sqlitestore.New(s.db).Entities, runmode.LocalDefaultOrgID, sourceID, false); err != nil {
		t.Fatalf("patch: %v", err)
	}

	got := readPRSnapshot(t, s, sourceID)
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

func TestPatchPRSnapshotDraft_MissingEntity_NoError(t *testing.T) {
	// Best-effort contract: if the entity hasn't been discovered yet
	// (user mutated before the first poll), the helper returns nil
	// silently rather than failing the whole request.
	s := newTestServer(t)

	if err := patchPRSnapshotDraft(context.Background(), sqlitestore.New(s.db).Entities, runmode.LocalDefaultOrgID, "sky-ai-eng/triage-factory#999", false); err != nil {
		t.Errorf("expected nil for missing entity, got: %v", err)
	}
}

func TestPatchPRSnapshotDraft_EmptySnapshot_NoError(t *testing.T) {
	// Entity exists (e.g., FindOrCreateEntity ran but UpdateSnapshot
	// hasn't fired yet). Treat as missing — nothing to patch.
	s := newTestServer(t)
	if _, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "sky-ai-eng/triage-factory#100", "pr", "Pending", ""); err != nil {
		t.Fatalf("seed empty entity: %v", err)
	}

	if err := patchPRSnapshotDraft(context.Background(), sqlitestore.New(s.db).Entities, runmode.LocalDefaultOrgID, "sky-ai-eng/triage-factory#100", true); err != nil {
		t.Errorf("expected nil for empty snapshot, got: %v", err)
	}
}

func TestPatchPRSnapshotDraft_MalformedSnapshot_ReturnsError(t *testing.T) {
	// A snapshot we can't decode is surprising — bubble the error up
	// rather than silently swallowing, so the caller logs it. The caller
	// already treats the whole patch as best-effort and won't fail the
	// request, so this doesn't hurt the user-facing path.
	s := newTestServer(t)
	entity, _, err := sqlitestore.New(s.db).Entities.FindOrCreate(context.Background(), runmode.LocalDefaultOrgID, "github", "sky-ai-eng/triage-factory#101", "pr", "Corrupt", "")
	if err != nil {
		t.Fatalf("seed entity: %v", err)
	}
	if err := sqlitestore.New(s.db).Entities.UpdateSnapshot(context.Background(), runmode.LocalDefaultOrgID, entity.ID, "{not json"); err != nil {
		t.Fatalf("seed malformed snapshot: %v", err)
	}

	if err := patchPRSnapshotDraft(context.Background(), sqlitestore.New(s.db).Entities, runmode.LocalDefaultOrgID, "sky-ai-eng/triage-factory#101", true); err == nil {
		t.Error("expected error on malformed snapshot, got nil")
	}
}
