package delegate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedMessageAt inserts a non-user run message stamped at the given time, so
// a test can place the artifact-change ledger watermark (the run's last agent
// activity) before/after the artifacts it seeds.
func seedMessageAt(t *testing.T, s *Spawner, conversationID string, at time.Time) {
	t.Helper()
	if _, err := s.conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: conversationID, Role: "assistant", Content: "agent turn", CreatedAt: at,
	}); err != nil {
		t.Fatalf("seed agent message: %v", err)
	}
}

// seedResolvedArtifact upserts an artifact in a given post-resolution state for
// conversationID (updated_at = now), so the ledger derivation sees it as a human
// resolution after a past watermark.
func seedResolvedArtifact(t *testing.T, s *Spawner, conversationID string, a domain.Artifact) {
	t.Helper()
	a.ConversationID = conversationID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	if _, err := s.artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a); err != nil {
		t.Fatalf("seed artifact %s/%s: %v", a.Kind, a.State, err)
	}
}

// TestArtifactLedgerForResume_DerivesResolvedSinceWatermark: a resumable run's
// ledger bundles exactly the artifacts a human resolved AFTER the agent's last
// activity, skipping unresolved drafts — derived from the artifact rows, no
// ledger table.
func TestArtifactLedgerForResume_DerivesResolvedSinceWatermark(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-led", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// Watermark in the past, so the artifacts seeded "now" all land after it.
	seedMessageAt(t, s, "r-led", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))

	// Resolved: PR approved (open) and a review dismissed → both reported.
	approvedPR := domain.NewPullRequestArtifact("o/r", 1, "node1", "h", "main",
		"https://github.com/o/r/pull/1", "PR one", "", false) // draft=false → state=open
	seedResolvedArtifact(t, s, "r-led", approvedPR)

	dismissedReview := domain.NewReviewArtifact("o/r", 3, "headsha", "r-led")
	dismissedReview.State = domain.ArtifactStateReviewDismissed
	seedResolvedArtifact(t, s, "r-led", dismissedReview)

	// Unresolved draft PR → must NOT appear (still awaiting a human).
	draftPR := domain.NewPullRequestArtifact("o/r", 2, "node2", "h", "main",
		"https://github.com/o/r/pull/2", "PR two", "", true) // draft=true → state=draft
	seedResolvedArtifact(t, s, "r-led", draftPR)

	conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, "r-led")
	if err != nil || conv == nil {
		t.Fatalf("GetSystem: err=%v conversation=%v", err, conv)
	}

	block := s.artifactLedgerForResume(context.Background(), runmode.LocalDefaultOrgID, conv)
	if block == "" {
		t.Fatal("expected a non-empty ledger block")
	}
	if !strings.HasPrefix(block, "<system-note>") || !strings.HasSuffix(block, "</system-note>") {
		t.Errorf("ledger block not wrapped: %q", block)
	}
	if !strings.Contains(block, "o/r#1") {
		t.Errorf("ledger missing the approved PR o/r#1: %q", block)
	}
	if !strings.Contains(block, "o/r#3") {
		t.Errorf("ledger missing the dismissed review on o/r#3: %q", block)
	}
	if strings.Contains(block, "o/r#2") {
		t.Errorf("ledger should not mention the still-draft PR o/r#2: %q", block)
	}
}

// TestArtifactLedgerForResume_ExcludesPreWatermark: an artifact resolved BEFORE
// the agent's last activity (e.g. delivered live while the run was warm) is not
// re-bundled into the ledger — the watermark filters it out.
func TestArtifactLedgerForResume_ExcludesPreWatermark(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-pre", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// Watermark in the FAR future, so the artifact seeded "now" is before it.
	seedMessageAt(t, s, "r-pre", time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC))

	approvedPR := domain.NewPullRequestArtifact("o/r", 1, "node1", "h", "main",
		"https://github.com/o/r/pull/1", "PR one", "", false)
	seedResolvedArtifact(t, s, "r-pre", approvedPR)

	conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, "r-pre")
	if err != nil || conv == nil {
		t.Fatalf("GetSystem: err=%v conversation=%v", err, conv)
	}
	if block := s.artifactLedgerForResume(context.Background(), runmode.LocalDefaultOrgID, conv); block != "" {
		t.Errorf("expected empty ledger (resolution predates the watermark), got %q", block)
	}
}

// waitSteerCalls polls the fake controller until it has seen at least want steer
// calls, or fails. Delivery runs on a detached goroutine (InjectArtifactNote is
// fire-and-forget), so the side effect is asserted by polling rather than a
// synchronous read.
func waitSteerCalls(t *testing.T, fc *fakeController, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		fc.mu.Lock()
		got := fc.steerCalls
		fc.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("steerCalls did not reach %d in time", want)
}

