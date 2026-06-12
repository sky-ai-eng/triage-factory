package server

import (
	"encoding/json"
	"net/http"
	"strings"
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

	projectID, err := srv.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "Curator HTTP test"})
	if err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return srv, c, projectID
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
	// 202 + a non-empty request_id + a persisted row is the HTTP
	// contract the Projects page (SKY-217) will rely on. The
	// goroutine's dispatch behavior (running → terminal flips) is
	// covered by the curator package's own tests; asserting on it
	// here would be a flake — on hosts with `claude` on PATH it
	// could spawn and stream before this test reads the row, on
	// hosts without it the row could fail-fast to the failed
	// status. Either way, the row's *terminal-ness* is not the
	// HTTP handler's contract.
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

	// Tear down synchronously before the row is read so the
	// project's goroutine doesn't keep racing the assertion.
	// CancelProject blocks on the goroutine's exit (curator's
	// shutdown contract) so by the time it returns no further
	// status writes can land on the row.
	c.CancelProject(runmode.LocalDefaultOrgID, projectID)

	got, err := db.GetCuratorRequest(srv.db, resp.RequestID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("request row not persisted")
	}
}

func TestHandleCuratorHistory_ReturnsRequestsWithMessages(t *testing.T) {
	// Seed directly via the db layer so we don't depend on claude
	// being on PATH. The HTTP shape is what matters here.
	srv, _, projectID := curatorTestSetup(t)

	id1, _ := db.CreateCuratorRequest(srv.db, projectID, "first")
	_, _ = db.CompleteCuratorRequest(srv.db, id1, "done", "", 0.01, 100, 1)
	if _, err := db.InsertCuratorMessage(srv.db, &domain.CuratorMessage{
		RequestID: id1,
		Role:      "assistant",
		Subtype:   "text",
		Content:   "first reply",
	}); err != nil {
		t.Fatalf("seed msg: %v", err)
	}

	id2, _ := db.CreateCuratorRequest(srv.db, projectID, "second")
	_, _ = db.CompleteCuratorRequest(srv.db, id2, "done", "", 0.02, 200, 2)

	rr := doJSON(t, srv, http.MethodGet, "/api/projects/"+projectID+"/curator/messages", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	var got []curatorRequestJSON
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d requests, want 2", len(got))
	}
	if got[0].UserInput != "first" || got[1].UserInput != "second" {
		t.Errorf("ordering wrong: %+v / %+v", got[0].UserInput, got[1].UserInput)
	}
	if len(got[0].Messages) != 1 || got[0].Messages[0].Content != "first reply" {
		t.Errorf("first request messages: %+v", got[0].Messages)
	}
	if len(got[1].Messages) != 0 {
		t.Errorf("second request should have no messages, got %d", len(got[1].Messages))
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

func TestHandleCuratorCancel_FlipsQueuedRow(t *testing.T) {
	// Seed a queued row directly so we don't race with goroutine
	// pickup. Cancel hits the DB-level flip path; the runtime's
	// in-flight cancel is a no-op when no goroutine has picked the
	// row up yet.
	srv, _, projectID := curatorTestSetup(t)
	requestID, _ := db.CreateCuratorRequest(srv.db, projectID, "queued forever")

	rr := doJSON(t, srv, http.MethodDelete, "/api/projects/"+projectID+"/curator/messages/in-flight", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}

	got, _ := db.GetCuratorRequest(srv.db, requestID)
	if got.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", got.Status)
	}
	if !strings.Contains(got.ErrorMsg, "user cancelled") {
		t.Errorf("error_msg = %q, want to contain 'user cancelled'", got.ErrorMsg)
	}
}

func TestHandleCuratorSend_503WhenRuntimeUnset(t *testing.T) {
	// SetCurator never called → handler returns 503 rather than nil-
	// dereferencing. Real binaries always wire it via main.go, but
	// we keep the guard so a future test or a partial init can't
	// crash the server.
	srv := newTestServer(t)
	projectID, _ := srv.projects.Create(t.Context(), runmode.LocalDefaultOrg, runmode.LocalDefaultTeamID, domain.Project{Name: "no-curator"})

	rr := doJSON(t, srv, http.MethodPost, "/api/projects/"+projectID+"/curator/messages", map[string]string{"content": "hi"})
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
}
