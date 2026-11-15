package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/llm/modelcatalog"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx/httpxtest"
)

// seedFollowUpConversation is a parked native conversation whose blueprint has
// finished, so a message to it is a follow-up whose model the send decides.
func seedFollowUpConversation(t *testing.T, suffix string) (*Server, string) {
	t.Helper()
	s := newTestServer(t)
	withEmptyBlobStore(t, s)
	conversationID := seedSteerConversation(t, s.db, suffix, "open")
	execSQL(t, s.db, `UPDATE conversations SET runtime='native', worktree_path=? WHERE id=?`, t.TempDir(), conversationID)
	execSQL(t, s.db, `UPDATE blueprint_runs SET status='completed' WHERE id=(SELECT blueprint_run_id FROM conversations WHERE id=?)`, conversationID)
	return s, conversationID
}

// A message that names no model runs on the newest person message's. When the
// organization has since disconnected that model's provider, the send is
// refused with 409 NOT_CONFIGURED on the model field, the status the settings
// saves give the fault, and writes nothing. The detail read stops offering
// the model, so the composer asks for one.
func TestHandleMessage_InheritedModelOnAnUnconnectedProviderIsNotConfigured(t *testing.T) {
	s, conversationID := seedFollowUpConversation(t, "lostprovider")
	bedrockModel := modelKeyOn(t, modelcatalog.ProviderBedrock)
	if _, err := sqlitestore.New(s.db).Conversations.InsertMessage(context.Background(), runmode.LocalDefaultOrgID, &domain.Message{
		ConversationID: conversationID, UserID: runmode.LocalDefaultUserID, Role: "user", Content: "earlier", Model: bedrockModel,
	}); err != nil {
		t.Fatalf("insert the earlier person message: %v", err)
	}
	connectOrgProviders(t, s, true, false)
	before := messageRowCount(t, s.db, conversationID)

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/message", map[string]string{"text": "carry on"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonNotConfigured, "model")
	if !strings.Contains(rec.Body.String(), modelcatalog.ProviderDisplayName(modelcatalog.ProviderBedrock)) {
		t.Errorf("refusal does not name the provider to connect: %s", rec.Body.String())
	}
	if n := messageRowCount(t, s.db, conversationID); n != before {
		t.Errorf("messages = %d, want %d — a refused send writes no row", n, before)
	}

	got := readConversation(t, s, conversationID)
	for _, field := range []string{"next_message_model", "next_message_effort"} {
		if v, ok := got[field]; ok {
			t.Errorf("%s = %v, want absent: a message naming no settings would be refused", field, v)
		}
	}
}

// An organization that brings its own credentials and has connected none can
// run nothing, whatever model a message names. The send is refused with the
// same 409 NOT_CONFIGURED and no field, because no choice of model fixes it.
func TestHandleMessage_NoCredentialsIsNotConfiguredWithNoField(t *testing.T) {
	s, conversationID := seedFollowUpConversation(t, "nocreds")
	set, err := s.allStores.Orgs.GetSettingsSystem(context.Background(), runmode.LocalDefaultOrgID)
	if err != nil {
		t.Fatalf("read org settings: %v", err)
	}
	set.LLMAuthMethod = domain.LLMAuthBYOK
	if _, err := s.allStores.Orgs.UpdateSettingsVersioned(context.Background(), runmode.LocalDefaultOrgID, set, set.Version); err != nil {
		t.Fatalf("record that the org brings its own credentials: %v", err)
	}

	rec := doJSON(t, s, "POST", "/api/agent/conversations/"+conversationID+"/message", map[string]string{"text": "carry on"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	httpxtest.AssertFirstError(t, rec, httpx.ReasonNotConfigured, "")
	if n := messageRowCount(t, s.db, conversationID); n != 0 {
		t.Errorf("messages = %d, want 0 — a refused send writes no row", n)
	}
}
