package delegate

import (
	"context"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// stageOne drives the stage-or-deliver primitive with no provenance, which is
// what these tests are about — the live/parked routing itself, not what the
// note was composed from.
func stageOne(t *testing.T, s *Spawner, orgID, conversationID, producer, body string) (delivered bool) {
	t.Helper()
	delivered, _, _ = s.stageOrDeliverInjection(context.Background(), orgID, conversationID, domain.MessageSubtypeInjectionSystemNote, producer, body, domain.NoteProvenance{})
	return delivered
}

// TestStageOrDeliverInjectionPrimitive_LiveStagesAtTheToolBoundary: a warm
// process gets the injection staged and its tool boundary armed, so its next
// tool batch hands the note to the agent; delivered=true. Nothing is sent
// into the running turn.
func TestStageOrDeliverInjectionPrimitive_LiveStagesAtTheToolBoundary(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "run-live", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	s.registerProc(runmode.LocalDefaultOrgID, "run-live", &agentproc.LiveRun{})
	s.getProc("run-live").boundary.take() // armed from registration

	delivered := stageOne(t, s, runmode.LocalDefaultOrgID, "run-live", domain.StagedInjectionProducerPRNewCommits, "the body")
	if !delivered {
		t.Fatal("expected delivered=true for a live run")
	}
	got := s.notesAtToolBatch(runmode.LocalDefaultOrgID, "run-live", "").Text
	if !strings.Contains(got, "<system-note>") || !strings.Contains(got, "the body") {
		t.Errorf("the next tool batch handed over %q, want the note wrapped in <system-note>", got)
	}
}

// TestStageOrDeliverInjectionPrimitive_ParkedStages: with no warm process the bare body is
// persisted to the durable queue; delivered=false.
func TestStageOrDeliverInjectionPrimitive_ParkedStages(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "run-parked", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	delivered := stageOne(t, s, runmode.LocalDefaultOrgID, "run-parked", domain.StagedInjectionProducerPRNewCommits, "staged body")
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
}

// TestStageOrDeliverInjectionPrimitive_BlankBodyNoOp: an empty body stages nothing.
func TestStageOrDeliverInjectionPrimitive_BlankBodyNoOp(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "run-blank", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	if stageOne(t, s, runmode.LocalDefaultOrgID, "run-blank", "p", "") {
		t.Error("blank body must return delivered=false")
	}
	injections, _ := s.stagedInjections.FlushPendingSystem(context.Background(), runmode.LocalDefaultOrgID, "run-blank")
	if len(injections) != 0 {
		t.Errorf("blank body must not stage; got %d", len(injections))
	}
}

// TestStagedInjectionsForEngagement_FlushesAndDrains: the helper bundles every
// staged injection into one wrapped block, oldest content present, and drains the
// queue so a second resume prepends nothing.
func TestStagedInjectionsForEngagement_FlushesAndDrains(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "run-resume", "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	ctx := context.Background()

	stageOne(t, s, runmode.LocalDefaultOrgID, "run-resume", "p", "injection one")
	stageOne(t, s, runmode.LocalDefaultOrgID, "run-resume", "p", "injection two")

	block, _ := s.stagedInjectionsForEngagement(ctx, runmode.LocalDefaultOrgID, "run-resume")
	if !strings.HasPrefix(block, "<system-note>") || !strings.HasSuffix(block, "</system-note>") {
		t.Fatalf("block not wrapped: %q", block)
	}
	if !strings.Contains(block, "injection one") || !strings.Contains(block, "injection two") {
		t.Errorf("block missing a staged injection: %q", block)
	}
	// Destructive: the flush drained the queue.
	if again, _ := s.stagedInjectionsForEngagement(ctx, runmode.LocalDefaultOrgID, "run-resume"); again != "" {
		t.Errorf("second resume should prepend nothing (queue drained), got %q", again)
	}
}

// TestStagedInjectionsForEngagement_NilStoreSafe: a spawner with no staged-injection store
// (partial test bundle) degrades to "" rather than panicking.
func TestStagedInjectionsForEngagement_NilStoreSafe(t *testing.T) {
	s := NewSpawner(nil, db.Stores{}, nil, nil, "")
	if got, _ := s.stagedInjectionsForEngagement(context.Background(), runmode.LocalDefaultOrgID, "r"); got != "" {
		t.Errorf("nil store should yield empty block, got %q", got)
	}
}
