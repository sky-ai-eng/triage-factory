package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/pkg/websocket"
)

// barringLoginExt is a LoginExtension that bars the orgs it is told to and
// declines everything else. The set is mutable mid-test so a socket can
// handshake while its org is open and be swept once the org bars it — the
// order a requirement turned on after the handshake arrives in.
type barringLoginExt struct {
	noopLoginExtension

	mu     sync.Mutex
	barred map[string]bool
	// asked records each (org, provider) SSOGate was asked about, so a test
	// can confirm the sweep judged the credential by the provider it carries.
	asked []string
}

func (e *barringLoginExt) bar(orgID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.barred == nil {
		e.barred = map[string]bool{}
	}
	e.barred[orgID] = true
}

func (e *barringLoginExt) SSOGate(_ context.Context, orgID, _, signInProviderID string) (SSOGateResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.asked = append(e.asked, orgID+"|"+signInProviderID)
	if e.barred[orgID] {
		return SSOGateResult{Barred: true, StartURL: "/api/auth/oauth/saml?provider_id=p"}, nil
	}
	return SSOGateResult{}, nil
}

// TestSSOGate_SweepClosesBarredSessionSocket is the streaming half of Require
// SSO: a socket opened while its org admitted the session is closed with 4002
// by one revalidation pass once the org bars that session, and a socket on an
// org that still admits it stays open.
func TestSSOGate_SweepClosesBarredSessionSocket(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	ext := &barringLoginExt{}
	r.srv.loginExt = ext

	userID := r.seedUser()
	barredOrg, _ := r.seedOrg(userID, "sso-barred-org")
	openOrg, _ := r.seedOrg(userID, "sso-open-org")
	base := r.liveServer(t)

	resp, _ := r.driveCallback(userID)
	sidBarred := r.sidFromResp(resp)
	r.setActiveOrg(t, sidBarred, barredOrg)
	barredConn := dialServerWS(t, base, r.srv.sidCookieName(), sidBarred)
	waitWSRegistered(t, r.srv.ws, barredConn, barredOrg.String())

	resp, _ = r.driveCallback(userID)
	sidOpen := r.sidFromResp(resp)
	r.setActiveOrg(t, sidOpen, openOrg)
	openConn := dialServerWS(t, base, r.srv.sidCookieName(), sidOpen)
	waitWSRegistered(t, r.srv.ws, openConn, openOrg.String())

	ext.bar(barredOrg.String())
	r.srv.revalidateCredentialsOnce(context.Background())

	expectServerCloseCode(t, barredConn, websocket.CloseMembershipChanged)
	if !r.srv.ws.PresentFor(openOrg.String(), "still-open-probe") {
		t.Error("the socket on an org that still admits the session was closed too")
	}
}

// TestSSOGate_SweepClosesBarredTokenSocket is the same pass for a Bearer
// socket, judged by the provider the token carries.
func TestSSOGate_SweepClosesBarredTokenSocket(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	ext := &barringLoginExt{}
	r.srv.loginExt = ext

	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "sso-token-org")
	_, plaintext, err := r.srv.authDeps.apiTokens.MintSystem(
		context.Background(), userID.String(), orgID.String(), "streamer", nil, nil, "provider-x", userID.String())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	conn := dialServerWSBearer(t, r.liveServer(t), plaintext)
	waitWSRegistered(t, r.srv.ws, conn, orgID.String())

	ext.bar(orgID.String())
	r.srv.revalidateCredentialsOnce(context.Background())

	expectServerCloseCode(t, conn, websocket.CloseMembershipChanged)

	ext.mu.Lock()
	defer ext.mu.Unlock()
	if got := ext.asked[len(ext.asked)-1]; got != orgID.String()+"|provider-x" {
		t.Errorf("sweep asked SSOGate about %q, want the token's org and inherited provider", got)
	}
}

// TestSSOGate_BarredCursorAndPathOrg: a barred cursor org is withheld (RequireOrg
// 403 SSO_REQUIRED, /api/me flags it and omits it as active), a barred path org is
// 403 to a member, and a non-member naming it gets the route's own 404.
func TestSSOGate_BarredCursorAndPathOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	ext := &barringLoginExt{}
	r.srv.loginExt = ext

	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "sso-cursor-org")
	resp, _ := r.driveCallback(userID)
	sid := r.sidFromResp(resp)
	r.setActiveOrg(t, sid, orgID)
	ext.bar(orgID.String())

	assertSSORequired := func(t *testing.T, got *http.Response) {
		t.Helper()
		if got.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", got.StatusCode)
		}
		var body struct {
			Errors []httpx.ErrorItem `json:"errors"`
		}
		if err := json.NewDecoder(got.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Errors) != 1 || body.Errors[0].Reason != httpx.ReasonSSORequired {
			t.Errorf("errors = %+v, want one SSO_REQUIRED", body.Errors)
		}
	}

	t.Run("cursor-scoped route", func(t *testing.T) {
		assertSSORequired(t, r.requestWithSid("GET", "/api/dashboard/stats", sid))
	})
	t.Run("path-scoped route", func(t *testing.T) {
		assertSSORequired(t, r.requestWithSid("GET", "/api/orgs/"+orgID.String(), sid))
	})
	t.Run("websocket handshake", func(t *testing.T) {
		assertSSORequired(t, r.requestWithSid("GET", "/api/ws", sid))
	})
	t.Run("me flags the org and names no active org", func(t *testing.T) {
		got := r.requestWithSid("GET", "/api/me", sid)
		if got.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", got.StatusCode)
		}
		var me struct {
			ActiveOrgID string `json:"active_org_id"`
			Orgs        []struct {
				ID          string `json:"id"`
				SSORequired bool   `json:"sso_required"`
				SSOStartURL string `json:"sso_start_url"`
			} `json:"orgs"`
		}
		if err := json.NewDecoder(got.Body).Decode(&me); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if me.ActiveOrgID != "" {
			t.Errorf("active_org_id = %q, want none for a barred cursor", me.ActiveOrgID)
		}
		if len(me.Orgs) != 1 || !me.Orgs[0].SSORequired || me.Orgs[0].SSOStartURL == "" {
			t.Errorf("orgs = %+v, want the org flagged with a start url", me.Orgs)
		}
	})
	t.Run("non-member keeps the 404", func(t *testing.T) {
		stranger := r.seedUser()
		strangerOrg, _ := r.seedOrg(stranger, "sso-stranger-org")
		resp, _ := r.driveCallback(stranger)
		strangerSID := r.sidFromResp(resp)
		r.setActiveOrg(t, strangerSID, strangerOrg)
		if got := r.requestWithSid("GET", "/api/orgs/"+orgID.String(), strangerSID); got.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want 404 for a non-member", got.StatusCode)
		}
	})
	t.Run("login default skips the barred org", func(t *testing.T) {
		got, err := r.srv.lookupEarliestMembership(context.Background(), r.srv.db, userID, "")
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		if got.Valid {
			t.Errorf("default org = %s, want none when the only membership is barred", got.UUID)
		}
	})
}
