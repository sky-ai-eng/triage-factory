package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// ssoTestServiceToken is the static service-role bearer the tests configure via
// TF_GOTRUE_SERVICE_ROLE_TOKEN; the fake GoTrue checks TF forwards it verbatim.
// Opaque here — TF treats the token as opaque and just presents it, so the
// handler test only needs to prove forwarding. (jwk-init's own tests cover that
// the real token is a valid RS256 service-role JWT with a matching kid.)
const ssoTestServiceToken = "test-service-role-token-deadbeefdeadbeefdeadbeefdeadbeef"

// ssoConnectionView / ssoConnectionResponse moved to ee/sso with the handler.
// These pgtest integration tests depend on the package-server authRig harness,
// so they keep faithful local copies of the wire shape the ee endpoint emits.
type ssoConnectionView struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	ProviderID  string    `json:"provider_id"`
	DefaultRole string    `json:"default_role"`
	Enabled     bool      `json:"enabled"`
	Enforced    bool      `json:"enforced"`
	Tested      bool      `json:"tested"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type ssoConnectionResponse struct {
	Connection *ssoConnectionView `json:"connection"`
	EntityID   string             `json:"entity_id"`
	ACSURL     string             `json:"acs_url"`
}

// envServiceRoleToken moved to ee/sso; the test asserts TF forwards this env's
// token to GoTrue, so it keeps a local copy of the name.
const envServiceRoleToken = "TF_GOTRUE_SERVICE_ROLE_TOKEN"

// ssoFakeGoTrue is a minimal stand-in for GoTrue's admin SSO API. It checks the
// bearer matches the configured service-role token, records the POST it
// receives, and returns a fresh provider id (a UUID) on 201 — the shape
// createSAMLProvider reads.
type ssoFakeGoTrue struct {
	srv       *httptest.Server
	wantToken string

	mu        sync.Mutex
	postCount int
	lastBody  map[string]any
}

func newSSOFakeGoTrue(t *testing.T, wantToken string) *ssoFakeGoTrue {
	t.Helper()
	f := &ssoFakeGoTrue{wantToken: wantToken}
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/sso/providers", func(w http.ResponseWriter, r *http.Request) {
		// GoTrue authenticates the admin call by the service-role token; here we
		// assert TF presented exactly the configured static token.
		if r.Header.Get("Authorization") != "Bearer "+f.wantToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.postCount++
		f.lastBody = body
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"id": uuid.NewString()})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *ssoFakeGoTrue) posts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postCount
}

func (f *ssoFakeGoTrue) lastPostBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

// newSSORig builds a multi-mode auth rig wired to point GoTrue admin calls at a
// fake, with the service-role token set in the env. Returns the rig + the fake
// so tests can assert what TF sent to GoTrue.
func newSSORig(t *testing.T) (*authRig, *ssoFakeGoTrue) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeMulti)
	fake := newSSOFakeGoTrue(t, ssoTestServiceToken)
	t.Setenv(envServiceRoleToken, ssoTestServiceToken)
	r := newAuthRig(t)
	// The rig wires SetAuthDeps with an unused GoTrue URL; repoint it at the
	// fake. The handler reads it lazily via s.gotrueAdminBaseURL on each call.
	r.srv.authCfg.gotrueURL = fake.srv.URL
	return r, fake
}

// TestSSOConnectionCreate_NoServiceToken_503: with TF_GOTRUE_SERVICE_ROLE_TOKEN
// unset, registration returns a clear 503 (operator must run jwk-init) and never
// calls GoTrue — the deployment simply isn't set up for SSO.
func TestSSOConnectionCreate_NoServiceToken_503(t *testing.T) {
	r, fake := newSSORig(t)
	owner := r.seedUser()
	r.seedOrg(owner, "sso-no-token")
	sid := r.signInAs(owner)

	t.Setenv(envServiceRoleToken, "") // override: token not configured

	resp := doInviteReq(r, http.MethodPost, "/api/sso/connection", sid,
		map[string]string{"metadata_url": "https://idp/metadata.xml"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 when service-role token unset (body=%s)", resp.StatusCode, readBody(resp))
	}
	if fake.posts() != 0 {
		t.Errorf("GoTrue POSTs = %d; want 0 (no token → no admin call)", fake.posts())
	}
}

// TestSSOConnectionCreate_HappyPath: an org admin registers a connection →
// GoTrue's admin API is called once with the spike-confirmed body, and a
// sso_connections row lands for the caller's org bound to the new provider.
func TestSSOConnectionCreate_HappyPath(t *testing.T) {
	r, fake := newSSORig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-happy")
	sid := r.signInAs(owner)

	const metadataURL = "https://login.microsoftonline.com/tenant/federationmetadata.xml"
	resp := doInviteReq(r, http.MethodPost, "/api/sso/connection", sid,
		map[string]string{"metadata_url": metadataURL})
	raw := readBody(resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d; want 201 (body=%s)", resp.StatusCode, raw)
	}

	var body ssoConnectionResponse
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Connection == nil {
		t.Fatal("connection nil; want the created connection")
	}
	if _, perr := uuid.Parse(body.Connection.ProviderID); perr != nil {
		t.Errorf("provider_id %q is not a UUID: %v", body.Connection.ProviderID, perr)
	}
	if body.Connection.DefaultRole != "member" {
		t.Errorf("default_role = %q; want member", body.Connection.DefaultRole)
	}
	if !body.Connection.Enabled {
		t.Error("enabled = false; want true on fresh registration")
	}

	// GoTrue POSTed exactly once, with the spike-confirmed body shape.
	if fake.posts() != 1 {
		t.Errorf("GoTrue POSTs = %d; want 1", fake.posts())
	}
	pb := fake.lastPostBody()
	if pb["type"] != "saml" {
		t.Errorf("POST type = %v; want saml", pb["type"])
	}
	if pb["metadata_url"] != metadataURL {
		t.Errorf("POST metadata_url = %v; want %q", pb["metadata_url"], metadataURL)
	}
	if doms, ok := pb["domains"].([]any); !ok || len(doms) != 0 {
		t.Errorf("POST domains = %v; want [] (domains are out of scope here)", pb["domains"])
	}

	// A row was written for the caller's org, bound to the minted provider.
	var (
		gotProvider, gotRole string
		gotEnabled           bool
	)
	if err := r.h.AdminDB.QueryRow(
		`SELECT provider_id, default_role::text, enabled FROM sso_connections WHERE org_id=$1`, orgID,
	).Scan(&gotProvider, &gotRole, &gotEnabled); err != nil {
		t.Fatalf("read sso_connections: %v", err)
	}
	if gotProvider != body.Connection.ProviderID {
		t.Errorf("row provider_id = %q; want %q", gotProvider, body.Connection.ProviderID)
	}
	if gotRole != "member" || !gotEnabled {
		t.Errorf("row role/enabled = %q/%v; want member/true", gotRole, gotEnabled)
	}
}

// TestSSOConnectionCreate_NonAdmin_404: a plain org member can't register a
// connection — 404 (non-disclosure), no GoTrue call, no row.
func TestSSOConnectionCreate_NonAdmin_404(t *testing.T) {
	r, fake := newSSORig(t)
	owner := r.seedUser()
	orgID, teamID := r.seedOrg(owner, "sso-nonadmin")
	member := r.seedUser()
	pgtest.AddOrgMember(t, r.h, member.String(), orgID.String(), teamID.String(), "member", "member")
	sid := r.signInAs(member)

	resp := doInviteReq(r, http.MethodPost, "/api/sso/connection", sid,
		map[string]string{"metadata_url": "https://idp/metadata.xml"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 (non-admin); body=%s", resp.StatusCode, readBody(resp))
	}
	if fake.posts() != 0 {
		t.Errorf("GoTrue POSTs = %d; want 0 (gated before registration)", fake.posts())
	}
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM sso_connections WHERE org_id=$1`, orgID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rows = %d; want 0", n)
	}
}

