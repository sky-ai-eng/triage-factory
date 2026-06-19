package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	ws "github.com/coder/websocket"

	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// liveServer stands up an httptest server around the rig's mux so a real
// websocket can be dialed against the full middleware chain (withSession
// resolves the sid cookie into claims + active org). The recorder-based
// requestWithSid path and this live server share the same *Server — and
// thus the same hub and session store — so a revoke issued through one
// actively closes a socket opened through the other.
func (r *authRig) liveServer(t *testing.T) string {
	t.Helper()
	ts := httptest.NewServer(r.srv.mux)
	t.Cleanup(ts.Close)
	return ts.URL
}

// setActiveOrg pins a session's active org so the WS handshake resolves a
// concrete org to scope (and re-validate membership for). driveCallback
// creates the session; this fixes its active_org_id deterministically.
func (r *authRig) setActiveOrg(t *testing.T, sid string, orgID uuid.UUID) {
	t.Helper()
	if err := r.srv.authDeps.sessions.UpdateActiveOrgSystem(
		context.Background(), uuid.MustParse(sid), orgID); err != nil {
		t.Fatalf("set active org: %v", err)
	}
}

// dialServerWS opens a websocket against the live server with the sid
// cookie set, so the handshake authenticates as that session.
func dialServerWS(t *testing.T, baseURL, cookieName, sid string) *ws.Conn {
	t.Helper()
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws"
	hdr := http.Header{}
	hdr.Set("Cookie", cookieName+"="+sid)
	conn, _, err := ws.Dial(context.Background(), wsURL, &ws.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ws.StatusNormalClosure, "") })
	return conn
}

// waitWSRegistered blocks until the dialed conn is registered in the
// hub's per-user index, so a revoke issued right after can actually find
// it. It absorbs the (microsecond, but nonzero) race between ws.Dial
// returning and HandleWS registering the client.
//
// Registration is detected WITHOUT reading the conn: the client sends one
// presence frame and we poll PresentFor, which reports hub state set by
// readPump. readPump only runs after the client is registered (HandleWS
// registers under the mutex, then starts the pumps), so a true result
// guarantees byUser is populated. We deliberately don't read the conn to
// probe readiness — coder/websocket closes a connection when a Read's
// context is cancelled, so a short-timeout read in a retry loop would
// tear the socket down on the first slow delivery; and any frame read
// here would also sit ahead of the close frame the test then asserts on.
func waitWSRegistered(t *testing.T, hub *websocket.Hub, conn *ws.Conn, orgID string) {
	t.Helper()
	// One-shot presence write (its own context, cancelled once the write
	// returns — same shape as presence_test.go's writer).
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := conn.Write(ctx, ws.MessageText,
			[]byte(`{"type":"presence","viewing":"board","visible":true}`)); err != nil {
			t.Fatalf("write presence frame: %v", err)
		}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// viewing="board" satisfies PresentFor for any runID in the org.
		if hub.PresentFor(orgID, "registration-probe") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("ws never registered with the hub (PresentFor stayed false)")
}

// expectServerCloseCode asserts the conn is closed with want within 1s —
// the revocation-latency bound from the ticket.
func expectServerCloseCode(t *testing.T, conn *ws.Conn, want ws.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatalf("expected close code %d, got a message instead", want)
	} else if got := ws.CloseStatus(err); got != want {
		t.Fatalf("close code = %d, want %d (err=%v)", got, want, err)
	}
}

// TestWS_LogoutClosesSocket is the revoke acceptance case: a live WS is
// open, /api/auth/logout revokes the session, and the socket must close
// with code 4001 (session revoked) within 1s — instead of streaming the
// org's events until it happens to drop.
func TestWS_LogoutClosesSocket(t *testing.T) {
	r := newAuthRig(t)

	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "revoke-org")
	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)
	r.setActiveOrg(t, sid, orgID)

	base := r.liveServer(t)
	conn := dialServerWS(t, base, r.srv.sidCookieName(), sid)
	waitWSRegistered(t, r.srv.ws, conn, orgID.String())

	// Revoke the session via the normal logout endpoint (same sid cookie).
	if got := r.requestWithSid("POST", "/api/auth/logout", sid).StatusCode; got != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", got)
	}

	expectServerCloseCode(t, conn, websocket.CloseSessionRevoked)
}

// TestWS_MembershipRemovalClosesSocket is the membership acceptance case:
// a member's WS is scoped to org B; an admin (here, a self-leave) deletes
// their membership, and the socket must close with code 4002 (membership
// changed) within 1s. 4002, not 4001 — the user keeps their session
// (they may still belong to other orgs) and re-handshakes rather than
// being logged out.
func TestWS_MembershipRemovalClosesSocket(t *testing.T) {
	r := newAuthRig(t)

	// Org owned by someone else so removing our member doesn't trip the
	// last-owner guard. Our user joins as a plain member.
	ownerID := r.seedUser()
	orgID, _ := r.seedOrg(ownerID, "shared-org")

	userID := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO public.org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		userID, orgID,
	); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)
	r.setActiveOrg(t, sid, orgID)

	base := r.liveServer(t)
	conn := dialServerWS(t, base, r.srv.sidCookieName(), sid)
	waitWSRegistered(t, r.srv.ws, conn, orgID.String())

	// Self-leave: DELETE the membership through the org-members endpoint.
	path := "/api/orgs/" + orgID.String() + "/members/" + userID.String()
	if got := r.requestWithSid("DELETE", path, sid).StatusCode; got != http.StatusNoContent {
		t.Fatalf("member remove status = %d, want 204", got)
	}

	expectServerCloseCode(t, conn, websocket.CloseMembershipChanged)
}
