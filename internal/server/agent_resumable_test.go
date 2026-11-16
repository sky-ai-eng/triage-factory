package server

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/delegate"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
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
			if got["status"] != status {
				t.Fatalf("display status = %v, want %s — the fixture didn't produce the shape under test", got["status"], status)
			}
			if _, ok := got["resumable"]; ok {
				t.Errorf("resumable = %v, want the key absent for a %s conversation", got["resumable"], status)
			}
		})
	}
}

// TestHandleAgentStatus_NextMessageSettingsAndCacheWarmth pins what the
// composer opens with: the settings a message naming none would run on, and
// when the provider cache from the newest assistant turn goes cold.
func TestHandleAgentStatus_NextMessageSettingsAndCacheWarmth(t *testing.T) {
	seed := seedFollowUpConversation
	insert := func(t *testing.T, s *Server, msg domain.Message) {
		t.Helper()
		if _, err := sqlitestore.New(s.db).Conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &msg); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}

	t.Run("the read names the conversation's runtime", func(t *testing.T) {
		s, id := seed(t, "runtime")
		if got := readConversation(t, s, id); got["runtime"] != domain.ConversationRuntimeNative {
			t.Errorf("runtime = %v, want %s", got["runtime"], domain.ConversationRuntimeNative)
		}
	})
	t.Run("no person row runs on the team default", func(t *testing.T) {
		s, id := seed(t, "nextdefault")
		got := readConversation(t, s, id)
		if got["next_message_model"] != "claude-sonnet-4-6" {
			t.Errorf("next_message_model = %v, want the team default", got["next_message_model"])
		}
		if _, ok := got["next_message_effort"]; ok {
			t.Errorf("next_message_effort = %v, want absent for the provider's default", got["next_message_effort"])
		}
	})
	t.Run("the newest person row's settings", func(t *testing.T) {
		s, id := seed(t, "nextperson")
		insert(t, s, domain.Message{ConversationID: id, UserID: runmode.LocalDefaultUserID, Role: "user", Content: "a", Model: "opus", Effort: domain.EffortLow})
		insert(t, s, domain.Message{ConversationID: id, UserID: runmode.LocalDefaultUserID, Role: "user", Content: "b", Model: "sonnet", Effort: domain.EffortHigh})
		got := readConversation(t, s, id)
		if got["next_message_model"] != "sonnet" || got["next_message_effort"] != domain.EffortHigh {
			t.Errorf("next message = (%v, %v), want (sonnet, high)", got["next_message_model"], got["next_message_effort"])
		}
	})
	t.Run("a person row on a model the team does not enable opens with none", func(t *testing.T) {
		s, id := seed(t, "nextdisabled")
		spawner := delegate.NewSpawner(s.db, sqlitestore.New(s.db), nil, s.ws, "sonnet")
		spawner.SetRunCredentialResolvers(nil, nil, func(context.Context, string, string) (domain.TeamModels, error) {
			return domain.NewTeamModels("sonnet", domain.OrgModelSet([]string{"sonnet"}, []string{"sonnet", "opus"})), nil
		})
		s.SetSpawner(spawner)
		insert(t, s, domain.Message{ConversationID: id, UserID: runmode.LocalDefaultUserID, Role: "user", Content: "a", Model: "opus", Effort: domain.EffortHigh})
		got := readConversation(t, s, id)
		for _, field := range []string{"next_message_model", "next_message_effort"} {
			if v, ok := got[field]; ok {
				t.Errorf("%s = %v, want absent: a message naming no settings would be refused", field, v)
			}
		}
	})
	t.Run("cache_warm_until is the newest assistant turn plus the TTL", func(t *testing.T) {
		s, id := seed(t, "warmuntil")
		if got := readConversation(t, s, id); got["cache_warm_until"] != nil {
			t.Errorf("cache_warm_until = %v with no assistant row, want absent", got["cache_warm_until"])
		}
		at := time.Date(2026, 11, 15, 9, 30, 0, 0, time.UTC)
		insert(t, s, domain.Message{ConversationID: id, Role: "assistant", Content: "older", Model: "sonnet", CreatedAt: at.Add(-time.Hour)})
		insert(t, s, domain.Message{ConversationID: id, Role: "assistant", Content: "newest", Model: "sonnet", CreatedAt: at})
		want := at.Add(domain.ProviderCacheTTL).Format(time.RFC3339)
		if got := readConversation(t, s, id); got["cache_warm_until"] != want {
			t.Errorf("cache_warm_until = %v, want %s", got["cache_warm_until"], want)
		}
	})
}
