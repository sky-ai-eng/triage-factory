package delegate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	ghclient "github.com/sky-ai-eng/triage-factory/internal/github"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// claimNext is the dispatcher's next claim, or nil when nothing is claimable.
func claimNext(t *testing.T, s *Spawner) *domain.Conversation {
	t.Helper()
	conv, err := s.conversationQueue.ClaimNextConversation(context.Background(), "stop-wake-exec", 1, db.ClaimPlacement{}, db.DefaultClaimLease)
	if err != nil {
		t.Fatalf("ClaimNextConversation: %v", err)
	}
	return conv
}

// queueMessage puts a person's message on the conversation, unread.
func queueMessage(t *testing.T, s *Spawner, conversationID, text string) {
	t.Helper()
	if _, err := s.conversations.InsertMessageSystem(context.Background(), runmode.LocalDefaultOrgID,
		pendingUserInput(conversationID, runmode.LocalDefaultUserID, text, domain.TurnSettings{})); err != nil {
		t.Fatalf("queue %q: %v", text, err)
	}
}

// TestDispatch_StopDuringBringUpKeepsTheWaitingMessageFromWaking: a person's
// message waits on a conversation whose engagement a stop catches during
// bring-up. No turn ran, so the message was the work the stop ended: the
// conversation parks with the message still unread, and the queue offers it
// to no second claim. A message sent after the stop wakes it.
func TestDispatch_StopDuringBringUpKeepsTheWaitingMessageFromWaking(t *testing.T) {
	fx := newLaunchFixtureWithWorktree(t, "939", "")
	markNative(t, fx.database, fx.conv.ID)
	queueMessage(t, fx.s, fx.conv.ID, "fix the null check")

	fetchEntered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(fetchEntered)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	fx.s.SetRunCredentialResolvers(bringUpResolver{client: ghclient.NewClient(server.URL, "test-token")}, nil, nil)

	conv := fx.conv
	conv.OrgID = runmode.LocalDefaultOrgID
	conv.Runtime = domain.ConversationRuntimeNative
	dispatched := make(chan struct{})
	go func() {
		defer close(dispatched)
		fx.s.dispatchClaimedConversation(context.Background(), &conv, time.Now())
	}()
	select {
	case <-fetchEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("bring-up never reached the PR fetch")
	}
	if err := fx.s.Stop(runmode.LocalDefaultOrgID, conv.ID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop during bring-up: %v", err)
	}
	select {
	case <-dispatched:
	case <-time.After(15 * time.Second):
		t.Fatal("the engagement did not return after its setup was cancelled")
	}

	if got := fx.storedStatus(t); got != domain.StatusOpen {
		t.Fatalf("status = %q, want open (a stop is a park)", got)
	}
	if got := claimNext(t, fx.s); got != nil {
		t.Fatalf("ClaimNextConversation = %s, want nothing: the message in front of the stop does not wake the conversation", got.ID)
	}
	if got := pendingRows(t, fx.s, conv.ID); len(got) != 1 || got[0].Content != "fix the null check" {
		t.Errorf("pending rows = %+v, want the stopped message still unread", got)
	}

	queueMessage(t, fx.s, conv.ID, "actually, fix the test")
	if got := claimNext(t, fx.s); got == nil || got.ID != conv.ID {
		t.Fatalf("ClaimNextConversation after a follow-up = %+v, want conversation %s", got, conv.ID)
	}
}

