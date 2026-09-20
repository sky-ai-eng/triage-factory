package tracker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
)

// TestBackfillReviewRequestedEvent_Shape pins the event the discovery seed
// commits for a PR found with a TF-known identity already in its
// requested-reviewer list: the type, entity and per-reviewer dedup key the
// router fans a task out on, plus the source-time lower bound the card orders
// by.
//
// It is an event the caller commits, not one this function publishes — the
// stored snapshot is the sole re-emit guard, so an event that reached the bus
// after the seed committed would be one the next cycle could never derive
// again.
func TestBackfillReviewRequestedEvent_Shape(t *testing.T) {
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

	got, err := backfillReviewRequestedEvent("entity-xyz", snap, "bob", "")
	if err != nil {
		t.Fatalf("backfillReviewRequestedEvent: %v", err)
	}
	if got.EventType != domain.EventGitHubPRReviewRequested {
		t.Errorf("event type = %q, want %q", got.EventType, domain.EventGitHubPRReviewRequested)
	}
	if got.EntityID == nil || *got.EntityID != "entity-xyz" {
		t.Errorf("entity_id mismatch: got %v, want entity-xyz", got.EntityID)
	}
	if want := reviewerDedupKey("bob"); got.DedupKey != want {
		t.Errorf("dedup_key = %q, want %q (one task per reviewer)", got.DedupKey, want)
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
	if meta.RequestedLogin != "bob" || meta.RequestedTeam != "" {
		t.Errorf("requested identity = (%q, %q), want (bob, \"\")", meta.RequestedLogin, meta.RequestedTeam)
	}
}

// TestBackfillReviewRequestedEvent_TeamReviewerKeysOnTheTeam pins the other
// identity shape: a requested TEAM keys the event by the team, so a team
// request and a personal one on the same PR are two tasks rather than one.
func TestBackfillReviewRequestedEvent_TeamReviewerKeysOnTheTeam(t *testing.T) {
	got, err := backfillReviewRequestedEvent("entity-team", domain.PRSnapshot{Repo: "owner/repo", Number: 7, Author: "alice"}, "", "org/reviewers")
	if err != nil {
		t.Fatalf("backfillReviewRequestedEvent: %v", err)
	}
	if want := reviewerDedupKey("org/reviewers"); got.DedupKey != want {
		t.Errorf("dedup_key = %q, want %q", got.DedupKey, want)
	}
	var meta events.GitHubPRReviewRequestedMetadata
	if err := json.Unmarshal([]byte(got.MetadataJSON), &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta.RequestedTeam != "org/reviewers" || meta.RequestedLogin != "" {
		t.Errorf("requested identity = (%q, %q), want (\"\", org/reviewers)", meta.RequestedLogin, meta.RequestedTeam)
	}
}

// TestBackfillReviewRequestedEvent_MissingCreatedAt_LeavesOccurredAtZero
// covers the degraded path where the GraphQL response was missing or
// unparseable. The router falls back to the event's CreatedAt when
// OccurredAt is zero, so propagating zero is the right signal — the
// router doesn't need a synthesized "now" from the tracker.
func TestBackfillReviewRequestedEvent_MissingCreatedAt_LeavesOccurredAtZero(t *testing.T) {
	got, err := backfillReviewRequestedEvent("entity-zero", domain.PRSnapshot{
		Repo:   "owner/repo",
		Number: 99,
		Author: "alice",
		// CreatedAt deliberately empty.
	}, "bob", "")
	if err != nil {
		t.Fatalf("backfillReviewRequestedEvent: %v", err)
	}
	if !got.OccurredAt.IsZero() {
		t.Errorf("OccurredAt = %v, want zero (no PR createdAt parsed)", got.OccurredAt)
	}
}
