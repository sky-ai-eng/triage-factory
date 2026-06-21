package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TFAC-426 — SP-initiated SAML login + JIT provisioning + cross-org isolation.
//
// NOTE on account merge (ticket §4): the ticket said "GitHub-then-Entra, same
// verified email → one auth.users". The TFAC-423 spike follow-up established
// from GoTrue v2.189.0 source that this does NOT happen — SSO is excluded from
// GoTrue's account linking (an `sso:` provider's linking domain is the provider
// itself; the email-grouping path filters is_sso_user=false), so a SAML login
// is ALWAYS its own auth.users. There is therefore no TF-side merge to test or
// build; claims.Subject is the SAML user's own id. The relevant guard we DO
// assert is that a SAML login writes no github identity row
// (TestSSOLogin_WritesNoGitHubIdentityRow).
//
// The other consequence: TF never reads the GoTrue JWT to discover which SSO
// provider this login used (provider / app_metadata / amr are GoTrue-internal +
// version-fragile). It carries the provider_id in its OWN signed state cookie
// (start → callback). The adversarial isolation test below proves TF ignores a
// mismatched app_metadata provider in the JWT and keys only on its state.

// ---------- SSO test helpers (extend the authRig in auth_handlers_test.go) ----------

// seedSSOConnection inserts an sso_connections row binding providerID → orgID.
// Mirrors what TFAC-424's handler writes after GoTrue registers the provider,
// without standing up GoTrue. role "" falls to the schema default ('member').
func (r *authRig) seedSSOConnection(orgID uuid.UUID, providerID, role string, enabled bool) {
	r.t.Helper()
	if role == "" {
		role = "member"
	}
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO sso_connections (org_id, provider_id, default_role, enabled)
		VALUES ($1, $2, $3::public.org_role, $4)
	`, orgID, providerID, role, enabled); err != nil {
		r.t.Fatalf("seed sso_connection: %v", err)
	}
}

// seedAuthOnlyUser seeds an auth.users row WITHOUT a public.users row — a
// genuine first-time login, where the callback's upsertUserFromClaims is what
// creates public.users. (seedUser, by contrast, pre-creates both.)
func (r *authRig) seedAuthOnlyUser() uuid.UUID {
	r.t.Helper()
	var idStr string
	if err := r.h.AdminDB.QueryRow(`SELECT gen_random_uuid()`).Scan(&idStr); err != nil {
		r.t.Fatalf("gen uuid: %v", err)
	}
	r.h.SeedAuthUser(r.t, idStr, idStr+"@corp.example")
	return uuid.MustParse(idStr)
}

// validSAMLClaimsFor returns JWT claims for a SAML login. jwtProviderID drives
// the app_metadata.provider value (a real Entra login carries "sso:<id>") — TF
// must IGNORE it and key on its signed state, so the isolation test deliberately
// passes a value that differs from the state's provider_id. user_metadata
// carries a preferred_username (an Entra UPN, NOT a github handle) to prove
// upsertUserFromClaims won't mint a github identity row from it.
func validSAMLClaimsFor(userID uuid.UUID, jwtProviderID string) jwt.MapClaims {
	return jwt.MapClaims{
		"sub":   userID.String(),
		"email": userID.String() + "@corp.example",
		"iss":   testIssuer,
		"aud":   testAudience,
		"exp":   time.Now().Add(1 * time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"app_metadata": map[string]any{
			"provider":  "sso:" + jwtProviderID,
			"providers": []any{"sso:" + jwtProviderID},
		},
		"user_metadata": map[string]any{
			"preferred_username": userID.String() + "@corp.example",
			"full_name":          "SAML User",
		},
	}
}

// driveSAMLCallback completes a SAML login at the callback handler. stateProviderID
// is what TF's signed state carries (the value the JIT keys on); jwtProviderID is
// what the minted JWT advertises in app_metadata (pass the same value for an
// "honest" login). Returns the callback response (carrying the sid Set-Cookie).
func (r *authRig) driveSAMLCallback(userID uuid.UUID, stateProviderID, jwtProviderID string) *http.Response {
	r.t.Helper()
	token := r.signKey.mintJWT(r.t, validSAMLClaimsFor(userID, jwtProviderID))
	refresh := "refresh-" + uuid.NewString()
	r.srv.authDeps.gotrueExchange = func(ctx context.Context, code, verifier string) (string, string, int64, error) {
		return token, refresh, time.Now().Add(time.Hour).Unix(), nil
	}

	csrf := "test-csrf-" + uuid.NewString()
	state := stateClaims{
		ReturnTo:     "/dashboard",
		CSRF:         csrf,
		CodeVerifier: "test-pkce-verifier",
		ProviderID:   stateProviderID,
		ExpiresAt:    time.Now().Add(10 * time.Minute).Unix(),
	}
	signed, err := state.sign(r.srv.deployCfg.hmacKey)
	if err != nil {
		r.t.Fatalf("sign state: %v", err)
	}

	req := httptest.NewRequest("GET", "/api/auth/callback?state="+csrf+"&code=fake-"+uuid.NewString(), nil)
	req.AddCookie(&http.Cookie{Name: stateCookieName, Value: signed})
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	return rec.Result()
}

type meBody struct {
	ID          string `json:"id"`
	ActiveOrgID string `json:"active_org_id"`
	Orgs        []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"orgs"`
}

