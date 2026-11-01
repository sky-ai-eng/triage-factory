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
	blobs, err := storage.NewWorkspace()
	if err != nil {
		t.Fatalf("storage.NewWorkspace: %v", err)
	}
	spawner.SetStorage(blobs)
	s.SetSpawner(spawner)
	return spawner
}

// TestHandleAgentStatus_Resumable pins resumable and resume_blocked_reason on
// a parked conversation's read: facts about its workspace and blueprint that its
// status cannot show the client.
func TestHandleAgentStatus_Resumable(t *testing.T) {
	native := func(t *testing.T, s *Server, id, worktree string) {
		execSQL(t, s.db, `UPDATE conversations SET runtime='native', worktree_path=? WHERE id=?`, worktree, id)
	}
	gone := func(t *testing.T) string { return filepath.Join(t.TempDir(), "swept-away") }
	cases := []struct {
		name, suffix string
		setup        func(t *testing.T, s *Server, id string)
		reason       string // "" means resumable, with no reason on the wire
	}{
		// The SDK session transcript lived inside the snapshot, so a gone
		// worktree with an empty blob store leaves nothing to resume.
		{"sdk without a workspace", "resgone", func(t *testing.T, s *Server, id string) {
			execSQL(t, s.db, `UPDATE conversations SET sdk_session_id='sess-resgone', model='m', worktree_path=? WHERE id=?`, gone(t), id)
		}, delegate.ResumeBlockedWorkspaceExpired},
		// A native transcript is the resume state; the claim builds a fresh tree.
		{"native without a workspace", "resgonenative", func(t *testing.T, s *Server, id string) {
			native(t, s, id, gone(t))
		}, ""},
		{"native with a workspace", "reswarm", func(t *testing.T, s *Server, id string) {
			native(t, s, id, t.TempDir())
		}, ""},
		// The workspace survives but nothing would drive it.
		{"blueprint cancelled", "rescancelled", func(t *testing.T, s *Server, id string) {
			native(t, s, id, t.TempDir())
			execSQL(t, s.db, `UPDATE blueprint_runs SET status='cancelled'
				WHERE id = (SELECT blueprint_run_id FROM conversations WHERE id=?)`, id)
		}, delegate.ResumeBlockedBlueprintCancelled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTestServer(t)
			withEmptyBlobStore(t, s)
			conversationID := seedSteerConversation(t, s.db, c.suffix, "open")
			c.setup(t, s, conversationID)

			got := readConversation(t, s, conversationID)
			reason, present := got["resume_blocked_reason"]
			if c.reason == "" && (got["resumable"] != true || present) {
				t.Errorf("(resumable, resume_blocked_reason) = (%v, %v), want (true, absent)", got["resumable"], reason)
			}
			if c.reason != "" && (got["resumable"] != false || reason != c.reason) {
				t.Errorf("(resumable, resume_blocked_reason) = (%v, %v), want (false, %q)", got["resumable"], reason, c.reason)
			}
		})
	}
}

// TestHandleAgentStatus_ResumabilityOmittedForActiveAndFailed: an active
// conversation is steered through its live process and a failed one has no
// workspace, so the read carries no resumable key for either.
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
				execSQL(t, s.db, `INSERT INTO claims (id, conversation_id, org_id, executor_id, boot_epoch, phase, lease_expires_at)
					VALUES (?, ?, 'local-org', 'exec-1', 1, 'cloning', strftime('%Y-%m-%d %H:%M:%f','now','+300.000 seconds'))`, "cl_"+conversationID, conversationID)
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
