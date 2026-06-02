package tracker

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/eventbus"
)

// TestBackfillReviewRequested_EmitsBusEvent pins the SKY-295 contract
// that the tracker no longer creates tasks directly for the
// review-requested backfill path. Instead it publishes a
// github:pr:review_requested event to the bus and the router
// subscribes, evaluating rules and fanning out to per-team tasks.
//
// Without this change the tracker called tasks.FindOrCreateAt with no
// team context, so every backfilled task ended up assigned to the
// org's oldest team — the membership-blind fallback the SQL relied on.
func TestBackfillReviewRequested_EmitsBusEvent(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()

	var (
		mu       sync.Mutex
		received []domain.Event
	)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "test-capture",
		Filter: []string{"github:pr:"},
		Handle: func(evt domain.Event) {
			mu.Lock()
			received = append(received, evt)
			mu.Unlock()
		},
	})

	tracker := &Tracker{pub: bus}

	prCreatedAt := "2026-04-01T10:00:00Z"
	wantOccurred, _ := time.Parse(time.RFC3339, prCreatedAt)
	snap := domain.PRSnapshot{
		Repo:      "owner/repo",
		Number:    42,
		Author:    "alice",
		Title:     "Backfill PR",
		HeadSHA:   "abc123",
		Labels:    []string{"ready"},
		CreatedAt: prCreatedAt,
	}
	if err := tracker.backfillReviewRequested("entity-xyz", snap, "bob", ""); err != nil {
		t.Fatalf("backfillReviewRequested: %v", err)
	}

	// Give the bus's subscriber goroutine a moment to drain — the
	// Subscribe → Handle hop is asynchronous. 100ms is generous for
	// an in-memory channel with one event in flight.
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected exactly 1 event published, got %d", len(received))
	}
	got := received[0]
	if got.EventType != domain.EventGitHubPRReviewRequested {
		t.Errorf("event type = %q, want %q", got.EventType, domain.EventGitHubPRReviewRequested)
	}
	if got.EntityID == nil || *got.EntityID != "entity-xyz" {
		t.Errorf("entity_id mismatch: got %v, want entity-xyz", got.EntityID)
	}
	if !got.OccurredAt.Equal(wantOccurred) {
		t.Errorf("OccurredAt = %v, want %v (PR's CreatedAt)", got.OccurredAt, wantOccurred)
	}
	var meta events.GitHubPRReviewRequestedMetadata
	if err := json.Unmarshal([]byte(got.MetadataJSON), &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta.Author != "alice" {
		t.Errorf("metadata.Author = %q, want alice", meta.Author)
	}
	if meta.Repo != "owner/repo" || meta.PRNumber != 42 {
		t.Errorf("metadata repo/number mismatch: %+v", meta)
	}
}

// TestBackfillReviewRequested_MissingCreatedAt_LeavesOccurredAtZero
// covers the degraded path where the GraphQL response was missing or
// unparseable. The router falls back to the event's CreatedAt when
// OccurredAt is zero, so propagating zero is the right signal — the
// router doesn't need a synthesized "now" from the tracker.
func TestBackfillReviewRequested_MissingCreatedAt_LeavesOccurredAtZero(t *testing.T) {
	bus := eventbus.New()
	defer bus.Close()

	var (
		mu       sync.Mutex
		received []domain.Event
	)
	bus.Subscribe(eventbus.Subscriber{
		Name:   "test-capture",
		Filter: []string{"github:pr:"},
		Handle: func(evt domain.Event) {
			mu.Lock()
			received = append(received, evt)
			mu.Unlock()
		},
	})

	tracker := &Tracker{pub: bus}
	snap := domain.PRSnapshot{
		Repo:   "owner/repo",
		Number: 99,
		Author: "alice",
		// CreatedAt deliberately empty.
	}
	if err := tracker.backfillReviewRequested("entity-zero", snap, "bob", ""); err != nil {
		t.Fatalf("backfillReviewRequested: %v", err)
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := len(received)
		mu.Unlock()
		if count >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("expected 1 event, got %d", len(received))
	}
	if !received[0].OccurredAt.IsZero() {
		t.Errorf("OccurredAt = %v, want zero (no PR createdAt parsed)", received[0].OccurredAt)
	}
}
