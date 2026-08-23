package server

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/storage"
)

// readConversation issues the conversation detail read and decodes it.
func readConversation(t *testing.T, s *Server, conversationID string) map[string]any {
	t.Helper()
	rec := doJSON(t, s, "GET", "/api/agent/conversations/"+conversationID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode conversation: %v", err)
	}
	return out
}

// withEmptyBlobStore wires a blob store with nothing in it, so a conversation whose
// worktree is also gone is unrecoverable for real rather than
// unverifiable (an unwired store answers "recoverable" by design).
func withEmptyBlobStore(t *testing.T, s *Server) *delegate.Spawner {
	t.Helper()
	paths.SetForTest(t, t.TempDir())
	spawner := delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "claude-sonnet-4-6")
	blobs, err := storage.New()
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	spawner.SetStorage(blobs)
	s.SetSpawner(spawner)
	return spawner
}

// TestHandleAgentStatus_ResumableWorkspaceExpired: the read tells the client
// what only the server can know. The row is parked and looks resumable from
// its status — which is all the client can see — but its workspace is gone, so
// the composer this read gates must come up disabled rather than accept a
// message that answers 410.
//
// SDK, because that is the runtime the answer belongs to: its session
// transcript lived inside the snapshot. A native row in the same state is NOT
// expired — the case below covers it.
func TestHandleAgentStatus_ResumableWorkspaceExpired(t *testing.T) {
	s := newTestServer(t)
	withEmptyBlobStore(t, s)
	conversationID := seedSteerConversation(t, s.db, "resgone", "open")
	// Everything the SDK's own rungs demand is present, and the recorded
	// worktree is simply not on disk — so the ladder reaches the workspace
	// question rather than stopping above it.
	execSQL(t, s.db, `UPDATE conversations SET sdk_session_id='sess-resgone', model='m', worktree_path=? WHERE id=?`,
		filepath.Join(t.TempDir(), "swept-away"), conversationID)

	got := readConversation(t, s, conversationID)
	if got["resumable"] != false {
		t.Errorf("resumable = %v, want false", got["resumable"])
	}
	if got["resume_blocked_reason"] != delegate.ResumeBlockedWorkspaceExpired {
		t.Errorf("resume_blocked_reason = %v, want %q", got["resume_blocked_reason"], delegate.ResumeBlockedWorkspaceExpired)
	}
}

// TestHandleAgentStatus_ResumableNativeWithoutAWorkspace is the other half of
// the split on the same fixture: an equally gone workspace reads resumable,
// because the transcript IS the resume state and the claim builds a fresh tree
// around it. The composer stays live and the send it accepts works.
func TestHandleAgentStatus_ResumableNativeWithoutAWorkspace(t *testing.T) {
	s := newTestServer(t)
	withEmptyBlobStore(t, s)
	conversationID := seedSteerConversation(t, s.db, "resgonenative", "open")
	execSQL(t, s.db, `UPDATE conversations SET runtime='native', worktree_path=? WHERE id=?`,
		filepath.Join(t.TempDir(), "swept-away"), conversationID)

	got := readConversation(t, s, conversationID)
	if got["resumable"] != true {
		t.Errorf("resumable = %v, want true", got["resumable"])
	}
	if _, ok := got["resume_blocked_reason"]; ok {
		t.Errorf("resume_blocked_reason = %v, want the key absent", got["resume_blocked_reason"])
	}
}

// TestHandleAgentStatus_ResumableParkedConversation: the unchanged case. A
// parked conversation with a live workspace under a running blueprint
// reads resumable, with no reason attached — the composer stays live and
// the follow-up works.
func TestHandleAgentStatus_ResumableParkedConversation(t *testing.T) {
	s := newTestServer(t)
	withEmptyBlobStore(t, s)
	conversationID := seedSteerConversation(t, s.db, "reswarm", "open")
	execSQL(t, s.db, `UPDATE conversations SET runtime='native', worktree_path=? WHERE id=?`, t.TempDir(), conversationID)

	got := readConversation(t, s, conversationID)
	if got["resumable"] != true {
		t.Errorf("resumable = %v, want true", got["resumable"])
	}
	if _, ok := got["resume_blocked_reason"]; ok {
		t.Errorf("resume_blocked_reason = %v, want the key absent when nothing is blocking", got["resume_blocked_reason"])
	}
}

// TestHandleAgentStatus_ResumableBlueprintCancelled: the workspace survives but
// nothing would drive it, which is a different sentence to put in front of a
// person than "expired" — so it is a different reason on the wire.
func TestHandleAgentStatus_ResumableBlueprintCancelled(t *testing.T) {
	s := newTestServer(t)
	withEmptyBlobStore(t, s)
	conversationID := seedSteerConversation(t, s.db, "rescancelled", "open")
	execSQL(t, s.db, `UPDATE conversations SET runtime='native', worktree_path=? WHERE id=?`, t.TempDir(), conversationID)
	execSQL(t, s.db, `UPDATE blueprint_runs SET status='cancelled'
		WHERE id = (SELECT blueprint_run_id FROM conversations WHERE id=?)`, conversationID)

	got := readConversation(t, s, conversationID)
	if got["resumable"] != false {
		t.Errorf("resumable = %v, want false", got["resumable"])
	}
	if got["resume_blocked_reason"] != delegate.ResumeBlockedBlueprintCancelled {
		t.Errorf("resume_blocked_reason = %v, want %q", got["resume_blocked_reason"], delegate.ResumeBlockedBlueprintCancelled)
	}
}

// TestHandleAgentStatus_ResumabilityOmittedForActiveAndFailed: the two shapes
// the read deliberately doesn't answer for. An active conversation is steered through
// its live process (and the client's own `active` arm already opens the
// composer); a failed one has no workspace by construction. Absence is the
// answer — the client falls back to the status reading, which is right for
// both — and it is what keeps a blob existence check off every live conversation's
// poll.
func TestHandleAgentStatus_ResumabilityOmittedForActiveAndFailed(t *testing.T) {
	for _, status := range []string{"running", "cloning", "failed"} {
		t.Run(status, func(t *testing.T) {
			s := newTestServer(t)
			withEmptyBlobStore(t, s)
			conversationID := seedSteerConversation(t, s.db, "resskip"+status, "running")
			if status != "running" {
				execSQL(t, s.db, `UPDATE conversations SET status=? WHERE id=?`, status, conversationID)
			}
			if status == "cloning" {
				// A claim phase is the coalesced display status, not a stored
				// one — seed it the way the dispatcher does.
				execSQL(t, s.db, `UPDATE conversations SET status='running' WHERE id=?`, conversationID)
				execSQL(t, s.db, `INSERT INTO claims (id, conversation_id, org_id, executor_id, boot_epoch, phase)
					VALUES (?, ?, 'local-org', 'exec-1', 1, 'cloning')`, "cl_"+conversationID, conversationID)
			}

			got := readConversation(t, s, conversationID)
			if got["Status"] != status {
				t.Fatalf("display status = %v, want %s — the fixture didn't produce the shape under test", got["Status"], status)
			}
			if _, ok := got["resumable"]; ok {
				t.Errorf("resumable = %v, want the key absent for a %s conversation", got["resumable"], status)
			}
		})
	}
}
