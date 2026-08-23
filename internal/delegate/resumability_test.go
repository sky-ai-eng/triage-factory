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

// TestParkConversationOpen_FencedSnapshotAnnouncesResumable closes the residual window a
// cross-pod stop leaves open. Control parks the row and announces `open`
// before the executor holding the workspace has recorded that it owes a
// persist for it, so every watcher reads that park as unresumable — truthfully,
// at the time. The fenced teardown records the persist and says so on the same
// conversation_update, and a browser attached to some other pod enables its
// composer without a reload.
//
// The state record is the precondition, not the blob: the wake gate accepts a
// pending persist, so the announcement is true from the moment the record
// lands, and waiting for the upload would hold it back for nothing.
//
// The status repeats what the row already has; that is the shape the field was
// chosen for (failure_kind rides the failed status the same way), and it is why
// consumers must merge a repeated `open` idempotently.
func TestParkConversationOpen_FencedSnapshotAnnouncesResumable(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, conversationID, _ := setupAdvanceFixture(t, "fenced-resumable")
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
	namespace := blueprintRunIDForConversation(t, database, conversationID)
	stub := &fencedConversationStore{ConversationStore: s.conversations}
	s.conversations = stub

	fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		claudeCwd:      wt,
		namespace:      namespace,
		triggerType:    "event",
		claimID:        "claim-1",
		reason:         db.ParkStopped("user_cancelled", "Cancelled by user"),
	}, "")
	if !fenced {
		t.Fatal("the teardown did not report the fence trip")
	}
	// The record is what makes the announcement true and the blob follows it;
	// both are asserted, because an announcement with neither behind it would
	// enable a composer over a workspace nobody is producing.
	assertSnapshotState(t, s, namespace, domain.WorkspaceSnapshotWritten, "claim-1")
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace))
	if err != nil {
		t.Fatalf("fenced teardown wrote no snapshot: %v", err)
	}
	_ = rc.Close()

	frames := captured.conversationUpdates(conversationID)
	if len(frames) != 1 {
		t.Fatalf("conversation_update frames = %d, want 1 (the fenced park records no status of its own; this one carries the workspace news)", len(frames))
	}
	if got := frames[0]["status"]; got != "open" {
		t.Errorf("frame status = %v, want open — resumability is an attribute of the park, not a new situation", got)
	}
	if got := frames[0]["resumable"]; got != true {
		t.Errorf("frame resumable = %v, want true", got)
	}

	published := pub.eventsCopy()
	if len(published) != 1 {
		t.Fatalf("published events = %d, want 1", len(published))
	}
	meta := decodeConversationStatus(t, published[0].MetadataJSON)
	if meta.ConversationID != conversationID || meta.Status != "open" {
		t.Errorf("metadata = %+v, want the parked status for %s", meta, conversationID)
	}
	if meta.Resumable == nil || !*meta.Resumable {
		t.Errorf("metadata Resumable = %v, want true", meta.Resumable)
	}
}

// TestParkConversationOpen_FencedWithoutSnapshotAnnouncesNothing is the other side of the
// branch: a teardown fenced before it had any workspace to capture (a stop
// during setup) owes no persist, so it has learned nothing a watcher needs —
// and saying "resumable" there would enable a composer over a workspace that
// does not exist and never will.
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
		triggerType:    "event",
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

// TestParkConversationOpen_FencedIdleParkAnnouncesNothing pins the other refusal the
// fence raises, and why the announcement is scoped to a deliberate stop.
//
// An idle park has no outside actor: nobody parked this row on the user's
// behalf, so a refusal means a successor took the conversation and is running
// it right now. Repeating `open` there would be a torn-down engagement
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
	namespace := blueprintRunIDForConversation(t, database, conversationID)
	s.conversations = &fencedConversationStore{ConversationStore: s.conversations}

	fenced := s.parkConversationOpen(context.Background(), liveParkContext{
		orgID:          runmode.LocalDefaultOrgID,
		conversationID: conversationID,
		claudeCwd:      wt,
		namespace:      namespace,
		triggerType:    "event",
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
