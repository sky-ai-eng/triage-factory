package delegate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/db"
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
// Both runtimes, because the ladder is shared — a native conversation skips the
// SDK's session/worktree/model rungs and answers identically on the two that
// are about the world rather than the row.
func TestResumabilityFor_AnswersWithSendMessage(t *testing.T) {
	cases := []struct {
		name            string
		blueprintStatus string
		removeWorkspace bool
		wantOK          bool
		wantReason      string
		wantErr         error
	}{
		{
			name:            "warm workspace, blueprint finished",
			blueprintStatus: "completed",
			wantOK:          true,
		},
		{
			name:            "workspace removed",
			blueprintStatus: "completed",
			removeWorkspace: true,
			wantOK:          false,
			wantReason:      ResumeBlockedWorkspaceExpired,
			wantErr:         ErrWorkspaceExpired,
		},
		{
			name:            "workspace intact, blueprint cancelled",
			blueprintStatus: "cancelled",
			wantOK:          false,
			wantReason:      ResumeBlockedBlueprintConcluded,
			wantErr:         ErrConversationConcluded,
		},
	}
	for _, runtime := range []string{"sdk", "native"} {
		t.Run(runtime, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					paths.SetForTest(t, t.TempDir())
					database := newDelegateTestDB(t)
					const runID = "r-resumability"
					wt := t.TempDir()
					seedRun(t, database, runID, "sess-"+runID, wt)
					setRunStatus(t, database, runID, "open")
					if _, err := database.Exec(`UPDATE blueprint_runs SET status=? WHERE id=?`,
						tc.blueprintStatus, blueprintRunIDForRun(t, database, runID)); err != nil {
						t.Fatalf("set blueprint status: %v", err)
					}
					s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")
					if runtime == "native" {
						markNative(t, database, runID)
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

					run, err := s.agentRuns.GetSystem(context.Background(), runmode.LocalDefaultOrgID, runID)
					if err != nil || run == nil {
						t.Fatalf("load run: %v", err)
					}
					ok, reason := s.ResumabilityFor(context.Background(), runmode.LocalDefaultOrgID, run)
					if ok != tc.wantOK || reason != tc.wantReason {
						t.Errorf("ResumabilityFor = (%v, %q), want (%v, %q)", ok, reason, tc.wantOK, tc.wantReason)
					}

					sendErr := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, runID, runmode.LocalDefaultUserID, "pick this up")
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
	const runID = "r-failed-resumability"
	seedRun(t, database, runID, "sess", t.TempDir())
	setRunStatus(t, database, runID, "failed")
	s := NewSpawner(database, testSpawnerStores(database), nil, nil, "m")

	run, err := s.agentRuns.GetSystem(context.Background(), runmode.LocalDefaultOrgID, runID)
	if err != nil || run == nil {
		t.Fatalf("load run: %v", err)
	}
	ok, reason := s.ResumabilityFor(context.Background(), runmode.LocalDefaultOrgID, run)
	if ok || reason != ResumeBlockedNotSteerable {
		t.Errorf("ResumabilityFor = (%v, %q), want (false, %q)", ok, reason, ResumeBlockedNotSteerable)
	}
	if err := s.SendMessage(context.Background(), runmode.LocalDefaultOrgID, runID, runmode.LocalDefaultUserID, "hello"); !errors.Is(err, ErrRunNotSteerable) {
		t.Errorf("SendMessage err = %v, want ErrRunNotSteerable", err)
	}
}

// TestParkRunOpen_FencedSnapshotAnnouncesResumable closes the residual window a
// cross-pod stop leaves open. Control parks the row and announces `open`
// seconds before the executor's teardown writes the snapshot, so every watcher
// reads that park as unresumable — truthfully, at the time. When the blob
// lands, the fenced teardown says so on the same conversation_update, and a
// browser attached to some other pod enables its composer without a reload.
//
// The status repeats what the row already has; that is the shape the field was
// chosen for (failure_kind rides the failed status the same way), and it is why
// consumers must merge a repeated `open` idempotently.
func TestParkRunOpen_FencedSnapshotAnnouncesResumable(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, runID, _ := setupAdvanceFixture(t, "fenced-resumable")
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
	namespace := blueprintRunIDForRun(t, database, runID)
	stub := &fencedConversationStore{ConversationStore: s.agentRuns}
	s.agentRuns = stub

	fenced := s.parkRunOpen(liveParkContext{
		orgID:       runmode.LocalDefaultOrgID,
		runID:       runID,
		claudeCwd:   wt,
		namespace:   namespace,
		triggerType: "event",
		claimID:     "claim-1",
		reason:      db.ParkStopped("user_cancelled", "Cancelled by user"),
	}, "")
	if !fenced {
		t.Fatal("the teardown did not report the fence trip")
	}
	// The blob is the precondition for the announcement — without it there is
	// nothing to announce.
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace))
	if err != nil {
		t.Fatalf("fenced teardown wrote no snapshot: %v", err)
	}
	_ = rc.Close()

	frames := captured.conversationUpdates(runID)
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
	meta := decodeRunStatus(t, published[0].MetadataJSON)
	if meta.ConversationID != runID || meta.Status != "open" {
		t.Errorf("metadata = %+v, want the parked status for %s", meta, runID)
	}
	if meta.Resumable == nil || !*meta.Resumable {
		t.Errorf("metadata Resumable = %v, want true", meta.Resumable)
	}
}

