package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// The epic's acceptance, encoded: one session mints one token, and from there
// a headless caller drives the cursor-scoped surface, the two wizard writes,
// and its own rotation with no cookie in sight. The rows below are the ones the
// live-handler harness can reach; the live-stack drive (websocket streaming
// against a real deployment, a real GitHub PAT bind) is executed by hand.

// ---------- the Bearer-only drive ----------

// TestBearerAuth_CursorScopedSurfaceNeedsNoActiveOrg is the reason the token
// exists. Under a session, POST /api/teams/list resolves its org from the
// active-org cursor and answers 409 without one — and moving that cursor is a
// session verb a token is refused. A token carries its org instead, so the
// same routes answer with no cursor and no POST /api/me/active-org ever made.
func TestBearerAuth_CursorScopedSurfaceNeedsNoActiveOrg(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, teamID := r.seedOrg(userID, "cursorless-token-org")
	sid := r.signIn(userID)
	tok := r.createToken(sid, map[string]any{"name": "wizard", "org_id": orgID.String()})

	// The only session state that exists points nowhere, so nothing below can
	// be answered off a cursor by accident.
	if _, err := r.h.AdminDB.Exec(`UPDATE sessions SET active_org_id = NULL`); err != nil {
		t.Fatalf("clear active org: %v", err)
	}
	if got := r.requestWithSid("POST", "/api/teams/list", sid); got.StatusCode != http.StatusConflict {
		t.Fatalf("cursorless session teams/list = %d, want 409 (the gap the token closes)", got.StatusCode)
	}

	var me struct {
		ActiveOrgID string `json:"active_org_id"`
	}
	rec := r.tokensJSON("GET", "/api/me", nil, "", tok.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode /api/me: %v", err)
	}
	if me.ActiveOrgID != orgID.String() {
		t.Errorf("/api/me active_org_id = %q under a token, want the token's org %q", me.ActiveOrgID, orgID)
	}

	for _, tc := range []struct {
		name, method, path string
		body               any
	}{
		{"tasks list", "POST", "/api/tasks/list", map[string]any{"page_size": 5}},
		{"teams list", "POST", "/api/teams/list", map[string]any{}},
		{"team settings", "GET", "/api/teams/" + teamID.String() + "/settings", nil},
		{"team tracked set", "GET", "/api/teams/" + teamID.String() + "/github-repos", nil},
		{"org token policy", "GET", "/api/orgs/" + orgID.String() + "/api-token-policy", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.tokensJSON(tc.method, tc.path, tc.body, "", tok.Token)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s under a token = %d, want 200: %s", tc.method, tc.path, rec.Code, rec.Body.String())
			}
		})
	}

	// teams/list answers with the token's org's teams specifically, not an
	// empty 200 — the org came from the credential.
	var teams struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	rec = r.tokensJSON("POST", "/api/teams/list", map[string]any{}, "", tok.Token)
	if err := json.Unmarshal(rec.Body.Bytes(), &teams); err != nil {
		t.Fatalf("decode teams/list: %v (body=%s)", err, rec.Body.String())
	}
	if len(teams.Items) != 1 || teams.Items[0].ID != teamID.String() {
		t.Errorf("teams/list under a token = %+v, want exactly the org's team %s", teams.Items, teamID)
	}
}