func (r *authRig) me(sid string) meBody {
	r.t.Helper()
	resp := r.requestWithSid("GET", "/api/me", sid)
	if resp.StatusCode != http.StatusOK {
		r.t.Fatalf("/api/me status=%d, want 200", resp.StatusCode)
	}
	var m meBody
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		r.t.Fatalf("decode /api/me: %v", err)
	}
	return m
}

// membershipCount returns how many org_memberships rows a user holds (optionally
// scoped to one org when orgID != uuid.Nil).
func (r *authRig) membershipCount(userID, orgID uuid.UUID) int {
	r.t.Helper()
	var n int
	q := `SELECT count(*) FROM org_memberships WHERE user_id = $1`
	args := []any{userID}
	if orgID != uuid.Nil {
		q += ` AND org_id = $2`
		args = append(args, orgID)
	}
	if err := r.h.AdminDB.QueryRow(q, args...).Scan(&n); err != nil {
		r.t.Fatalf("count memberships: %v", err)
	}
	return n
}

// principalOf resolves a GoTrue login identity (auth.users id) to its principal
// (public.users id) via the user_identities link the callback writes. A genuine
// first login mints a fresh principal, so memberships land on the principal, not
// the auth identity — assertions must key on this.
func (r *authRig) principalOf(authUserID uuid.UUID) uuid.UUID {
	r.t.Helper()
	var pid uuid.UUID
	if err := r.h.AdminDB.QueryRow(
		`SELECT user_id FROM user_identities WHERE auth_user_id = $1`, authUserID,
	).Scan(&pid); err != nil {
		r.t.Fatalf("resolve principal for identity %s: %v", authUserID, err)
	}
	return pid
}

// ---------- callback / JIT tests ----------

// Existing member via SAML → session, lands in their org.
func TestSSOLogin_ExistingMember_LandsInOrg(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedUser()
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		user, orgA); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	resp := r.driveSAMLCallback(user, providerID, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	me := r.me(sid)
	if me.ActiveOrgID != orgA.String() {
		t.Errorf("active_org_id=%q, want %q", me.ActiveOrgID, orgA)
	}
	if got := r.membershipCount(user, uuid.Nil); got != 1 {
		t.Errorf("memberships=%d, want 1 (grant must be idempotent for an existing member)", got)
	}
	if len(me.Orgs) != 1 || me.Orgs[0].Role != "member" {
		t.Errorf("orgs=%+v, want one org with role member (JIT grants, never re-roles)", me.Orgs)
	}
}