// TestDispatchResumeClaim_AStopBeforeItTakesTheMessageKeepsItFromWaking: an
// SDK resume a stop reaches before it takes the person's message parks the
// conversation, and the message, which no turn read, brings no second claim.
// The follow-up that wakes it hands the resume the stopped message, the stop
// note, and the follow-up, in that order.
func TestDispatchResumeClaim_AStopBeforeItTakesTheMessageKeepsItFromWaking(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	s, database, _, taskID, convID := reactorFixture(t, "sdk-resume-stopped", 3, "open", "")
	giveConversationResumeState(t, database, convID)
	if err := s.SendMessage(ctx, org, convID, runmode.LocalDefaultUserID, "fix the null check", domain.TurnSettings{}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	conv := claimNext(t, s)
	if conv == nil || conv.ID != convID {
		t.Fatalf("ClaimNextConversation = %+v, want conversation %s", conv, convID)
	}
	conv.OrgID = org
	if err := s.Stop(org, convID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	stopped, cancel := context.WithCancel(ctx)
	cancel()
	s.dispatchResumeClaim(stopped, conv, ptrTask(loadTask(t, s, taskID)), "fix the null check", runmode.LocalDefaultUserID)

	if st := storedStatus(t, database, convID); st != domain.StatusOpen {
		t.Fatalf("stored status = %q, want open (a stop is a park)", st)
	}
	if got := claimNext(t, s); got != nil {
		t.Fatalf("ClaimNextConversation = %s, want nothing: the message in front of the stop does not wake the conversation", got.ID)
	}

	if err := s.SendMessage(ctx, org, convID, runmode.LocalDefaultUserID, "actually, fix the test", domain.TurnSettings{}); err != nil {
		t.Fatalf("SendMessage after the stop: %v", err)
	}
	msg, _, ok, err := s.pendingInput.Peek(ctx, org, convID)
	want := "fix the null check" + db.PendingInputSeparator + stopNoteByUser + db.PendingInputSeparator + "actually, fix the test"
	if err != nil || !ok || msg != want {
		t.Errorf("Peek = (%q, %v, %v), want %q", msg, ok, err, want)
	}
}

// TestStop_ANativeTurnStoppedWithAMessageWaitingRunsItNext: a person's message
// sent while a native turn runs waits behind it, and a stop of that turn ends
// the turn, not the message. The conversation parks and the message wakes it
// for the next engagement, which is how a person redirects a working agent.
func TestStop_ANativeTurnStoppedWithAMessageWaitingRunsItNext(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	org := runmode.LocalDefaultOrgID
	s, database, conversationID, taskID := setupAdvanceFixture(t, "native-redirect")
	markNative(t, database, conversationID)
	claimID := markEngaged(t, database, conversationID)

	queueMessage(t, s, conversationID, "do this instead")
	if err := s.Stop(org, conversationID, runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	namespace := blueprintRunIDForConversation(t, database, conversationID)
	if fenced := s.recordNativeResult(context.Background(), org, conversationID, loadTask(t, s, taskID),
		runConfig{orgID: org, claimID: claimID, blueprintRunID: namespace},
		namespace, t.TempDir(), runmode.LocalDefaultUserID, time.Now(),
		agentloop.Result{Kind: agentloop.ResultCancelled, Err: context.Canceled}, nil).fenced; fenced {
		t.Fatal("the stopped turn's teardown reported a fence trip while holding the claim")
	}

	if st := storedStatus(t, database, conversationID); st != domain.StatusOpen {
		t.Fatalf("stored status = %q, want open (a stop is a park)", st)
	}
	if got := claimNext(t, s); got == nil || got.ID != conversationID {
		t.Fatalf("ClaimNextConversation = %+v, want conversation %s: the message waiting behind the stopped turn runs next", got, conversationID)
	}
}

// TestStop_TheNextNativeEngagementReadsTheStopWhereItFell: a native
// conversation stopped with a person's message unread and nothing running,
// then sent a follow-up, hands its next engagement the stopped message, the
// stop note, and the follow-up in that order, all three still to be drained.
func TestStop_TheNextNativeEngagementReadsTheStopWhereItFell(t *testing.T) {
	org := runmode.LocalDefaultOrgID
	ctx := context.Background()
	database := newDelegateTestDB(t)
	seedConversation(t, database, "r-stop-wake", "", "/tmp/wt-stop-wake")
	markNative(t, database, "r-stop-wake")
	if _, err := database.Exec(`UPDATE conversations SET status = 'open' WHERE id = 'r-stop-wake'`); err != nil {
		t.Fatalf("park: %v", err)
	}
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "claude-sonnet-4-6")

	queueMessage(t, s, "r-stop-wake", "fix the null check")
	if err := s.Stop(org, "r-stop-wake", runmode.LocalDefaultUserID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := s.SettleTaskStops(ctx, org, loadConversation(t, s, "r-stop-wake").TaskID); err != nil {
		t.Fatalf("settle the stop: %v", err)
	}
	if got := claimNext(t, s); got != nil {
		t.Fatalf("ClaimNextConversation = %s, want nothing before the follow-up", got.ID)
	}
	if err := s.SendMessage(ctx, org, "r-stop-wake", runmode.LocalDefaultUserID, "actually, fix the test", domain.TurnSettings{}); err != nil {
		t.Fatalf("follow-up after the stop: %v", err)
	}

	rows, err := s.conversations.ListForAssemblySystem(ctx, org, "r-stop-wake")
	if err != nil {
		t.Fatalf("list for assembly: %v", err)
	}
	var got []string
	for _, r := range rows {
		if r.Role == "user" && (agentloop.IsHumanInput(r) || r.Subtype == domain.MessageSubtypeStopNote) {
			got = append(got, r.Content)
		}
	}
	want := []string{"fix the null check", stopNoteByUser, "actually, fix the test"}
	if len(got) != len(want) {
		t.Fatalf("assembled person rows and stop notes = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("assembled row %d = %q, want %q (all: %q)", i, got[i], want[i], got)
		}
	}
	if pending := pendingRows(t, s, "r-stop-wake"); len(pending) != 2 {
		t.Errorf("pending rows = %+v, want the stopped message and the follow-up, both for the next engagement to drain", pending)
	}
}
