package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TestResumabilityFor_AnswersWithSendMessage is the property the read exists
// for: the composer's gate and the send's refusal are the same decision, so
// they cannot disagree about a row.
//
// The failure it forecloses is the one users actually hit — a parked run whose
// workspace never made it looks identical to a warm one from status alone, so
// the composer offered an input that answered every message with a 410. Here
// the same fixture is run with the workspace present and with it removed, and
// both answers have to flip together; a `resumable: true` beside an
// ErrWorkspaceExpired is exactly the drift that would put the dead input back.
//
// Both runtimes, because that agreement is what the read is FOR — and the
// engines do not answer every rung alike, so a case whose answer turns on the
// runtime names it (sdkOnly below) rather than being asserted for one engine
// and assumed for the other.
func TestResumabilityFor_AnswersWithSendMessage(t *testing.T) {
	cases := []struct {
		name            string
		blueprintStatus string
		currentStep     int // the blueprint's position; the seeded run is step 0
		cancelRequested bool
		removeWorkspace bool
		wantOK          bool
		wantReason      string
		wantErr         error
		// sdkOnly marks a refusal only the SDK raises: a native conversation
		// carries its context in `messages`, so a lost workspace costs it
		// uncommitted work rather than the run.
		sdkOnly bool
	}{
		{
			name:            "warm workspace, blueprint finished",
			blueprintStatus: "completed",
			wantOK:          true,
		},
		{
			// The blueprint is still going and a LATER step holds the shared
			// worktree. Nothing about this row says so — it is a cleanly
			// completed step with a warm tree — which is why the server has to
			// answer, and why it must answer the same way the send does.
			name:            "workspace intact, blueprint running a later step",
			blueprintStatus: "running",
			currentStep:     1,
			wantOK:          false,
			wantReason:      ResumeBlockedBlueprintConcluded,
			wantErr:         ErrConversationConcluded,
		},
		{
			name:            "workspace removed",
			blueprintStatus: "completed",
			removeWorkspace: true,
			wantOK:          false,
			wantReason:      ResumeBlockedWorkspaceExpired,
			wantErr:         ErrWorkspaceExpired,
			sdkOnly:         true,
		},
		{
			name:            "workspace intact, blueprint cancelled",
			blueprintStatus: "cancelled",
			wantOK:          false,
			wantReason:      ResumeBlockedBlueprintCancelled,
			wantErr:         ErrBlueprintCancelled,
		},
		{
			// The close transaction's stamp, before (or without) the finalize:
			// cancel_requested is the durable intent, and it refuses under the
			// cancel's own name even while the status still says running.
			name:            "workspace intact, cancel requested on a running blueprint",
			blueprintStatus: "running",
			cancelRequested: true,
			wantOK:          false,
			wantReason:      ResumeBlockedBlueprintCancelled,
			wantErr:         ErrBlueprintCancelled,
		},
	}
	for _, runtime := range []string{"sdk", "native"} {
		t.Run(runtime, func(t *testing.T) {
			for _, tc := range cases {
				if tc.sdkOnly && runtime == "native" {
					// Same fixture, opposite answer: composer live, send
					// accepted.
					tc.wantOK, tc.wantReason, tc.wantErr = true, "", nil
				}
				t.Run(tc.name, func(t *testing.T) {
					paths.SetForTest(t, t.TempDir())
					database := newDelegateTestDB(t)
					const conversationID = "r-resumability"
					wt := t.TempDir()
					seedConversation(t, database, conversationID, "sess-"+conversationID, wt)
					setConversationStatus(t, database, conversationID, "open")
					if _, err := database.Exec(`UPDATE blueprint_runs SET status=?, current_step_index=?, cancel_requested=? WHERE id=?`,
						tc.blueprintStatus, tc.currentStep, tc.cancelRequested, blueprintRunIDForConversation(t, database, conversationID)); err != nil {
						t.Fatalf("set blueprint status: %v", err)
					}
					s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
					if runtime == "native" {
						markNative(t, database, conversationID)
					}
					// An empty blob store: nothing to cold-rehydrate from, so
					// removing the worktree really does end the workspace.
					blobs, err := storage.New()
					if err != nil {
						t.Fatalf("storage.New: %v", err)
					}
					s.SetStorage(blobs)
					if tc.removeWorkspace {
						if err := os.RemoveAll(wt); err != nil {
							t.Fatalf("remove worktree: %v", err)
						}
					}

					conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
					if err != nil || conv == nil {
						t.Fatalf("load run: %v", err)
					}
					ok, reason := s.ResumabilityFor(context.Background(), runmode.LocalDefaultOrgID, conv)
					if ok != tc.wantOK || reason != tc.wantReason {
						t.Errorf("ResumabilityFor = (%v, %q), want (%v, %q)", ok, reason, tc.wantOK, tc.wantReason)
					}

					sendErr := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "pick this up")
					if !errors.Is(sendErr, tc.wantErr) {
						t.Errorf("SendMessage err = %v, want %v", sendErr, tc.wantErr)
					}
					// The two halves of the same answer: a composer the server
					// left live must be one the server would accept a message
					// from, and one it disabled must be one it would refuse.
					if ok != (sendErr == nil) {
						t.Errorf("resumable = %v but SendMessage returned %v — the read and the ladder disagree", ok, sendErr)
					}
				})
			}
		})
	}
}

