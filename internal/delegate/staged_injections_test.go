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

// TestStageOrDeliverInjection_LiveSteers: a warm process gets the injection steered in,
// wrapped, fire-and-forget; delivered=true and nothing is persisted.
func TestStageOrDeliverInjection_LiveSteers(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-live", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	fc := &fakeController{}
	s.controller = fc
	s.registerProc(runmode.LocalDefaultOrgID, "run-live", &agentproc.LiveRun{})

	delivered := s.StageOrDeliverInjection(runmode.LocalDefaultOrgID, "run-live", domain.StagedInjectionProducerPRNewCommits, "the body")
	if !delivered {
		t.Fatal("expected delivered=true for a live run")
	}
	waitSteerCalls(t, fc, 1)
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if !strings.Contains(fc.steerText, "<system-note>") || !strings.Contains(fc.steerText, "the body") {
		t.Errorf("steered text missing wrapper/body: %q", fc.steerText)
	}
	// Live delivery must NOT persist to the queue.
	injections, err := s.stagedInjections.FlushPendingSystem(context.Background(), runmode.LocalDefaultOrgID, "run-live")
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(injections) != 0 {
		t.Errorf("live delivery should not stage; flush returned %d", len(injections))
	}
}

// TestStageOrDeliverInjection_ParkedStages: with no warm process the bare body is
// persisted to the durable queue; delivered=false and nothing is steered.
func TestStageOrDeliverInjection_ParkedStages(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-parked", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	fc := &fakeController{}
	s.controller = fc

	delivered := s.StageOrDeliverInjection(runmode.LocalDefaultOrgID, "run-parked", domain.StagedInjectionProducerPRNewCommits, "staged body")
	if delivered {
		t.Error("expected delivered=false for a run with no live process")
	}
	injections, err := s.stagedInjections.FlushPendingSystem(context.Background(), runmode.LocalDefaultOrgID, "run-parked")
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(injections) != 1 || injections[0].Body != "staged body" || injections[0].Producer != domain.StagedInjectionProducerPRNewCommits {
		t.Fatalf("want one staged injection {body:staged body}, got %+v", injections)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if fc.steerCalls != 0 {
		t.Errorf("parked stage must not steer; steerCalls=%d", fc.steerCalls)
	}
}

// TestStageOrDeliverInjection_BlankBodyNoOp: an empty body neither steers nor stages.
func TestStageOrDeliverInjection_BlankBodyNoOp(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-blank", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if s.StageOrDeliverInjection(runmode.LocalDefaultOrgID, "run-blank", "p", "") {
		t.Error("blank body must return delivered=false")
	}
	injections, _ := s.stagedInjections.FlushPendingSystem(context.Background(), runmode.LocalDefaultOrgID, "run-blank")
	if len(injections) != 0 {
		t.Errorf("blank body must not stage; got %d", len(injections))
	}
}

// TestStagedInjectionsForResume_FlushesAndDrains: the resume helper bundles every
// staged injection into one wrapped block, oldest content present, and drains the
// queue so a second resume prepends nothing.
func TestStagedInjectionsForResume_FlushesAndDrains(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "run-resume", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()

	s.StageOrDeliverInjection(runmode.LocalDefaultOrgID, "run-resume", "p", "injection one")
	s.StageOrDeliverInjection(runmode.LocalDefaultOrgID, "run-resume", "p", "injection two")

	block := s.stagedInjectionsForResume(ctx, runmode.LocalDefaultOrgID, "run-resume")
	if !strings.HasPrefix(block, "<system-note>") || !strings.HasSuffix(block, "</system-note>") {
		t.Fatalf("block not wrapped: %q", block)
	}
	if !strings.Contains(block, "injection one") || !strings.Contains(block, "injection two") {
		t.Errorf("block missing a staged injection: %q", block)
	}
	// Destructive: the flush drained the queue.
	if again := s.stagedInjectionsForResume(ctx, runmode.LocalDefaultOrgID, "run-resume"); again != "" {
		t.Errorf("second resume should prepend nothing (queue drained), got %q", again)
	}
}

// TestResumeSystemPrepends_OrdersInjectionsThenLedger: a resumable run with BOTH
// a staged injection AND a human-resolved artifact composes both <system-note>
// blocks ahead of the user's text, staged-injection block first:
// [staged injections][artifact ledger]. Guards the composed order against a
// refactor dropping or reordering a block (the components are tested individually
// elsewhere; this pins SendMessage's assembly).
func TestResumeSystemPrepends_OrdersInjectionsThenLedger(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-compose", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()

	// Artifact ledger: a watermark in the past so the resolved PR lands in it.
	seedMessageAt(t, s, "r-compose", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	approvedPR := domain.NewPullRequestArtifact("o/r", 1, "node1", "h", "main",
		"https://github.com/o/r/pull/1", "PR one", "", false) // draft=false → open (approved)
	seedResolvedArtifact(t, s, "r-compose", approvedPR)

	// Staged injection on the same run.
	s.StageOrDeliverInjection(runmode.LocalDefaultOrgID, "r-compose",
		domain.StagedInjectionProducerPRNewCommits, "the staged injection body")

	run, err := s.agentRuns.GetSystem(ctx, runmode.LocalDefaultOrgID, "r-compose")
	if err != nil || run == nil {
		t.Fatalf("GetSystem: err=%v run=%v", err, run)
	}
	prefix := s.resumeSystemPrepends(ctx, runmode.LocalDefaultOrgID, run)

	inj := strings.Index(prefix, "the staged injection body")
	ledger := strings.Index(prefix, "o/r#1")
	if inj < 0 || ledger < 0 {
		t.Fatalf("a block is missing: injection=%d ledger=%d in %q", inj, ledger, prefix)
	}
	if inj > ledger {
		t.Errorf("staged-injection block must precede the artifact ledger: %q", prefix)
	}
	if strings.Count(prefix, "<system-note>") != 2 {
		t.Errorf("want two distinct <system-note> blocks, got %d: %q", strings.Count(prefix, "<system-note>"), prefix)
	}
}

// TestResumeSystemPrepends_EmptyWhenNothingPending: no staged injection and no
// resolved artifact → empty prefix, so a plain resume prepends nothing.
func TestResumeSystemPrepends_EmptyWhenNothingPending(t *testing.T) {
	database := newDelegateTestDB(t)
	seedRun(t, database, "r-none", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()
	run, err := s.agentRuns.GetSystem(ctx, runmode.LocalDefaultOrgID, "r-none")
	if err != nil || run == nil {
		t.Fatalf("GetSystem: err=%v run=%v", err, run)
	}
	if got := s.resumeSystemPrepends(ctx, runmode.LocalDefaultOrgID, run); got != "" {
		t.Errorf("want empty prefix when nothing pending, got %q", got)
	}
}

// TestStagedInjectionsForResume_NilStoreSafe: a spawner with no staged-injection store
// (partial test bundle) degrades to "" rather than panicking.
func TestStagedInjectionsForResume_NilStoreSafe(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if got := s.stagedInjectionsForResume(context.Background(), runmode.LocalDefaultOrgID, "r"); got != "" {
		t.Errorf("nil store should yield empty block, got %q", got)
	}
}
