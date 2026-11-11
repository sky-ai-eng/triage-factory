package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ws "github.com/coder/websocket"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// TestHandleWS_MultiMode_NoIdentityRefused: a handshake that reaches handleWS
// with no user, as one would if a mount skipped withSession, is refused with
// 401 and registers nothing with the hub, which delivers every org's events to
// a client with no identity.
func TestHandleWS_MultiMode_NoIdentityRefused(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	s := &Server{ws: websocket.NewHub()}
	ts := httptest.NewServer(http.HandlerFunc(s.handleWS))
	t.Cleanup(ts.Close)

	assertWSRefused(t, s, ts.URL, http.StatusUnauthorized)
}

// TestWS_MultiMode_NoCredentialRefused: through the full middleware chain with
// auth wired, a handshake carrying no credential is refused with 401 and
// registers nothing with the hub.
func TestWS_MultiMode_NoCredentialRefused(t *testing.T) {
	rig := newAuthRig(t)
	assertWSRefused(t, rig.srv, rig.liveServer(t), http.StatusUnauthorized)
}

// assertWSRefused dials /api/ws on baseURL with no credential and asserts the
// handshake is answered with want and that s's hub holds no client afterwards.
func assertWSRefused(t *testing.T, s *Server, baseURL string, want int) {
	t.Helper()
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws"
	conn, resp, err := ws.Dial(context.Background(), wsURL, nil)
	if err == nil {
		_ = conn.Close(ws.StatusNormalClosure, "")
		t.Fatal("handshake succeeded, want it refused")
	}
	if resp == nil || resp.StatusCode != want {
		t.Fatalf("handshake response = %v, want status %d", resp, want)
	}
	if n := s.ws.ClientCount(); n != 0 {
		t.Errorf("hub has %d clients after a refused handshake, want 0", n)
	}
}
