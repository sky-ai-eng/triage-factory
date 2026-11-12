package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
)

// TestHandleMessages_InvisibleConversationIs404: the transcript read answers a
// conversation on a team the caller is not in, and one that does not exist,
// with the same 404 its sibling routes give, never an empty 200 page.
func TestHandleMessages_InvisibleConversationIs404(t *testing.T) {
	r := newAuthRig(t)
	alice := r.seedUser()
	orgID, _ := r.seedOrg(alice, "messages-org")
	// The conversation lands on the org's default team, which only alice is in.
	conversationID := seedConversationInOrg(t, r, orgID, alice, "messages")
	pgtest.MustExec(t, r.h.AdminDB, `
		INSERT INTO messages (org_id, conversation_id, role, content, subtype, delivered, window_state)
		VALUES ($1, $2, 'assistant', 'hello', '', true, 'active')
	`, orgID, conversationID)

	bob := r.seedUser()
	pgtest.MustExec(t, r.h.AdminDB,
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`, bob, orgID)
	r.seedTeam(orgID, "elsewhere", "member", bob)

	read := func(sid, id string) (int, []byte) {
		t.Helper()
		resp := r.requestWithSid(http.MethodGet, "/api/agent/conversations/"+id+"/messages", sid)
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, body
	}

	code, body := read(r.signIn(alice), conversationID)
	if code != http.StatusOK {
		t.Fatalf("alice GET messages = %d, want 200; body=%s", code, body)
	}
	var page struct {
		Items []struct {
			Content string `json:"content"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if len(page.Items) != 1 || page.Items[0].Content != "hello" {
		t.Errorf("alice's transcript = %+v, want the one seeded message", page.Items)
	}

	want := httptest.NewRecorder()
	notFound(want, "conversation")
	sidBob := r.signIn(bob)
	for desc, id := range map[string]string{
		"another team's conversation": conversationID,
		"nonexistent conversation":    uuid.NewString(),
	} {
		code, body := read(sidBob, id)
		if code != http.StatusNotFound || string(body) != want.Body.String() {
			t.Errorf("%s: GET messages = %d %s, want 404 %s", desc, code, body, want.Body.String())
		}
	}
}