// TestBearerAuth_WizardWritesLandUnderToken covers the two writes the headless
// wizard drive needed and could not make: the team's tracked set, and the
// per-user GitHub identity bind. The tracked-set write is proven by read-back.
// The identity bind is proven against a stand-in GitHub that accepts the PAT,
// so what lands is the identity row the real flow would write.
func TestBearerAuth_WizardWritesLandUnderToken(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, teamID := r.seedOrg(userID, "wizard-writes-org")
	sid := r.signIn(userID)
	tok := r.createToken(sid, map[string]any{"name": "wizard", "org_id": orgID.String()})

	path := "/api/teams/" + teamID.String() + "/github-repos"
	rec := r.tokensJSON("PUT", path, map[string]any{"repos": []string{"acme/widgets", "acme/gadgets"}}, "", tok.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT tracked set under a token = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var tracked struct {
		Repos []string `json:"repos"`
	}
	rec = r.tokensJSON("GET", path, nil, "", tok.Token)
	if err := json.Unmarshal(rec.Body.Bytes(), &tracked); err != nil {
		t.Fatalf("decode tracked set: %v (body=%s)", err, rec.Body.String())
	}
	if strings.Join(tracked.Repos, ",") != "acme/gadgets,acme/widgets" {
		t.Errorf("tracked set after the token's write = %v, want both repos", tracked.Repos)
	}

	// A GitHub that answers the two calls the bind makes — whoami and the
	// email list — for exactly one PAT, so the handler's own validation is
	// exercised without a network and the outcome is a persisted identity.
	const pat = "ghp_wizard_probe"
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+pat {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/api/v3/user":
			_, _ = w.Write([]byte(`{"login":"wizard-user","id":4242}`))
		case "/api/v3/user/emails":
			_, _ = w.Write([]byte(`[{"email":"wizard@test","primary":true,"verified":true}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(github.Close)
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO org_event_sources (org_id, kind, base_url, poll_interval)
		VALUES ($1, 'github', $2, make_interval(secs => 60))
		ON CONFLICT (org_id, kind) DO UPDATE SET base_url = EXCLUDED.base_url
	`, orgID, github.URL); err != nil {
		t.Fatalf("point the org at the stub GitHub: %v", err)
	}

	bind := "/api/orgs/" + orgID.String() + "/github/identity/pat"
	rec = r.tokensJSON("POST", bind, map[string]any{"pat": "ghp_wrong"}, "", tok.Token)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bind with a PAT GitHub rejects = %d, want 422 (the handler's own fault, not the credential's): %s",
			rec.Code, rec.Body.String())
	}
	if f := errorItems(t, rec)[0].Field; f != "github_pat" {
		t.Errorf("bind fault field = %q, want github_pat", f)
	}

	rec = r.tokensJSON("POST", bind, map[string]any{"pat": pat}, "", tok.Token)
	if rec.Code != http.StatusOK {
		t.Fatalf("bind under a token = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var login, source string
	if err := r.h.AdminDB.QueryRow(`
		SELECT login, source FROM user_github_identities WHERE user_id = $1 AND github_base_url = $2
	`, userID, github.URL).Scan(&login, &source); err != nil {
		t.Fatalf("identity row after the token's bind: %v", err)
	}
	if login != "wizard-user" || source != "pat" {
		t.Errorf("identity row = (%q, %q), want (wizard-user, pat)", login, source)
	}
}

// ---------- expiry under the org's cap ----------

// TestBearerAuth_OrgCapExpiresTokenAtUse pins that the cap is applied at use,
// not at mint: a token older than the cap is refused the moment the cap says
// so, with a body indistinguishable from an unknown token's, and it works again
// the moment the cap is lifted. Nothing about the token row changed in between.
func TestBearerAuth_OrgCapExpiresTokenAtUse(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "cap-at-use-org")
	sid := r.signIn(userID)

	old := r.createToken(sid, map[string]any{"name": "forty-days-old", "org_id": orgID.String()})
	if _, err := r.h.AdminDB.Exec(
		`UPDATE user_api_tokens SET created_at = now() - interval '40 days' WHERE id = $1`, old.ID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	fresh := r.createToken(sid, map[string]any{"name": "minted-today", "org_id": orgID.String()})

	me := func(token string) *http.Response {
		return r.serve(r.bearerReq("GET", "/api/me", token)).Result()
	}
	if got := me(old.Token); got.StatusCode != http.StatusOK {
		t.Fatalf("old token in an uncapped org = %d, want 200", got.StatusCode)
	}

	r.setTokenAgeCap(orgID, 30)

	expired := r.serve(r.bearerReq("GET", "/api/me", old.Token))
	unknown := r.serve(r.bearerReq("GET", "/api/me", "tf_"+strings.Repeat("C", 43)))
	if expired.Code != http.StatusUnauthorized {
		t.Fatalf("40-day-old token under a 30-day cap = %d, want 401: %s", expired.Code, expired.Body.String())
	}
	if expired.Body.String() != unknown.Body.String() {
		t.Errorf("cap-expired body %q differs from unknown-token body %q; the refusals must be indistinguishable",
			expired.Body.String(), unknown.Body.String())
	}
	if got := me(fresh.Token); got.StatusCode != http.StatusOK {
		t.Errorf("today's token under the same cap = %d, want 200", got.StatusCode)
	}

	// The list read tells the owner why: the effective expiry is in the past
	// while the stored one is still null.
	for _, item := range r.listTokens(sid, "", map[string]any{}).Items {
		if item.ID != old.ID {
			continue
		}
		if item.ExpiresAt != nil {
			t.Errorf("expires_at = %v on a token minted with none", *item.ExpiresAt)
		}
		if item.EffectiveExpiresAt == nil {
			t.Fatal("effective_expires_at = null under a cap the token has outlived")
		}
		eff, err := time.Parse(time.RFC3339, *item.EffectiveExpiresAt)
		if err != nil {
			t.Fatalf("parse effective_expires_at: %v", err)
		}
		if !eff.Before(time.Now()) {
			t.Errorf("effective_expires_at = %s, want in the past", eff)
		}
	}

	if _, err := r.h.AdminDB.Exec(
		`UPDATE org_settings SET api_token_max_age_days = NULL WHERE org_id = $1`, orgID); err != nil {
		t.Fatalf("lift the cap: %v", err)
	}
	if got := me(old.Token); got.StatusCode != http.StatusOK {
		t.Errorf("old token after the cap is lifted = %d, want 200 (the cap binds at use, not at mint)", got.StatusCode)
	}
}

// ---------- org scope on the sub-routes ----------

// TestBearerAuth_OrgScopeSealsSubRoutes extends the sealed-scope rule past the
// org's own read to the routes mounted beneath it, reads and writes alike: a
// token bound to A gets 404 on every /api/orgs/B/... it names, for a caller who
// is a member of both. The same routes on A answer, which is what proves the
// 404 is the scope's doing and not a route that was never there.
func TestBearerAuth_OrgScopeSealsSubRoutes(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgA, _ := r.seedOrg(userID, "sealed-sub-a")
	orgB, _ := r.seedOrg(userID, "sealed-sub-b")
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
	t.Cleanup(entitlements.Reset)
	_, plaintext := r.mintToken(userID, orgA, "a-bound")

	routes := []struct {
		name, method, suffix string
		body                 any
	}{
		{"org settings read", "GET", "/settings", nil},
		{"org settings write", "PATCH", "/settings", map[string]any{"api_token_max_age_days": 30, "version": 1}},
		{"token policy", "GET", "/api-token-policy", nil},
		{"access log list", "POST", "/usage/access-log/list", map[string]any{}},
		{"event sources", "GET", "/sources", nil},
	}
	for _, tc := range routes {
		t.Run(tc.name, func(t *testing.T) {
			other := r.tokensJSON(tc.method, "/api/orgs/"+orgB.String()+tc.suffix, tc.body, "", plaintext)
			if other.Code != http.StatusNotFound {
				t.Errorf("%s %s on the other org = %d, want 404: %s", tc.method, tc.suffix, other.Code, other.Body.String())
			}
			own := r.tokensJSON(tc.method, "/api/orgs/"+orgA.String()+tc.suffix, tc.body, "", plaintext)
			if own.Code == http.StatusNotFound {
				t.Errorf("%s %s on the token's own org = 404; the route must exist for the 404 above to mean anything: %s",
					tc.method, tc.suffix, own.Body.String())
			}
			if own.Code == http.StatusUnauthorized || own.Code == http.StatusForbidden {
				t.Errorf("%s %s on the token's own org = %d; the token must reach its own org: %s",
					tc.method, tc.suffix, own.Code, own.Body.String())
			}
		})
	}
}

// ---------- governance ----------

// TestAPITokens_GovernanceLogAfterRotation is the audit half of the drive: after
// a session mints a bounded token and the token rotates itself, the org's
// access log carries one row per event, each naming the actor and everything a
// reviewer needs to recognize the credential — name, prefix, the bounds it was
// minted under — and never the secret. Read under both credentials, since the
// org admin who audits may well be doing it headlessly.
func TestAPITokens_GovernanceLogAfterRotation(t *testing.T) {
	r := newAuthRig(t)
	userID := r.seedUser()
	orgID, _ := r.seedOrg(userID, "governed-org")
	entitlements.RegisterProvider(entitlements.Static(entitlements.FeatureGovernance))
	t.Cleanup(entitlements.Reset)
	sid := r.signIn(userID)
	r.setTokenAgeCap(orgID, 90)
	// The login just made rewrote the display name from the identity's claims;
	// the log resolves actors through the same row, so read it rather than
	// assume the seed's value.
	var actor string
	if err := r.h.AdminDB.QueryRow(`SELECT display_name FROM users WHERE id = $1`, userID).Scan(&actor); err != nil {
		t.Fatalf("read display name: %v", err)
	}

	expires := time.Now().Add(14 * 24 * time.Hour).UTC().Truncate(time.Second)
	first := r.createToken(sid, map[string]any{
		"name": "deploy", "org_id": orgID.String(),
		"expires_at":    expires.Format(time.RFC3339),
		"allowed_cidrs": []string{"192.0.2.0/24", "2001:db8::/32"},
	})
	rec := r.tokensJSON("POST", "/api/me/tokens", map[string]any{"name": "deploy", "org_id": orgID.String()}, "", first.Token)
	if rec.Code != http.StatusCreated {
		t.Fatalf("token-authed mint = %d: %s", rec.Code, rec.Body.String())
	}
	var second tokenCreated
	if err := json.Unmarshal(rec.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec := r.tokensJSON("DELETE", "/api/me/tokens/"+first.ID, nil, "", second.Token); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke original with replacement = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := r.serve(r.bearerReq("GET", "/api/me", first.Token)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("original after rotation = %d, want 401", rec.Code)
	}

	type row struct {
		Action      string `json:"action"`
		ActionLabel string `json:"action_label"`
		ActorName   string `json:"actor_name"`
		Detail      struct {
			TokenID      string     `json:"token_id"`
			Name         string     `json:"name"`
			Prefix       string     `json:"prefix"`
			Source       string     `json:"source"`
			ExpiresAt    *time.Time `json:"expires_at"`
			MaxAgeDays   *int       `json:"max_age_days"`
			AllowedCIDRs []string   `json:"allowed_cidrs"`
		} `json:"detail_json"`
	}
	readLog := func(sid, bearer string) map[string]row {
		t.Helper()
		rec := r.tokensJSON("POST", "/api/orgs/"+orgID.String()+"/usage/access-log/list",
			map[string]any{"category": domain.AccessCategoryCredential}, sid, bearer)
		if rec.Code != http.StatusOK {
			t.Fatalf("access log = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Items []row `json:"items"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode access log: %v (body=%s)", err, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), first.Token) || strings.Contains(rec.Body.String(), second.Token) {
			t.Fatal("the access log carries a token plaintext")
		}
		byKey := map[string]row{}
		for _, it := range out.Items {
			if !strings.HasPrefix(it.Action, "api_token_") {
				continue
			}
			byKey[it.Action+":"+it.Detail.TokenID] = it
		}
		return byKey
	}

	for _, cred := range []struct{ name, sid, bearer string }{
		{"session", sid, ""},
		{"token", "", second.Token},
	} {
		t.Run(cred.name, func(t *testing.T) {
			got := readLog(cred.sid, cred.bearer)
			if len(got) != 3 {
				t.Fatalf("api_token rows = %d, want 3 (two mints, one revoke): %+v", len(got), got)
			}

			minted, ok := got[domain.AccessActionAPITokenCreated+":"+first.ID]
			if !ok {
				t.Fatalf("no api_token_created row for the first token")
			}
			if minted.ActorName != actor {
				t.Errorf("actor = %q, want the minter's display name %q", minted.ActorName, actor)
			}
			if minted.Detail.Name != "deploy" || minted.Detail.Prefix != first.TokenPrefix {
				t.Errorf("created detail = {%q %q}, want {deploy %q}", minted.Detail.Name, minted.Detail.Prefix, first.TokenPrefix)
			}
			if minted.Detail.ExpiresAt == nil || !minted.Detail.ExpiresAt.Equal(expires) {
				t.Errorf("created expires_at = %v, want %s", minted.Detail.ExpiresAt, expires)
			}
			if minted.Detail.MaxAgeDays == nil || *minted.Detail.MaxAgeDays != 90 {
				t.Errorf("created max_age_days = %v, want the cap in force at mint (90)", minted.Detail.MaxAgeDays)
			}
			if strings.Join(minted.Detail.AllowedCIDRs, ",") != "192.0.2.0/24,2001:db8::/32" {
				t.Errorf("created allowed_cidrs = %v, want both ranges", minted.Detail.AllowedCIDRs)
			}
			for _, want := range []string{"deploy", first.TokenPrefix, "2 IP ranges"} {
				if !strings.Contains(minted.ActionLabel, want) {
					t.Errorf("created label %q does not name %q", minted.ActionLabel, want)
				}
			}

			rotated, ok := got[domain.AccessActionAPITokenCreated+":"+second.ID]
			if !ok {
				t.Fatalf("no api_token_created row for the token-authed mint")
			}
			if rotated.ActorName != actor {
				t.Errorf("token-authed mint actor = %q, want the token's owner %q", rotated.ActorName, actor)
			}

			revoked, ok := got[domain.AccessActionAPITokenRevoked+":"+first.ID]
			if !ok {
				t.Fatalf("no api_token_revoked row for the rotated-out token")
			}
			if revoked.Detail.Name != "deploy" || revoked.Detail.Prefix != first.TokenPrefix {
				t.Errorf("revoked detail = {%q %q}, want {deploy %q}", revoked.Detail.Name, revoked.Detail.Prefix, first.TokenPrefix)
			}
			if revoked.Detail.Source != "" {
				t.Errorf("owner revoke carries source %q, want none (only deprovisioning marks a source)", revoked.Detail.Source)
			}
			if !strings.Contains(revoked.ActionLabel, first.TokenPrefix) {
				t.Errorf("revoked label %q does not name the prefix", revoked.ActionLabel)
			}
		})
	}
}
