package delegate

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeBoundaryProc records what a boundary delivery does to the process, in
// order, and closes interrupted on the interrupt.
type fakeBoundaryProc struct {
	mu          sync.Mutex
	calls       []string
	sent        string
	interrupted chan struct{}
}

func (p *fakeBoundaryProc) Send(_ context.Context, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "send")
	p.sent = text
	return nil
}

func (p *fakeBoundaryProc) Interrupt(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, "interrupt")
	close(p.interrupted)
	return nil
}

// newBoundaryFixture is a conversation with a live handle whose boundary
// delivers into a fake process, and the activity sink reading that process's
// stream, as runLiveAndDrive wires them.
func newBoundaryFixture(t *testing.T, conversationID string) (*Spawner, *activitySink, *fakeBoundaryProc) {
	t.Helper()
	database := newDelegateTestDB(t)
	seedConversation(t, database, conversationID, "sess", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")
	proc := &fakeBoundaryProc{interrupted: make(chan struct{})}
	s.mu.Lock()
	s.procs[conversationID] = &liveRunHandle{
		conversationID: conversationID, orgID: runmode.LocalDefaultOrgID,
		boundary: &toolBoundary{deliver: func() { s.deliverNotesAtBoundary(runmode.LocalDefaultOrgID, conversationID, proc) }},
	}
	s.mu.Unlock()
	sink := newActivitySink(agentproc.NoopSink{}, nil, s.activityFor(conversationID), s.resolvedActivityTimings())
	sink.onToolBoundary = func() { s.toolBoundaryReached(conversationID) }
	return s, sink, proc
}

func boundaryArmed(s *Spawner, conversationID string) bool {
	h := s.getProc(conversationID)
	h.boundary.mu.Lock()
	defer h.boundary.mu.Unlock()
	return h.boundary.armed
}

// TestToolBoundary_NoteWaitsForEveryToolCallThenPausesTheTurn: an armed note
// waits while any tool call is in flight, and once the last one returns it is
// sent into the process before the turn is interrupted, so the interrupted
// turn has the note queued behind it. The flush marks it delivered.
func TestToolBoundary_NoteWaitsForEveryToolCallThenPausesTheTurn(t *testing.T) {
	s, sink, proc := newBoundaryFixture(t, "run-boundary")
	ctx := context.Background()

	sink.OnToolUse("a", "Bash")
	sink.OnToolUse("b", "Read")
	if !s.StageNoteAtToolBoundary(ctx, runmode.LocalDefaultOrgID, "run-boundary", domain.StagedInjectionProducerTaskClosing, "the task is resolved", domain.NoteProvenance{}) {
		t.Fatal("the note was not staged")
	}
	sink.OnToolResult("a")
	if !boundaryArmed(s, "run-boundary") {
		t.Fatal("the note fired with a tool call still in flight")
	}

	sink.OnToolResult("b")
	select {
	case <-proc.interrupted:
	case <-time.After(5 * time.Second):
		t.Fatal("the turn was not interrupted once every tool call returned")
	}
	proc.mu.Lock()
	calls, sent := proc.calls, proc.sent
	proc.mu.Unlock()
	if strings.Join(calls, ",") != "send,interrupt" {
		t.Errorf("calls = %v, want the send before the interrupt", calls)
	}
	if !strings.Contains(sent, "<system-note>") || !strings.Contains(sent, "the task is resolved") {
		t.Errorf("sent %q, want the wrapped note", sent)
	}
	if left, err := s.stagedInjections.FlushPendingSystem(ctx, runmode.LocalDefaultOrgID, "run-boundary"); err != nil || len(left) != 0 {
		t.Errorf("still staged after the delivery: %v (err %v)", left, err)
	}
}

// TestToolBoundary_TurnEndingWithoutOneInterruptsNothing: a turn that ends
// with no tool call returning after the note was armed is not interrupted,
// and the note stays staged for the conversation's next resume.
func TestToolBoundary_TurnEndingWithoutOneInterruptsNothing(t *testing.T) {
	s, sink, _ := newBoundaryFixture(t, "run-no-boundary")
	ctx := context.Background()

	sink.OnToolUse("a", "Bash")
	if !s.StageNoteAtToolBoundary(ctx, runmode.LocalDefaultOrgID, "run-no-boundary", domain.StagedInjectionProducerTaskClosing, "the task is resolved", domain.NoteProvenance{}) {
		t.Fatal("the note was not staged")
	}
	sink.OnTurnEnd()

	// Still armed is what says the turn's end reached no boundary: reaching
	// one disarms before the delivery starts, which then runs on its own.
	if !boundaryArmed(s, "run-no-boundary") {
		t.Fatal("the turn's end delivered the note")
	}
	left, err := s.stagedInjections.FlushPendingSystem(ctx, runmode.LocalDefaultOrgID, "run-no-boundary")
	if err != nil || len(left) != 1 || left[0].Body != "the task is resolved" {
		t.Errorf("staged after the turn = %+v (err %v), want the note", left, err)
	}
}
