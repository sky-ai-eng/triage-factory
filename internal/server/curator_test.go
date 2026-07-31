package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/curator"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// curatorTestSetup wires a real Curator into the test server but
// uses a hub the handler tests don't read from. The runtime's
// per-project goroutine WILL try to spawn `claude` if a real message
// flows through; for HTTP-layer tests we cancel each request
// immediately afterward (or use cases that don't reach dispatch) so
// the subprocess invocation never matters.
func curatorTestSetup(t *testing.T) (*Server, *curator.Curator, string) {
	t.Helper()
	srv := newTestServer(t)
	hub := websocket.NewHub()
	c := curator.New(sqlitestore.New(srv.db), hub, "")
	srv.SetCurator(c)
	t.Cleanup(c.Shutdown)

	projectID, err := srv.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "Curator HTTP test"})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return srv, c, projectID
}

// seedCuratorTurn records a queued turn through the store exactly the way
// SendMessage does (conversation find-or-mint + undelivered user message),
// without starting a session goroutine.
func seedCuratorTurn(t *testing.T, srv *Server, projectID, input string) (convID string, msgID int64) {
	t.Helper()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	if err := srv.tx.SyntheticClaimsWithTx(t.Context(), org, user, func(ts db.TxStores) error {
		conv, err := ts.Curator.GetOrCreateConversation(t.Context(), org, projectID, user)
		if err != nil {
			return err
		}
		convID = conv.ID
		msgID, err = ts.Curator.EnqueueUserMessage(t.Context(), org, conv.ID, user, input)
		return err
	}); err != nil {
		t.Fatalf("seed turn %q: %v", input, err)
	}
	return convID, msgID
}

// completeCuratorTurn drives a seeded turn to a released engagement: claim,
// deliver, optionally stream one assistant row, release completed.
func completeCuratorTurn(t *testing.T, srv *Server, projectID, convID string, msgID int64, reply string, cost float64) {
	t.Helper()
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	claimID, ok, err := srv.curatorStore.ClaimTurnSystem(t.Context(), org, convID, msgID, "test-exec", 1)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := srv.tx.SyntheticClaimsWithTx(t.Context(), org, user, func(ts db.TxStores) error {
		if _, err := ts.Curator.BeginTurn(t.Context(), org, projectID, convID, msgID); err != nil {
			return err
		}
		if reply == "" {
			return nil
		}
		_, err := ts.Conversations.InsertMessage(t.Context(), org, &domain.Message{
			ConversationID: convID, UserID: user, ClaimID: claimID,
			Role: "assistant", Content: reply,
		})
		return err
	}); err != nil {
		t.Fatalf("run turn: %v", err)
	}
	if _, err := srv.curatorStore.ReleaseActiveTurnSystem(t.Context(), org, convID, "completed", "", cost, 100, 1); err != nil {
		t.Fatalf("release: %v", err)
	}
}