// TestSSOConnectionCreate_Idempotent: re-registering reuses the existing
// provider/row — 200, same provider_id, no second GoTrue provider, one row.
func TestSSOConnectionCreate_Idempotent(t *testing.T) {
	r, fake := newSSORig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-idem")
	sid := r.signInAs(owner)

	first := doInviteReq(r, http.MethodPost, "/api/sso/connection", sid,
		map[string]string{"metadata_url": "https://idp/metadata.xml"})
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first status = %d; want 201 (body=%s)", first.StatusCode, readBody(first))
	}
	var fb ssoConnectionResponse
	_ = json.Unmarshal([]byte(readBody(first)), &fb)

	// A second call (even with a different metadata URL) is a no-op reuse:
	// rotating the metadata is a deferred follow-up, so no 2nd provider.
	second := doInviteReq(r, http.MethodPost, "/api/sso/connection", sid,
		map[string]string{"metadata_url": "https://idp/metadata-changed.xml"})
	raw := readBody(second)
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second status = %d; want 200 idempotent (body=%s)", second.StatusCode, raw)
	}
	var sb ssoConnectionResponse
	_ = json.Unmarshal([]byte(raw), &sb)
	if sb.Connection == nil || fb.Connection == nil {
		t.Fatal("connection nil across idempotent re-register")
	}
	if sb.Connection.ProviderID != fb.Connection.ProviderID {
		t.Errorf("provider_id changed: %q -> %q", fb.Connection.ProviderID, sb.Connection.ProviderID)
	}
	if fake.posts() != 1 {
		t.Errorf("GoTrue POSTs = %d; want 1 (re-register must reuse, not mint a 2nd)", fake.posts())
	}
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM sso_connections WHERE org_id=$1`, orgID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("rows = %d; want 1 (no duplicate)", n)
	}
}

// TestSSOConnectionUpdate_EnableDisable: PATCH flips enabled both ways and the
// change round-trips; the disable does not alter the default role.
func TestSSOConnectionUpdate_EnableDisable(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	r.seedOrg(owner, "sso-toggle")
	sid := r.signInAs(owner)

	create := doInviteReq(r, http.MethodPost, "/api/sso/connection", sid,
		map[string]string{"metadata_url": "https://idp/metadata.xml"})
	if create.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d (body=%s)", create.StatusCode, readBody(create))
	}

	dis := doInviteReq(r, http.MethodPatch, "/api/sso/connection", sid,
		map[string]bool{"enabled": false})
	raw := readBody(dis)
	if dis.StatusCode != http.StatusOK {
		t.Fatalf("disable status = %d; want 200 (body=%s)", dis.StatusCode, raw)
	}
	var disBody ssoConnectionResponse
	_ = json.Unmarshal([]byte(raw), &disBody)
	if disBody.Connection == nil || disBody.Connection.Enabled {
		t.Errorf("after disable enabled = %v; want false", disBody.Connection)
	}
	if disBody.Connection.DefaultRole != "member" {
		t.Errorf("disable changed default_role to %q; want member preserved", disBody.Connection.DefaultRole)
	}

	en := doInviteReq(r, http.MethodPatch, "/api/sso/connection", sid,
		map[string]bool{"enabled": true})
	var enBody ssoConnectionResponse
	_ = json.Unmarshal([]byte(readBody(en)), &enBody)
	if enBody.Connection == nil || !enBody.Connection.Enabled {
		t.Errorf("after re-enable enabled = %v; want true", enBody.Connection)
	}
}

// TestSSOConnectionUpdate_NoConnection_404: PATCH with nothing registered is a
// 404.
func TestSSOConnectionUpdate_NoConnection_404(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	r.seedOrg(owner, "sso-patch-empty")
	sid := r.signInAs(owner)

	resp := doInviteReq(r, http.MethodPatch, "/api/sso/connection", sid,
		map[string]bool{"enabled": false})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 (no connection)", resp.StatusCode)
	}
}

// TestSSOConnectionGet_ReturnsSPValues: GET returns the SP Entity-ID/ACS values
// (always, even with no connection) and surfaces the connection once registered.
func TestSSOConnectionGet_ReturnsSPValues(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	r.seedOrg(owner, "sso-get")
	sid := r.signInAs(owner)

	wantEntity := r.srv.deployCfg.publicURL + "/auth/v1/sso/saml/metadata"
	wantACS := r.srv.deployCfg.publicURL + "/auth/v1/sso/saml/acs"

	// Before registration: SP values present, connection null.
	pre := doInviteReq(r, http.MethodGet, "/api/sso/connection", sid, nil)
	rawPre := readBody(pre)
	if pre.StatusCode != http.StatusOK {
		t.Fatalf("get(pre) status = %d (body=%s)", pre.StatusCode, rawPre)
	}
	var pb ssoConnectionResponse
	if err := json.Unmarshal([]byte(rawPre), &pb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pb.EntityID != wantEntity {
		t.Errorf("entity_id = %q; want %q", pb.EntityID, wantEntity)
	}
	if pb.ACSURL != wantACS {
		t.Errorf("acs_url = %q; want %q", pb.ACSURL, wantACS)
	}
	if pb.Connection != nil {
		t.Errorf("connection = %+v; want nil before registration", pb.Connection)
	}

	// After registration: connection present, SP values unchanged.
	doInviteReq(r, http.MethodPost, "/api/sso/connection", sid,
		map[string]string{"metadata_url": "https://idp/metadata.xml"})
	post := doInviteReq(r, http.MethodGet, "/api/sso/connection", sid, nil)
	var ab ssoConnectionResponse
	_ = json.Unmarshal([]byte(readBody(post)), &ab)
	if ab.Connection == nil {
		t.Fatal("connection nil after registration")
	}
	if ab.EntityID != wantEntity || ab.ACSURL != wantACS {
		t.Errorf("SP values changed across GET: entity=%q acs=%q", ab.EntityID, ab.ACSURL)
	}
}

// TestSSOConnectionGet_NoPublicURL_500: a missing deployment public URL is a
// server misconfiguration — the handler fails fast with a 500 rather than
// emitting relative SP paths (e.g. "/auth/v1/sso/saml/metadata") into the
// response. Uses GET (no CSRF) so a blanked publicURL doesn't trip the
// same-origin check.
func TestSSOConnectionGet_NoPublicURL_500(t *testing.T) {
	r, _ := newSSORig(t)
	owner := r.seedUser()
	r.seedOrg(owner, "sso-no-public-url")
	sid := r.signInAs(owner)

	// Blank the public URL after sign-in (the rig wired it for the OAuth flow);
	// publicURL() now returns "".
	r.srv.deployCfg.publicURL = ""

	resp := doInviteReq(r, http.MethodGet, "/api/sso/connection", sid, nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 when public URL unconfigured (body=%s)", resp.StatusCode, readBody(resp))
	}
}

// TestSSOConnectionCreate_NonHTTPSMetadata_400: metadata_url must be a valid
// https URL — SSRF defense-in-depth, since GoTrue fetches it server-side from
// inside the compose network. Non-https / malformed values are rejected before
// any GoTrue call or row write.
func TestSSOConnectionCreate_NonHTTPSMetadata_400(t *testing.T) {
	r, fake := newSSORig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-ssrf")
	sid := r.signInAs(owner)

	for _, bad := range []string{
		"http://postgres:5432/",
		"http://169.254.169.254/latest/meta-data/",
		"ftp://idp/metadata.xml",
		"not a url",
	} {
		resp := doInviteReq(r, http.MethodPost, "/api/sso/connection", sid,
			map[string]string{"metadata_url": bad})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("metadata_url=%q: status = %d; want 400", bad, resp.StatusCode)
		}
	}
	if fake.posts() != 0 {
		t.Errorf("GoTrue POSTs = %d; want 0 (rejected before registration)", fake.posts())
	}
	var n int
	if err := r.h.AdminDB.QueryRow(`SELECT count(*) FROM sso_connections WHERE org_id=$1`, orgID.String()).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rows = %d; want 0", n)
	}
}

// TestSSOConnection_OrgIsolation: org B's admin can neither see nor modify org
// A's connection. This is the cross-org isolation boundary TFAC-426's JIT trusts
// (provider_id → exactly one org), enforced by RLS + requireOrg + the org-scoped
// store reads — the non-admin 404 covers privilege, this covers tenancy.
func TestSSOConnection_OrgIsolation(t *testing.T) {
	r, _ := newSSORig(t)
	ownerA := r.seedUser()
	orgA, _ := r.seedOrg(ownerA, "sso-iso-a")
	ownerB := r.seedUser()
	orgB, _ := r.seedOrg(ownerB, "sso-iso-b")

	// Org A registers a connection.
	sidA := r.signInAs(ownerA)
	if resp := doInviteReq(r, http.MethodPost, "/api/sso/connection", sidA,
		map[string]string{"metadata_url": "https://idp-a/metadata.xml"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("org A create status = %d (body=%s)", resp.StatusCode, readBody(resp))
	}

	sidB := r.signInAs(ownerB)

	// Org B's GET must not surface org A's connection (RLS-scoped read).
	getB := doInviteReq(r, http.MethodGet, "/api/sso/connection", sidB, nil)
	if getB.StatusCode != http.StatusOK {
		t.Fatalf("org B get status = %d (body=%s)", getB.StatusCode, readBody(getB))
	}
	var bResp ssoConnectionResponse
	_ = json.Unmarshal([]byte(readBody(getB)), &bResp)
	if bResp.Connection != nil {
		t.Errorf("org B sees connection %+v; want nil (org A's must be invisible)", bResp.Connection)
	}

	// Org B's PATCH finds no connection in its own org → 404; it cannot reach A.
	if resp := doInviteReq(r, http.MethodPatch, "/api/sso/connection", sidB,
		map[string]bool{"enabled": false}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("org B patch status = %d; want 404 (no connection in B)", resp.StatusCode)
	}

	// Org A's connection is untouched (still enabled) after B's attempts.
	var enabled bool
	if err := r.h.AdminDB.QueryRow(
		`SELECT enabled FROM sso_connections WHERE org_id=$1`, orgA.String()).Scan(&enabled); err != nil {
		t.Fatalf("read org A connection: %v", err)
	}
	if !enabled {
		t.Error("org A connection disabled by org B's PATCH — isolation breach")
	}
	var bCount int
	if err := r.h.AdminDB.QueryRow(
		`SELECT count(*) FROM sso_connections WHERE org_id=$1`, orgB.String()).Scan(&bCount); err != nil {
		t.Fatalf("count org B: %v", err)
	}
	if bCount != 0 {
		t.Errorf("org B has %d connections; want 0", bCount)
	}
}

// TestSSOConnection_LocalMode404 moved to ee/sso/handlers_test.go with the
// connection handler.
