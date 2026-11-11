package delegate

import (
	"context"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// The turn-end checkpoint is taken whether or not a boundary's checkpoint
// would be due, and it waits for a host slot rather than skipping, since a
// skip is a window that does not open. A second one over an unchanged tree
// writes nothing and still covers the turn.
func TestCheckpointTurnEnd_WaitsForASlotAndIgnoresTheInterval(t *testing.T) {
	f := newCheckpointFixture(t, "r-ckpt-turn-end")
	c := f.start()
	defer c.stop()

	for range checkpointHostSlots {
		if !f.s.acquireCheckpointSlot() {
			t.Fatal("could not take a host slot")
		}
	}
	done := make(chan bool, 1)
	go func() { done <- c.checkpointTurnEnd(7) }()
	select {
	case got := <-done:
		t.Fatalf("checkpointTurnEnd returned %v with every host slot taken, want it to wait for one", got)
	case <-time.After(100 * time.Millisecond):
	}
	f.s.releaseCheckpointSlot()
	select {
	case got := <-done:
		if !got {
			t.Fatal("checkpointTurnEnd = false once a slot came free, want the tree covered")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("checkpointTurnEnd did not return once a slot came free")
	}
	for range checkpointHostSlots - 1 {
		f.s.releaseCheckpointSlot()
	}

	man, _ := f.blob(t)
	if got := man.positionFor(f.conversationID, nil); got == nil || *got != 7 {
		t.Errorf("checkpoint position = %v, want the turn's newest row, 7", got)
	}
	if !c.checkpointTurnEnd(9) {
		t.Fatal("a turn-end checkpoint over an unchanged tree = false, want the tree covered")
	}
	if got := f.outcomes(checkpointSkippedUnchanged); got != 1 {
		t.Errorf("unchanged skips = %d, want the second checkpoint to write nothing", got)
	}
}

// A steer signal is the doorbell of an engagement waiting warm: it rings the
// engagement and is acked ok.
func TestApplySignal_SteerWakesAWaitingEngagement(t *testing.T) {
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-warm-steer", "", "/tmp/wt")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
	fakeSignals := newFakeConversationSignalStore()
	s.SetConversationSignals(fakeSignals, nil)
	wait := s.warmWaits.Register("r-warm-steer", runmode.LocalDefaultOrgID)
	defer wait.Unregister()

	if _, err := fakeSignals.Insert(context.Background(), runmode.LocalDefaultOrgID, "r-warm-steer", domain.ConversationSignalSteer, "", "me"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	sigs, _ := fakeSignals.ListUnackedForTarget(context.Background(), "me")
	s.applySignal(context.Background(), sigs[0])

	acked, result, err := fakeSignals.AckStatus(context.Background(), sigs[0].ID)
	if err != nil || !acked || result != domain.ConversationSignalAckOK {
		t.Fatalf("ack = (%v,%q,%v), want (true, ok, nil)", acked, result, err)
	}
	select {
	case <-wait.Wake():
	default:
		t.Error("the waiting engagement was not rung")
	}
}

// The rows that open a warm window are the ones a person wrote, delivered as
// a turn's input or as a steer during it.
func TestIsPersonMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  domain.Message
		want bool
	}{
		{"a person's message", domain.Message{Role: "user", UserID: "u1"}, true},
		{"a person's steer", domain.Message{Role: "user", UserID: "u1", Subtype: domain.MessageSubtypeInjectionSteer}, true},
		{"a user row with no author", domain.Message{Role: "user"}, false},
		{"an event injection", domain.Message{Role: "user", UserID: "u1", Subtype: domain.MessageSubtypeInjectionEvent}, false},
		{"the task context", domain.Message{Role: "user", UserID: "u1", Subtype: domain.MessageSubtypeInjectionTaskContext}, false},
		{"a stop note", domain.Message{Role: "user", UserID: "u1", Subtype: domain.MessageSubtypeStopNote}, false},
		{"the agent", domain.Message{Role: "assistant", UserID: "u1"}, false},
	} {
		if got := isPersonMessage(tc.row); got != tc.want {
			t.Errorf("%s: isPersonMessage = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A wait ends for a person's message or for an event folded in place of a run,
// and for nothing else TF stages.
func TestWakesWarmWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  domain.Message
		want bool
	}{
		{"a person's message", domain.Message{Role: "user", UserID: "u1"}, true},
		{"a new-event injection", domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionEvent}, true},
		{"an informational note", domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionSystemNote}, false},
		{"a turn-end nudge", domain.Message{Role: "user", Subtype: domain.MessageSubtypeInjectionNudge}, false},
		{"a user row with no author", domain.Message{Role: "user"}, false},
	} {
		if got := wakesWarmWait(tc.row); got != tc.want {
			t.Errorf("%s: wakesWarmWait = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The transcript notes a person's message when a flush delivers one, and not
// when it delivers only rows TF wrote.
func TestNativeTranscript_NotesAPersonsMessageDelivered(t *testing.T) {
	f := newStallFixture(t, "r-warm-delivered", activityTimings{idle: time.Hour})
	ctx := context.Background()
	org := runmode.LocalDefaultOrgID
	tr := newNativeTranscript(f.s, org, f.conversationID, f.claimID)

	person, err := f.s.conversations.InsertMessageSystem(ctx, org, pendingUserInput(f.conversationID, runmode.LocalDefaultUserID, "please fix it"))
	if err != nil {
		t.Fatalf("queue the person's message: %v", err)
	}
	pending := false
	note, err := f.s.conversations.InsertMessageSystem(ctx, org, &domain.Message{
		ConversationID: f.conversationID, UserID: runmode.LocalDefaultUserID, Role: "user",
		Subtype: domain.MessageSubtypeInjectionEvent, Content: "an event", Delivered: &pending,
	})
	if err != nil {
		t.Fatalf("stage the note: %v", err)
	}

	if _, err := tr.ListForAssembly(ctx, org, f.conversationID); err != nil {
		t.Fatalf("list: %v", err)
	}
	if err := tr.MarkDelivered(ctx, org, f.conversationID, []int{int(note)}, ""); err != nil {
		t.Fatalf("flush the note: %v", err)
	}
	if tr.takePersonDelivered() {
		t.Error("a flush of TF's own note counted as a person's message")
	}
	if err := tr.MarkDelivered(ctx, org, f.conversationID, []int{int(person)}, domain.MessageSubtypeInjectionSteer); err != nil {
		t.Fatalf("flush the person's message: %v", err)
	}
	if !tr.takePersonDelivered() {
		t.Error("a flush of the person's message was not counted")
	}
	if tr.takePersonDelivered() {
		t.Error("the count did not start again after it was read")
	}
}