func TestHandleCuratorSend_RequiresContent(t *testing.T) {
	srv, _, projectID := curatorTestSetup(t)

	cases := []struct {
		name    string
		payload map[string]string
	}{
		{"empty body", nil},
		{"empty content", map[string]string{"content": ""}},
		{"whitespace only", map[string]string{"content": "   \n\t"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := doJSON(t, srv, http.MethodPost, "/api/projects/"+projectID+"/curator/messages", tc.payload)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHandleCuratorSend_404OnMissingProject(t *testing.T) {
	srv, _, _ := curatorTestSetup(t)
	rr := doJSON(t, srv, http.MethodPost, "/api/projects/nope/curator/messages", map[string]string{"content": "hi"})
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleCuratorSend_AcceptedReturnsRequestID(t *testing.T) {
	// 202 + a non-empty request_id is the HTTP contract the Projects page
	// relies on; the id is the turn's user-message id rendered decimal.
	// The goroutine's dispatch behavior (running → terminal flips) is
	// covered by the curator package's own tests; asserting on it here
	// would be a flake.
	srv, c, projectID := curatorTestSetup(t)

	rr := doJSON(t, srv, http.MethodPost, "/api/projects/"+projectID+"/curator/messages", map[string]string{"content": "hello"})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body=%s", rr.Code, rr.Body.String())
	}
	var resp curatorSendResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.RequestID == "" {
		t.Error("empty request_id in response")
	}
	if _, err := strconv.ParseInt(resp.RequestID, 10, 64); err != nil {
		t.Errorf("request_id %q is not a decimal message id: %v", resp.RequestID, err)
	}

	// Tear down synchronously so the project's goroutine doesn't keep
	// racing later assertions in other tests sharing the binary.
	c.CancelProject(runmode.LocalDefaultOrgID, projectID, "project deleted")
}

func TestHandleCuratorHistory_ReturnsRequestsWithMessages(t *testing.T) {
	// Seed directly via the store layer so we don't depend on claude
	// being on PATH. The HTTP shape is what matters here.
	srv, _, projectID := curatorTestSetup(t)

	conv, msg1 := seedCuratorTurn(t, srv, projectID, "first")
	completeCuratorTurn(t, srv, projectID, conv, msg1, "first reply", 0.01)
	_, msg2 := seedCuratorTurn(t, srv, projectID, "second")
	completeCuratorTurn(t, srv, projectID, conv, msg2, "", 0.02)

	rr := doJSON(t, srv, http.MethodGet, "/api/projects/"+projectID+"/curator/messages", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var got []curatorRequestJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d requests, want 2 (body=%s)", len(got), rr.Body.String())
	}
	if got[0].UserInput != "first" || got[1].UserInput != "second" {
		t.Errorf("ordering wrong: %+v / %+v", got[0].UserInput, got[1].UserInput)
	}
	if got[0].ID != strconv.FormatInt(msg1, 10) || got[1].ID != strconv.FormatInt(msg2, 10) {
		t.Errorf("turn ids = (%s, %s), want the message ids (%d, %d)", got[0].ID, got[1].ID, msg1, msg2)
	}
	if got[0].Status != "done" || got[1].Status != "done" {
		t.Errorf("statuses = (%s, %s), want (done, done)", got[0].Status, got[1].Status)
	}
	if got[0].CostUSD != 0.01 {
		t.Errorf("first turn cost = %v, want 0.01 (from the executing claim)", got[0].CostUSD)
	}
	if len(got[0].Messages) != 1 || got[0].Messages[0].Content != "first reply" {
		t.Errorf("first request messages: %+v", got[0].Messages)
	}
	if got[0].Messages[0].ConversationID == "" {
		t.Errorf("message conversation_id is empty, want the owning curator conversation id")
	}
	if len(got[1].Messages) != 0 {
		t.Errorf("second request should have no messages, got %d", len(got[1].Messages))
	}
}

// TestHandleCuratorHistory_DeadLetteredTurnRendersFailed pins the synthesis
// fallback for a turn with no agent-side stream: a dead-lettered turn's user
// message carries the final claim itself, and history must derive the
// terminal 'failed' (and its error) from that claim rather than fabricate a
// 'done'.
func TestHandleCuratorHistory_DeadLetteredTurnRendersFailed(t *testing.T) {
	srv, _, projectID := curatorTestSetup(t)
	convID, msgID := seedCuratorTurn(t, srv, projectID, "poisoned")

	const giveUp = "curator turn failed 3 times, giving up: begin turn: boom"
	matched, err := srv.curatorStore.DeadLetterTurnSystem(t.Context(), runmode.LocalDefaultOrgID, convID, msgID, "test-exec", 1, giveUp)
	if err != nil || !matched {
		t.Fatalf("dead-letter: matched=%v err=%v", matched, err)
	}

	rr := doJSON(t, srv, http.MethodGet, "/api/projects/"+projectID+"/curator/messages", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var got []curatorRequestJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d turns, want the dead-lettered one (body=%s)", len(got), rr.Body.String())
	}
	if got[0].Status != "failed" {
		t.Errorf("status = %q, want failed (derived from the claim stamped on the user row)", got[0].Status)
	}
	if got[0].ErrorMsg != giveUp {
		t.Errorf("error = %q, want the give-up message", got[0].ErrorMsg)
	}
	if len(got[0].Messages) != 0 {
		t.Errorf("dead-lettered turn has %d stream messages, want 0", len(got[0].Messages))
	}
}

func TestHandleCuratorCancel_404OnNoInFlight(t *testing.T) {
	srv, _, projectID := curatorTestSetup(t)
	rr := doJSON(t, srv, http.MethodDelete, "/api/projects/"+projectID+"/curator/messages/in-flight", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleCuratorCancel_404OnMissingProject(t *testing.T) {
	srv, _, _ := curatorTestSetup(t)
	rr := doJSON(t, srv, http.MethodDelete, "/api/projects/nope/curator/messages/in-flight", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleCuratorCancel_DeletesQueuedTurn(t *testing.T) {
	// Seed a queued turn directly so we don't race with goroutine pickup.
	// Cancelling a turn that never entered context deletes its undelivered
	// user message — it simply never happened.
	srv, _, projectID := curatorTestSetup(t)
	convID, msgID := seedCuratorTurn(t, srv, projectID, "queued forever")

	rr := doJSON(t, srv, http.MethodDelete, "/api/projects/"+projectID+"/curator/messages/in-flight", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	var msgs []domain.Message
	if err := srv.tx.SyntheticClaimsWithTx(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, func(ts db.TxStores) error {
		ms, err := ts.Curator.ListConversationMessages(t.Context(), runmode.LocalDefaultOrgID, convID)
		msgs = ms
		return err
	}); err != nil {
		t.Fatalf("list messages: %v", err)
	}
	for _, m := range msgs {
		if int64(m.ID) == msgID {
			t.Errorf("queued turn %d survived cancel; want the undelivered row deleted", msgID)
		}
	}
}

func TestHandleCuratorReset_ArchivesConversation(t *testing.T) {
	srv, _, projectID := curatorTestSetup(t)
	convID, msgID := seedCuratorTurn(t, srv, projectID, "history")
	completeCuratorTurn(t, srv, projectID, convID, msgID, "reply", 0.01)

	rr := doJSON(t, srv, http.MethodPost, "/api/projects/"+projectID+"/curator/reset", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	// The archived conversation drops out of history — the next GET is a
	// clean slate.
	rr = doJSON(t, srv, http.MethodGet, "/api/projects/"+projectID+"/curator/messages", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("history status = %d, want 200", rr.Code)
	}
	var got []curatorRequestJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("history after reset = %d turns, want 0 (body=%s)", len(got), rr.Body.String())
	}
}

func TestHandleCuratorReset_409OnRunningTurn(t *testing.T) {
	srv, _, projectID := curatorTestSetup(t)
	convID, msgID := seedCuratorTurn(t, srv, projectID, "in flight")
	org, user := runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID
	if _, ok, err := srv.curatorStore.ClaimTurnSystem(t.Context(), org, convID, msgID, "test-exec", 1); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := srv.tx.SyntheticClaimsWithTx(t.Context(), org, user, func(ts db.TxStores) error {
		_, err := ts.Curator.BeginTurn(t.Context(), org, projectID, convID, msgID)
		return err
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}

	rr := doJSON(t, srv, http.MethodPost, "/api/projects/"+projectID+"/curator/reset", nil)
	if rr.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 while a turn is running; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandleCuratorSend_503WhenRuntimeUnset(t *testing.T) {
	// SetCurator never called → handler returns 503 rather than nil-
	// dereferencing. Real binaries always wire it via main.go, but
	// we keep the guard so a future test or a partial init can't
	// crash the server.
	srv := newTestServer(t)
	projectID, _ := srv.projects.Create(t.Context(), runmode.LocalDefaultOrgID, runmode.LocalDefaultTeamID, domain.Project{Name: "no-curator"})

	rr := doJSON(t, srv, http.MethodPost, "/api/projects/"+projectID+"/curator/messages", map[string]string{"content": "hi"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}