// TestArtifactLedgerForResume_FallsBackToStartedAt: a run with NO agent messages
// yet (LastAgentActivityAtSystem ok=false) uses run.StartedAt as the watermark,
// so a resolution after the run started still lands in the ledger.
func TestArtifactLedgerForResume_FallsBackToStartedAt(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-nomsg", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	// Pin started_at into the past so the artifact seeded "now" is unambiguously
	// after it; insert NO messages, so the watermark must fall back to it.
	if _, err := database.Exec(`UPDATE conversations SET started_at = ? WHERE id = 'r-nomsg'`,
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("set started_at: %v", err)
	}

	approvedPR := domain.NewPullRequestArtifact("o/r", 1, "node1", "h", "main",
		"https://github.com/o/r/pull/1", "PR one", "", false)
	seedResolvedArtifact(t, s, "r-nomsg", approvedPR)

	conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, "r-nomsg")
	if err != nil || conv == nil {
		t.Fatalf("GetSystem: err=%v conversation=%v", err, conv)
	}
	// Sanity: this run genuinely has no agent message, so we exercise the fallback.
	if _, ok, err := s.conversations.LastAgentActivityAtSystem(context.Background(), runmode.LocalDefaultOrgID, "r-nomsg"); err != nil || ok {
		t.Fatalf("precondition: want no agent message (ok=false), got ok=%v err=%v", ok, err)
	}

	block := s.artifactLedgerForResume(context.Background(), runmode.LocalDefaultOrgID, conv)
	if block == "" || !strings.Contains(block, "o/r#1") {
		t.Errorf("expected the resolution in the ledger via the StartedAt fallback, got %q", block)
	}
}

// TestInjectArtifactNote_LiveSteersWrappedNote: resolving an artifact on a live
// run steers a single <system-note>-wrapped, kind-specific note into the warm
// process (on a detached goroutine) and reports dispatched=true.
func TestInjectArtifactNote_LiveSteersWrappedNote(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	fc := &fakeController{}
	s.controller = fc
	// Register a live handle so getProc (the liveness gate) sees the run as warm.
	s.registerProc(runmode.LocalDefaultOrgID, "run-live", &agentproc.LiveRun{})

	art := domain.Artifact{Kind: domain.ArtifactKindPullRequest, State: domain.ArtifactStatePROpen, Target: "o/r#7"}
	if dispatched := s.InjectArtifactNote(runmode.LocalDefaultOrgID, "run-live", art); !dispatched {
		t.Fatal("expected dispatched=true for a live run")
	}
	waitSteerCalls(t, fc, 1)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.steerCalls != 1 || fc.steerConversationID != "run-live" {
		t.Fatalf("steer = {calls:%d conversationID:%q}, want {1 run-live}", fc.steerCalls, fc.steerConversationID)
	}
	if !strings.Contains(fc.steerText, "<system-note>") || !strings.Contains(fc.steerText, "o/r#7") || !strings.Contains(fc.steerText, "approved") {
		t.Errorf("steered text missing wrapper/ref/copy: %q", fc.steerText)
	}
}

// TestInjectArtifactNote_TerminalNoOp: with no live process, InjectArtifactNote
// dispatches nothing (it must not restart the agent) and reports dispatched=false
// — the resolution surfaces via the derived ledger on the next resume instead.
// dispatched=false means no goroutine was spawned, so steerCalls stays 0.
func TestInjectArtifactNote_TerminalNoOp(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	fc := &fakeController{}
	s.controller = fc

	art := domain.Artifact{Kind: domain.ArtifactKindPullRequest, State: domain.ArtifactStatePRClosed, Target: "o/r#7"}
	if dispatched := s.InjectArtifactNote(runmode.LocalDefaultOrgID, "run-dead", art); dispatched {
		t.Error("expected dispatched=false for a run with no live process")
	}
	// No sleep/poll needed: dispatched=false means InjectArtifactNote returned at
	// the getProc gate BEFORE the `go func`, so no goroutine exists to race — the
	// steer count is deterministically 0.
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.steerCalls != 0 {
		t.Errorf("steerCalls = %d, want 0 (terminal run must not be steered/restarted)", fc.steerCalls)
	}
}

// TestInjectArtifactNote_NonResolutionStateNoOp: a non-resolution artifact state
// (an unresolved draft) produces no note and no steer, even on a live run.
func TestInjectArtifactNote_NonResolutionStateNoOp(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	fc := &fakeController{}
	s.controller = fc
	s.registerProc(runmode.LocalDefaultOrgID, "run-live", &agentproc.LiveRun{})

	art := domain.Artifact{Kind: domain.ArtifactKindPullRequest, State: domain.ArtifactStatePRDraft, Target: "o/r#7"}
	if dispatched := s.InjectArtifactNote(runmode.LocalDefaultOrgID, "run-live", art); dispatched {
		t.Error("expected dispatched=false for a non-resolution state")
	}
	// Deterministic: the empty-note guard returns before the `go func`, so no
	// goroutine is spawned and steerCalls is 0 without any wait.
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.steerCalls != 0 {
		t.Errorf("steerCalls = %d, want 0", fc.steerCalls)
	}
}