// TestParkRunOpen_FencedWithoutSnapshotAnnouncesNothing is the other side of the
// branch: a teardown fenced before it had any workspace to capture (a stop
// during setup) has learned nothing a watcher needs, and saying "resumable"
// there would enable a composer over a workspace that does not exist.
func TestParkRunOpen_FencedWithoutSnapshotAnnouncesNothing(t *testing.T) {
	s, _, runID, _ := setupAdvanceFixture(t, "fenced-no-snapshot")
	hub, captured := capturingHub(t)
	s.wsHub = hub
	pub := &fakeEventPublisher{}
	s.SetEventPublisher(pub)
	s.agentRuns = &fencedConversationStore{ConversationStore: s.agentRuns}

	fenced := s.parkRunOpen(liveParkContext{
		orgID:       runmode.LocalDefaultOrgID,
		runID:       runID,
		triggerType: "event",
		claimID:     "claim-1",
		reason:      db.ParkStopped("user_cancelled", "Cancelled by user"),
	}, "")
	if !fenced {
		t.Fatal("the teardown did not report the fence trip")
	}
	if frames := captured.conversationUpdates(runID); len(frames) != 0 {
		t.Errorf("conversation_update frames = %d, want none: %+v", len(frames), frames)
	}

	if published := pub.eventsCopy(); len(published) != 0 {
		t.Errorf("published events = %d, want none", len(published))
	}
}

// TestParkRunOpen_FencedIdleParkAnnouncesNothing pins the other refusal the
// fence raises, and why the announcement is scoped to a deliberate stop.
//
// An idle park has no outside actor: nobody parked this row on the user's
// behalf, so a refusal means a successor took the conversation and is running
// it right now. Repeating `open` there would be a torn-down engagement
// reporting a status the row does not have — and the board writes a frame's
// status onto its card optimistically.
func TestParkRunOpen_FencedIdleParkAnnouncesNothing(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	setupGitTestEnv(t)
	s, database, runID, _ := setupAdvanceFixture(t, "fenced-idle")
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
	namespace := blueprintRunIDForRun(t, database, runID)
	s.agentRuns = &fencedConversationStore{ConversationStore: s.agentRuns}

	fenced := s.parkRunOpen(liveParkContext{
		orgID:       runmode.LocalDefaultOrgID,
		runID:       runID,
		claudeCwd:   wt,
		namespace:   namespace,
		triggerType: "event",
		claimID:     "claim-1",
		reason:      db.ParkIdle(),
	}, "")
	if !fenced {
		t.Fatal("the teardown did not report the fence trip")
	}
	// The snapshot landed, so silence here is the reason arm doing its job —
	// not the snapshot arm failing and taking the announcement with it.
	rc, err := s.Storage().Get(context.Background(), snapshotKey(runmode.LocalDefaultOrgID, namespace))
	if err != nil {
		t.Fatalf("fixture wrote no snapshot, so this test would pass for the wrong reason: %v", err)
	}
	_ = rc.Close()
	if frames := captured.conversationUpdates(runID); len(frames) != 0 {
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
func (c *capturedEvents) conversationUpdates(runID string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []map[string]any
	for _, e := range c.events {
		if e.Type != "conversation_update" || e.ConversationID != runID {
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