// New user via SAML (provider bound to org A) → JIT 'member' in org A, lands in
// the app (active org set), not onboarding.
func TestSSOLogin_NewUser_JITProvisionsMember(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedAuthOnlyUser() // genuine first login: no public.users yet
	resp := r.driveSAMLCallback(user, providerID, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	principal := r.principalOf(user)
	if got := r.membershipCount(principal, uuid.Nil); got != 1 {
		t.Fatalf("memberships=%d, want exactly 1", got)
	}
	var gotOrg, gotRole string
	if err := r.h.AdminDB.QueryRow(
		`SELECT org_id::text, role FROM org_memberships WHERE user_id=$1`, principal,
	).Scan(&gotOrg, &gotRole); err != nil {
		t.Fatalf("read membership: %v", err)
	}
	if gotOrg != orgA.String() || gotRole != "member" {
		t.Errorf("membership = (%s, %s), want (%s, member)", gotOrg, gotRole, orgA)
	}

	me := r.me(sid)
	if me.ActiveOrgID != orgA.String() {
		t.Errorf("active_org_id=%q, want %q (a JIT'd user must land in the app, not onboarding)", me.ActiveOrgID, orgA)
	}
}

// Isolation: a provider bound to org A never provisions into org B — even when
// the JWT's app_metadata advertises org B's provider. The JIT keys on TF's
// signed state (the provider_id TF itself chose at start), not on anything the
// assertion carries.
func TestSSOLogin_Isolation_ProviderBoundToANeverProvisionsB(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerA := uuid.NewString()
	providerB := uuid.NewString()
	ownerA := r.seedUser()
	orgA, _ := r.seedOrg(ownerA, "org-a")
	ownerB := r.seedUser()
	orgB, _ := r.seedOrg(ownerB, "org-b")
	r.seedSSOConnection(orgA, providerA, "member", true)
	r.seedSSOConnection(orgB, providerB, "member", true)

	// Authenticate through A's provider (state=providerA) while the JWT lies and
	// claims B's provider in app_metadata.
	user := r.seedAuthOnlyUser()
	resp := r.driveSAMLCallback(user, providerA, providerB)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	principal := r.principalOf(user)
	if got := r.membershipCount(principal, orgA); got != 1 {
		t.Errorf("org A memberships=%d, want 1", got)
	}
	if got := r.membershipCount(principal, orgB); got != 0 {
		t.Errorf("LEAK: org B memberships=%d, want 0 — A's provider provisioned into B", got)
	}
	if me := r.me(sid); me.ActiveOrgID != orgA.String() {
		t.Errorf("active_org_id=%q, want %q (TF must trust its state, not the JWT app_metadata)", me.ActiveOrgID, orgA)
	}
}

// SSO login routes to the SSO org even when the user already belongs to an
// earlier org — the active org is the org whose IdP they came through, not the
// earliest-membership default a non-SSO login would pick.
func TestSSOLogin_ActiveOrgIsSSOOrg_NotEarliestMembership(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerA := uuid.NewString()
	ownerA := r.seedUser()
	orgA, _ := r.seedOrg(ownerA, "sso-org")
	r.seedSSOConnection(orgA, providerA, "member", true)

	user := r.seedUser()
	ownerX := r.seedUser()
	orgX, _ := r.seedOrg(ownerX, "other-org")
	if _, err := r.h.AdminDB.Exec(
		`INSERT INTO org_memberships (user_id, org_id, role) VALUES ($1, $2, 'member')`,
		user, orgX); err != nil {
		t.Fatalf("seed prior membership: %v", err)
	}

	resp := r.driveSAMLCallback(user, providerA, providerA)
	sid := r.sidFromResp(resp)

	me := r.me(sid)
	if me.ActiveOrgID != orgA.String() {
		t.Errorf("active_org_id=%q, want SSO org %q (not earliest membership %q)", me.ActiveOrgID, orgA, orgX)
	}
	if got := r.membershipCount(user, uuid.Nil); got != 2 {
		t.Errorf("memberships=%d, want 2 (kept org X, added org A)", got)
	}
}

// New user via SAML whose provider_id has no TF connection → no provisioning,
// falls through to onboarding (active org omitted). Guards the case where the
// connection vanished between start and callback (start would otherwise 404).
func TestSSOLogin_NoMatchingConnection_FallsToOnboarding(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	user := r.seedAuthOnlyUser()
	resp := r.driveSAMLCallback(user, uuid.NewString(), uuid.NewString()) // no seedSSOConnection
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	if got := r.membershipCount(user, uuid.Nil); got != 0 {
		t.Errorf("memberships=%d, want 0 (no accidental provisioning without a binding)", got)
	}
	me := r.me(sid)
	if me.ActiveOrgID != "" {
		t.Errorf("active_org_id=%q, want empty (zero-membership onboarding)", me.ActiveOrgID)
	}
}

// A disabled connection provisions nothing (login still succeeds; the user just
// isn't JIT'd).
func TestSSOLogin_DisabledConnection_NoProvisioning(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", false) // disabled

	user := r.seedAuthOnlyUser()
	resp := r.driveSAMLCallback(user, providerID, providerID)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	if got := r.membershipCount(user, uuid.Nil); got != 0 {
		t.Errorf("memberships=%d, want 0 (a disabled connection must not JIT)", got)
	}
}

// A SAML login writes no user_github_identities row — even when the assertion
// carries a preferred_username (which is a UPN, not a github handle).
func TestSSOLogin_WritesNoGitHubIdentityRow(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	user := r.seedAuthOnlyUser()
	r.driveSAMLCallback(user, providerID, providerID)

	var n int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM user_github_identities WHERE user_id = $1`, user,
	).Scan(&n); err != nil {
		t.Fatalf("count github identities: %v", err)
	}
	if n != 0 {
		t.Errorf("user_github_identities rows=%d, want 0 for a SAML login", n)
	}
}

// A raw IdP-initiated assertion (no TF state cookie) is rejected at the callback
// — reinforces SP-initiated-only.
func TestSSOLogin_RawIdPInitiated_NoStateCookie_Rejected(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	req := httptest.NewRequest("GET", "/api/auth/callback?state=x&code=y", nil)
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (missing state cookie → reject)", rec.Code)
	}
}

// ---------- start-endpoint tests ----------

// GET /api/auth/oauth/saml?provider_id=… → 303 to the IdP, with TF's signed
// state cookie carrying the provider_id + a PKCE verifier whose S256 challenge
// is what TF posted to GoTrue.
func TestSAMLStart_RedirectsToIdP(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", true)

	var gotProvider, gotRedirect, gotChallenge string
	r.srv.authDeps.gotrueSSO = func(ctx context.Context, pid, redirectTo, challenge string) (string, error) {
		gotProvider, gotRedirect, gotChallenge = pid, redirectTo, challenge
		return "https://login.microsoftonline.com/saml?SAMLRequest=abc", nil
	}

	req := httptest.NewRequest("GET", "/api/auth/oauth/saml?provider_id="+providerID+"&return_to=/dashboard", nil)
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://login.microsoftonline.com/saml?SAMLRequest=abc" {
		t.Errorf("Location=%q, want the IdP redirect GoTrue returned", loc)
	}
	if gotProvider != providerID {
		t.Errorf("provider_id posted to GoTrue=%q, want %q", gotProvider, providerID)
	}
	wantRedirectPrefix := r.srv.deployCfg.publicURL + "/api/auth/callback?state="
	if len(gotRedirect) < len(wantRedirectPrefix) || gotRedirect[:len(wantRedirectPrefix)] != wantRedirectPrefix {
		t.Errorf("redirect_to=%q, want prefix %q", gotRedirect, wantRedirectPrefix)
	}

	var stateVal string
	for _, c := range rec.Result().Cookies() {
		if c.Name == stateCookieName {
			stateVal = c.Value
		}
	}
	if stateVal == "" {
		t.Fatal("no state cookie set")
	}
	sc, err := parseStateCookie(stateVal, r.srv.deployCfg.hmacKey)
	if err != nil {
		t.Fatalf("parse state cookie: %v", err)
	}
	if sc.ProviderID != providerID {
		t.Errorf("state provider_id=%q, want %q", sc.ProviderID, providerID)
	}
	if sc.ReturnTo != "/dashboard" {
		t.Errorf("state return_to=%q, want /dashboard", sc.ReturnTo)
	}
	if sc.CodeVerifier == "" {
		t.Error("state carries no code_verifier")
	}
	if gotChallenge != pkceChallenge(sc.CodeVerifier) {
		t.Errorf("challenge posted to GoTrue is not S256(verifier in state)")
	}
}

// An unknown or disabled provider_id → 404, and TF never calls GoTrue.
func TestSAMLStart_UnknownOrDisabledProvider_404(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	called := false
	r.srv.authDeps.gotrueSSO = func(ctx context.Context, _, _, _ string) (string, error) {
		called = true
		return "", nil
	}

	// Unknown provider_id (no row at all).
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/auth/oauth/saml?provider_id="+uuid.NewString(), nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown provider status=%d, want 404", rec.Code)
	}

	// Disabled connection.
	providerID := uuid.NewString()
	owner := r.seedUser()
	orgA, _ := r.seedOrg(owner, "acme")
	r.seedSSOConnection(orgA, providerID, "member", false)
	rec2 := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/auth/oauth/saml?provider_id="+providerID, nil))
	if rec2.Code != http.StatusNotFound {
		t.Errorf("disabled provider status=%d, want 404", rec2.Code)
	}

	// A malformed (non-UUID) provider_id. provider_id is a `text` column treated
	// as an opaque handle — GetByProviderID compares text = text with no ::uuid
	// cast — so a garbage value matches no row → 404, NOT a DB cast error / 500.
	rec3 := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec3, httptest.NewRequest("GET", "/api/auth/oauth/saml?provider_id=not-a-uuid", nil))
	if rec3.Code != http.StatusNotFound {
		t.Errorf("malformed provider status=%d, want 404 (opaque text key, no cast)", rec3.Code)
	}

	if called {
		t.Error("GoTrue /sso was called for an unknown/disabled/malformed provider")
	}
}

// Missing provider_id → 400.
func TestSAMLStart_MissingProviderID_400(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	rec := httptest.NewRecorder()
	r.srv.mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/auth/oauth/saml", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", rec.Code)
	}
}

// gotrueSSOFunc posts JSON to the PUBLIC /sso with NO Authorization header and
// forwards the 303 Location without following it.
func TestGoTrueSSO_PublicNoAuth_Forwards303(t *testing.T) {
	var (
		gotAuth, gotCT, gotPath string
		gotBody                 map[string]string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotAuth = req.Header.Get("Authorization")
		gotCT = req.Header.Get("Content-Type")
		gotPath = req.URL.Path
		_ = json.NewDecoder(req.Body).Decode(&gotBody)
		w.Header().Set("Location", "https://idp.example/saml?SAMLRequest=zzz")
		w.WriteHeader(http.StatusSeeOther)
	}))
	defer upstream.Close()

	srv := &Server{}
	cfg := &authConfig{gotrueURL: upstream.URL}
	loc, err := srv.gotrueSSOFunc(cfg)(context.Background(), "prov-123", "https://tf/cb?state=x", "challenge-abc")
	if err != nil {
		t.Fatalf("gotrueSSO: %v", err)
	}
	if loc != "https://idp.example/saml?SAMLRequest=zzz" {
		t.Errorf("location=%q", loc)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header sent to public /sso: %q (must be none)", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", gotCT)
	}
	if gotPath != "/sso" {
		t.Errorf("path=%q, want /sso", gotPath)
	}
	if gotBody["provider_id"] != "prov-123" ||
		gotBody["redirect_to"] != "https://tf/cb?state=x" ||
		gotBody["code_challenge"] != "challenge-abc" {
		t.Errorf("body=%v", gotBody)
	}
	if gotBody["code_challenge_method"] != "S256" {
		t.Errorf("code_challenge_method=%q, want S256 (RFC 7636 canonical, matching /authorize)", gotBody["code_challenge_method"])
	}
}

// A non-303 response from GoTrue (e.g. a 400 for a bad provider) surfaces as an
// error rather than a bogus empty redirect.
func TestGoTrueSSO_Non303_Errors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, `{"error":"bad provider"}`, http.StatusBadRequest)
	}))
	defer upstream.Close()

	srv := &Server{}
	cfg := &authConfig{gotrueURL: upstream.URL}
	if _, err := srv.gotrueSSOFunc(cfg)(context.Background(), "p", "r", "c"); err == nil {
		t.Fatal("expected an error for a non-303 /sso response, got nil")
	}
}
