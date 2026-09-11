package delegate

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestWaitForDispatches_JoinsADetachedTerminalWrite is the shutdown-ordering
// regression: a dispatch goroutine reaches the reactor only AFTER its context
// is cancelled (that write is detached on purpose, so a shutdown can never
// strand a blueprint mid-finalize), and the join must not return until that
// write has landed. Without it the shutdown path closes the pools underneath
// the write and it fails with "database is closed" — an error no context
// plumbing can catch, because the context it runs on is never cancelled.
//
// The goroutine registers on dispatchWG by hand because that is exactly what
// drainConversationQueue does around its own dispatch; running a real one to
// the reactor would need a real agent, and a real one cancelled this early
// returns at its first pre-agent gate without writing anything at all.
func TestWaitForDispatches_JoinsADetachedTerminalWrite(t *testing.T) {
	s, database, brID, _, step0ConversationID := reactorFixture(t, "shutdown-join", 2, "completed", "continue")
	org := runmode.LocalDefaultOrgID

	stepConversation, err := s.conversations.GetSystem(context.Background(), org, step0ConversationID)
	if err != nil || stepConversation == nil {
		t.Fatalf("load step conversation: (%v, %v)", stepConversation, err)
	}
	stepConversation.TriggerType = "manual"
	stepConversation.CreatorUserID = runmode.LocalDefaultUserID

	ctx, cancel := context.WithCancel(context.Background())
	reactorEntered := make(chan struct{})
	s.dispatchWG.Add(1)
	go func() {
		defer s.dispatchWG.Done()
		<-ctx.Done() // the agent has run; shutdown arrives mid-finalize
		close(reactorEntered)
		s.reactToStepTerminal(ctx, org, mustGetRun(t, s, org, brID), *stepConversation, runConfig{orgID: org}, time.Now())
	}()

	cancel() // SIGTERM: the dispatcher's context goes
	<-reactorEntered

	joinCtx, joinCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer joinCancel()
	if !s.WaitForDispatches(joinCtx) {
		t.Fatal("WaitForDispatches reported a timeout on a dispatch that does finish")
	}

	// Read with no polling and no retry: the join is the only thing that can
	// have made this true, so a wait that returned early fails here.
	if q := queuedStepConversations(t, database, brID); len(q) != 1 || q[0] != 1 {
		t.Fatalf("queued step conversations = %v, want [1] — the join returned before the detached terminal write landed", q)
	}

	// What the shutdown path does next. It is safe only because the join above
	// returned, which is the whole point of the ordering.
	if err := database.Close(); err != nil {
		t.Fatalf("close pool after join: %v", err)
	}
}

// TestWaitForDispatches_ReportsDeadlineExpiry pins the other arm: a dispatch
// that outlasts the caller's deadline reports false rather than blocking
// forever, which is what lets shutdown bound its drain and log why it gave up
// instead of hanging past the orchestrator's grace period.
func TestWaitForDispatches_ReportsDeadlineExpiry(t *testing.T) {
	s := NewSpawner(nil, testSpawnerStores(nil), nil, nil, "")

	release := make(chan struct{})
	s.dispatchWG.Add(1)
	go func() {
		defer s.dispatchWG.Done()
		<-release
	}()
	defer func() {
		close(release)
		s.dispatchWG.Wait()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if s.WaitForDispatches(ctx) {
		t.Fatal("WaitForDispatches reported success while a dispatch was still in flight")
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Errorf("returned after %s, before the deadline it was given", waited)
	}
}

// TestWaitForDispatches_IdleSpawnerDrainsAtOnce: the common shutdown — an
// idle executor reports drained without spending any of its budget, so the
// join costs a healthy pod nothing.
func TestWaitForDispatches_IdleSpawnerDrainsAtOnce(t *testing.T) {
	s := NewSpawner(nil, testSpawnerStores(nil), nil, nil, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if !s.WaitForDispatches(ctx) {
		t.Fatal("an idle spawner reported a drain timeout")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("idle drain took %s; it must not wait on anything", waited)
	}
}

// blockingClaimStore holds the claim loop inside ClaimNextConversation until
// the test releases it, which is how a test puts the dispatcher in the state
// the shutdown race needs: mid-iteration, past its context check, still able
// to register one more dispatch. Everything else delegates to the real store.
type blockingClaimStore struct {
	db.ConversationQueueStore
	entered   chan struct{}
	enterOnce sync.Once
	release   chan struct{}
}

func (b *blockingClaimStore) ClaimNextConversation(ctx context.Context, executorID string, bootEpoch int64, p db.ClaimPlacement) (*domain.Conversation, error) {
	b.enterOnce.Do(func() { close(b.entered) })
	<-b.release
	return nil, nil // queue drained: the loop unwinds and RunDispatcher returns
}

// TestWaitForDispatches_WaitsForTheClaimLoopToStop pins the ordering
// sync.WaitGroup requires. drainConversationQueue re-reads its context only
// between iterations, so a cancel landing mid-iteration still lets it reach
// dispatchWG.Add — and an Add racing a Wait on a zero counter is documented
// misuse: it panics when the WaitGroup catches it, and otherwise lets Wait
// return without joining the dispatch that just started.
//
// So the join must not report drained while the claim loop can still register
// one. Holding the loop inside ClaimNextConversation is what makes that window
// a state the test can observe rather than a race it has to hope for.
func TestWaitForDispatches_WaitsForTheClaimLoopToStop(t *testing.T) {
	database := newDelegateTestDB(t)
	stores := testSpawnerStores(database)
	blocker := &blockingClaimStore{
		ConversationQueueStore: stores.ConversationQueue,
		entered:                make(chan struct{}),
		release:                make(chan struct{}),
	}
	stores.ConversationQueue = blocker
	s := NewSpawner(database, stores, nil, nil, "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.RunDispatcher(ctx, time.Hour) // the boot drain is what reaches the claim

	select {
	case <-blocker.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the dispatcher never reached its first claim")
	}

	cancel() // SIGTERM lands while the loop is mid-iteration

	joined := make(chan bool, 1)
	go func() {
		joinCtx, joinCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer joinCancel()
		joined <- s.WaitForDispatches(joinCtx)
	}()

	select {
	case <-joined:
		t.Fatal("WaitForDispatches reported drained while the claim loop was still inside ClaimNextConversation, where it can still register a dispatch")
	case <-time.After(250 * time.Millisecond):
	}

	close(blocker.release) // the claim returns empty; the loop and RunDispatcher unwind

	select {
	case ok := <-joined:
		if !ok {
			t.Fatal("WaitForDispatches reported a timeout after the dispatcher stopped")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("WaitForDispatches never returned after the dispatcher stopped")
	}
	if s.DispatcherAlive() {
		t.Error("the join returned while the dispatcher loop was still live")
	}
}