// TestResumabilityFor_FailedRunIsNotSteerable pins the one status the read is
// asked about that no workspace can rescue: the infrastructure under the run
// died, there is no coherent tree to rehydrate, and the answer is the same
// refusal SendMessage gives.
func TestResumabilityFor_FailedRunIsNotSteerable(t *testing.T) {
	database := newDelegateTestDB(t)
	const conversationID = "r-failed-resumability"
	seedConversation(t, database, conversationID, "sess", t.TempDir())
	setConversationStatus(t, database, conversationID, "failed")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	conv, err := s.conversations.GetSystem(context.Background(), runmode.LocalDefaultOrgID, conversationID)
	if err != nil || conv == nil {
		t.Fatalf("load run: %v", err)
	}
	ok, reason := s.ResumabilityFor(context.Background(), runmode.LocalDefaultOrgID, conv)
	if ok || reason != ResumeBlockedNotSteerable {
		t.Errorf("ResumabilityFor = (%v, %q), want (false, %q)", ok, reason, ResumeBlockedNotSteerable)
	}
	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, conversationID, runmode.LocalDefaultUserID, "hello"); !errors.Is(err, ErrConversationNotSteerable) {
		t.Errorf("SendMessage err = %v, want ErrConversationNotSteerable", err)
	}
}

// TestParkConversationOpen_FencedWithoutSnapshotAnnouncesNothing: a teardown
// fenced before it had any workspace to capture (a stop during setup) owes no
// persist and wrote no status, so it has nothing a watcher needs — and saying
// "resumable" there would enable a composer over a workspace that does not
// exist and never will.
func TestParkConversationOpen_FencedWithoutSnapshotAnnouncesNothing(t *testing.T) {
	s, _, conversationID, _ := setupAdvanceFixture(t, "fenced-no-snapshot")
	hub, captured := capturingHub(t)
	s.wsHub = hub
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)
	s.conversations = &fencedConversationStore{ConversationStore: s.conversations}

	fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		claimID:        "claim-1",
		reason:         db.ParkStopped("user_cancelled", "Cancelled by user"),
	}, "")
	if !fenced {
		t.Fatal("the teardown did not report the fence trip")
	}
	if frames := captured.conversationUpdates(conversationID); len(frames) != 0 {
		t.Errorf("conversation_update frames = %d, want none: %+v", len(frames), frames)
	}

	if published := pub.eventsCopy(); len(published) != 0 {
		t.Errorf("published events = %d, want none", len(published))
	}
}

// TestParkConversationOpen_FencedIdleParkAnnouncesNothing pins the silence of a
// refused park that did capture its workspace.
//
// A refusal means the claim is gone from under this engagement — a successor
// may be running the conversation right now. Repeating `open` there would be a torn-down engagement
// reporting a status the row does not have — and the board writes a frame's
// status onto its card optimistically.
func TestParkConversationOpen_FencedIdleParkAnnouncesNothing(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "fenced-idle")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	s.SetStorage(blobs)
	hub, captured := capturingHub(t)
	s.wsHub = hub
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)

	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "_tfac", "notes.txt"), "half-finished work")
	namespace := taskIDForConversation(t, database, conversationID)
	s.conversations = &fencedConversationStore{ConversationStore: s.conversations}

	fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		claudeCwd:      wt,
		namespace:      namespace,
		claimID:        "claim-1",
		reason:         db.ParkIdle(),
	}, "")
	if !fenced {
		t.Fatal("the teardown did not report the fence trip")
	}
	// The persist was recorded and landed, so silence here is the reason arm
	// doing its job — not the workspace arm failing and taking the
	// announcement with it.
	assertSnapshotState(t, s, namespace, domain.WorkspaceSnapshotWritten, "claim-1")
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace))
	if err != nil {
		t.Fatalf("fixture wrote no snapshot, so this test would pass for the wrong reason: %v", err)
	}
	_ = rc.Close()
	if frames := captured.conversationUpdates(conversationID); len(frames) != 0 {
		t.Errorf("conversation_update frames = %d, want none — the successor's run is not parked: %+v", len(frames), frames)
	}
	if published := pub.eventsCopy(); len(published) != 0 {
		t.Errorf("published events = %d, want none", len(published))
	}
}

// capturingHub returns a hub whose broadcasts are recorded, so a test can read
// the frames a browser would receive.
func capturingHub(t *testing.T) (*websocket.Hub, *capturedEvents) {
	t.Helper()
	hub := websocket.NewHub()
	captured := &capturedEvents{}
	hub.SetBackplane(captured)
	return hub, captured
}

// conversationUpdates returns the conversation_update payloads captured for one
// conversation, in broadcast order.
func (c *capturedEvents) conversationUpdates(conversationID string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, e := range c.events {
		if e.Type != "conversation_update" || e.ConversationID != conversationID {
			continue
		}
		switch data := e.Data.(type) {
		case map[string]any:
			out = append(out, data)
		case map[string]string:
			m := make(map[string]any, len(data))
			for k, v := range data {
				m[k] = v
			}
			out = append(out, m)
		}
	}
	return out
}
