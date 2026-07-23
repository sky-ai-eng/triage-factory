package delegate

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// seedPendingReview upserts a pending review artifact anchored to repo#pr for
// runID, with the given start-review head and (optional) reconciled comment SHAs.
func seedPendingReview(t *testing.T, s *Spawner, runID, repo string, pr int, headSHA string, commentSHAs ...string) {
	t.Helper()
	a := domain.NewReviewArtifact(repo, pr, headSHA, runID)
	if len(commentSHAs) > 0 {
		d, _ := domain.ParseReviewArtifactDetails(a.DetailsJSON)
		for _, sha := range commentSHAs {
			d.StagedComments = append(d.StagedComments, domain.ReviewArtifactComment{Path: "a.go", Body: "x", CommitSHA: sha})
		}
		a.DetailsJSON = domain.MarshalReviewArtifactDetails(d)
	}
	a.RunID = runID
	a.OrgID = runmode.LocalDefaultOrgID
	a.TeamID = runmode.LocalDefaultTeamID
	if _, err := s.artifacts.UpsertSystem(context.Background(), runmode.LocalDefaultOrgID, a); err != nil {
		t.Fatalf("seed pending review: %v", err)
	}
}

// newCommitsEvent builds a github:pr:new_commits bus event for repo#pr advancing
// oldSHA→newSHA.
func newCommitsEvent(repo string, pr int, oldSHA, newSHA string) domain.Event {
	b, _ := json.Marshal(events.GitHubPRNewCommitsMetadata{
		Repo: repo, PRNumber: pr, HeadSHA: newSHA, PrevHeadSHA: oldSHA,
	})
	return domain.Event{
		ID: "evt-1", EventType: domain.EventGitHubPRNewCommits,
		OrgID: runmode.LocalDefaultOrgID, MetadataJSON: string(b),
	}
}

func flushOne(t *testing.T, s *Spawner, runID string) []domain.StagedInjection {
	t.Helper()
	injections, err := s.stagedInjections.FlushPendingSystem(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	return injections
}

// TestHandlePRNewCommits_ParkedBehindStages: a parked (resumable) review run
// behind the new head gets the injection STAGED for its next resume.
func TestHandlePRNewCommits_ParkedBehindStages(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-parked", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	setRunStatus(t, database, "run-parked", "open") // resumable, no warm process
	seedPendingReview(t, s, "run-parked", "o/r", 7, "oldHead")

	s.HandlePRNewCommits(newCommitsEvent("o/r", 7, "oldHead", "newHead"))

	injections := flushOne(t, s, "run-parked")
	if len(injections) != 1 {
		t.Fatalf("want 1 staged injection, got %d", len(injections))
	}
	if injections[0].Producer != domain.StagedInjectionProducerPRNewCommits {
		t.Errorf("producer = %q", injections[0].Producer)
	}
	if !strings.Contains(injections[0].Body, "#7") || !strings.Contains(injections[0].Body, "advanced") {
		t.Errorf("injection body missing PR/advanced copy: %q", injections[0].Body)
	}
}

// TestHandlePRNewCommits_LiveBehindSteers: a live review run behind the new head
// gets the injection STEERED in (not staged).
func TestHandlePRNewCommits_LiveBehindSteers(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-live", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	fc := &fakeController{}
	s.controller = fc
	s.registerProc(runmode.LocalDefaultOrgID, "run-live", &agentproc.LiveRun{})
	seedPendingReview(t, s, "run-live", "o/r", 7, "oldHead")

	s.HandlePRNewCommits(newCommitsEvent("o/r", 7, "oldHead", "newHead"))

	waitSteerCalls(t, fc, 1)
	fc.mu.Lock()
	steer := fc.steerText
	fc.mu.Unlock()
	if !strings.Contains(steer, "<system-note>") || !strings.Contains(steer, "#7") {
		t.Errorf("steered text missing wrapper/PR: %q", steer)
	}
	if injections := flushOne(t, s, "run-live"); len(injections) != 0 {
		t.Errorf("live delivery must not stage; got %d", len(injections))
	}
}

// TestHandlePRNewCommits_AtHeadSkips: the agent already anchored at the new head
// (start anchor) gets no injection.
func TestHandlePRNewCommits_AtHeadSkips(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-current", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	setRunStatus(t, database, "run-current", "open")
	seedPendingReview(t, s, "run-current", "o/r", 7, "newHead") // anchor == the new head

	s.HandlePRNewCommits(newCommitsEvent("o/r", 7, "oldHead", "newHead"))

	if injections := flushOne(t, s, "run-current"); len(injections) != 0 {
		t.Errorf("agent already at head: want no injection, got %d", len(injections))
	}
}

// TestHandlePRNewCommits_ReconciledCommentHeadSkips: a review whose start anchor
// is stale but whose comments were finalize-reconciled to the new head reads as
// current — no injection.
func TestHandlePRNewCommits_ReconciledCommentHeadSkips(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-reconciled", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	setRunStatus(t, database, "run-reconciled", "open")
	// Start head is old, but a staged comment already moved to newHead.
	seedPendingReview(t, s, "run-reconciled", "o/r", 7, "oldHead", "newHead")

	s.HandlePRNewCommits(newCommitsEvent("o/r", 7, "oldHead", "newHead"))

	if injections := flushOne(t, s, "run-reconciled"); len(injections) != 0 {
		t.Errorf("comments reconciled to head: want no injection, got %d", len(injections))
	}
}

// TestHandlePRNewCommits_TerminalNonResumableSkips: a terminal, non-resumable run
// (completed+finish) can never read a staged injection, so none is staged.
func TestHandlePRNewCommits_TerminalNonResumableSkips(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-finished", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	// completed + outcome=finish → resumableState=false.
	if err := s.agentRuns.CompleteSystem(context.Background(), runmode.LocalDefaultOrgID, "run-finished", "completed", 0, 0, 0, 0, "", "", string(domain.RunOutcomeFinish), "", ""); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	seedPendingReview(t, s, "run-finished", "o/r", 7, "oldHead")

	s.HandlePRNewCommits(newCommitsEvent("o/r", 7, "oldHead", "newHead"))

	if injections := flushOne(t, s, "run-finished"); len(injections) != 0 {
		t.Errorf("terminal non-resumable run: want no staged injection (would never flush), got %d", len(injections))
	}
}

// TestHandlePRNewCommits_NoReviewNoOp: a head change on a PR with no pending
// review artifact does nothing (and doesn't panic).
func TestHandlePRNewCommits_NoReviewNoOp(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-noreview", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	setRunStatus(t, database, "run-noreview", "open")

	s.HandlePRNewCommits(newCommitsEvent("o/r", 99, "oldHead", "newHead"))

	if injections := flushOne(t, s, "run-noreview"); len(injections) != 0 {
		t.Errorf("no pending review: want no injection, got %d", len(injections))
	}
}
