package sso_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/db/pgtest"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TFAC-427 — identifier-first SSO login discovery (POST /api/sso/discover).
// The endpoint is pre-login (no session), so these requests carry no sid; the
// admin-pool GetVerifiedByDomain read is what the rig exercises end-to-end.

// ssoDiscoverResponse / ssoStartURL moved to ee/sso with the handler. These
// integration tests assert against the wire shape the ee endpoint emits and
// depend on the package-server authRig harness, so they keep faithful local
// copies rather than importing ee.
type ssoDiscoverResponse struct {
	SSO      bool   `json:"sso"`
	StartURL string `json:"start_url,omitempty"`
}

func ssoStartURL(providerID, returnTo string) string {
	q := url.Values{}
	q.Set("provider_id", providerID)
	q.Set("return_to", returnTo)
	return "/api/auth/oauth/saml?" + q.Encode()
}

// ssoDiscover POSTs an email to the discovery endpoint as an anonymous visitor
// (no sid cookie — the whole point is that the caller is pre-login) and decodes
// the reply. Returns the decoded body + the raw text (for assertions about what
// the body does NOT contain).
//
// It fails fast on a non-200 status or a body that isn't the expected JSON:
// every discovery answer (sso:true and sso:false alike) is a 200, so a non-200
// or undecodable body is always a regression — and if the helper swallowed it,
// the zero-value ssoDiscoverResponse ({sso:false}) would let "false" assertions
// pass silently. The one deliberate non-200 (a malformed BODY → 400) is tested
// separately against the handler directly, not through this helper.
func ssoDiscover(r *authRig, body any) (ssoDiscoverResponse, string) {
	r.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			r.t.Fatalf("marshal discover body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sso/discover", rdr)
	req.Header.Set("Content-Type", "application/json")
	resp := r.serve(req)
	raw := readBody(resp)
	if resp.StatusCode != http.StatusOK {
		r.t.Fatalf("POST /api/sso/discover: status = %d; want 200 (body=%s)", resp.StatusCode, raw)
	}
	var out ssoDiscoverResponse
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		r.t.Fatalf("POST /api/sso/discover: decode %q: %v", raw, err)
	}
	return out, raw
}

// seedVerifiedSSODomain inserts an sso_connections row + a VERIFIED sso_domains
// row for domainName, via the admin pool (a fixture; bypasses RLS). Returns the
// connection's provider_id (for start_url assertions) and its id (for the
// privacy test, which proves neither leaks into the response).
func seedVerifiedSSODomain(t *testing.T, h *pgtest.Harness, orgID, domainName string, enabled bool) (providerID, connID string) {
	t.Helper()
	providerID = uuid.NewString()
	if err := h.AdminDB.QueryRow(`
		INSERT INTO sso_connections (org_id, provider_id, enabled) VALUES ($1, $2, $3)
		RETURNING id::text
	`, orgID, providerID, enabled).Scan(&connID); err != nil {
		t.Fatalf("seed sso_connection: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO sso_domains (connection_id, org_id, domain, token, verified_at)
		VALUES ($1, $2, $3, $4, now())
	`, connID, orgID, domainName, "tok-"+uuid.NewString()); err != nil {
		t.Fatalf("seed verified sso_domain: %v", err)
	}
	return providerID, connID
}

// seedPendingSSODomain inserts an sso_connections row + a PENDING (verified_at
// NULL) sso_domains row, via the admin pool. A pending claim must be inert —
// discovery routes only verified domains.
func seedPendingSSODomain(t *testing.T, h *pgtest.Harness, orgID, domainName string) {
	t.Helper()
	var connID string
	if err := h.AdminDB.QueryRow(`
		INSERT INTO sso_connections (org_id, provider_id) VALUES ($1, $2)
		RETURNING id::text
	`, orgID, uuid.NewString()).Scan(&connID); err != nil {
		t.Fatalf("seed sso_connection: %v", err)
	}
	if _, err := h.AdminDB.Exec(`
		INSERT INTO sso_domains (connection_id, org_id, domain, token)
		VALUES ($1, $2, $3, $4)
	`, connID, orgID, domainName, "tok-"+uuid.NewString()); err != nil {
		t.Fatalf("seed pending sso_domain: %v", err)
	}
}

// TestSSODiscover_VerifiedDomain_RoutesToSSO: a verified email domain on an
// enabled connection returns {sso:true} + the SP-initiated start_url carrying
// that connection's provider_id and the visitor's return_to.
func TestSSODiscover_VerifiedDomain_RoutesToSSO(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-disco")
	providerID, _ := seedVerifiedSSODomain(t, r.h, orgID.String(), "corp.com", true)

	body, raw := ssoDiscover(r, map[string]string{
		"email":     "alice@corp.com",
		"return_to": "/dashboard",
	})
	if !body.SSO {
		t.Fatalf("sso = false; want true for a verified domain (body=%s)", raw)
	}
	want := ssoStartURL(providerID, "/dashboard")
	if body.StartURL != want {
		t.Errorf("start_url = %q; want %q", body.StartURL, want)
	}
	// The start_url is the same SP-initiated endpoint handleSAMLStart serves.
	if !strings.HasPrefix(body.StartURL, "/api/auth/oauth/saml?") {
		t.Errorf("start_url = %q; want the /api/auth/oauth/saml start endpoint", body.StartURL)
	}
}

// TestSSODiscover_HostileReturnTo_ClampedToRoot: a hostile return_to must not
// survive into start_url — the handler runs it through normalizeReturnTo, which
// clamps an absolute/off-site URL to "/". normalizeReturnTo is unit-tested in
// its own right; this locks in that the discovery handler actually calls it, so
// dropping that call in a refactor fails here rather than shipping an open
// redirect through the SSO start link.
func TestSSODiscover_HostileReturnTo_ClampedToRoot(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-disco-returnto")
	seedVerifiedSSODomain(t, r.h, orgID.String(), "corp.com", true)

	body, _ := ssoDiscover(r, map[string]string{
		"email":     "alice@corp.com",
		"return_to": "https://evil.com",
	})
	if !body.SSO {
		t.Fatalf("expected an SSO match to exercise return_to normalization")
	}
	if strings.Contains(body.StartURL, "evil.com") {
		t.Errorf("hostile return_to leaked into start_url: %s", body.StartURL)
	}
}

// TestSSODiscover_VerifiedDomain_DrivesIntoSAMLStart: the returned start_url,
// when followed, actually initiates the SP flow — discovery hands the login
// page a URL that handleSAMLStart accepts (303 to the IdP), not a dead link.
func TestSSODiscover_VerifiedDomain_DrivesIntoSAMLStart(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-disco-drive")
	seedVerifiedSSODomain(t, r.h, orgID.String(), "corp.com", true)

	body, raw := ssoDiscover(r, map[string]string{"email": "alice@corp.com", "return_to": "/dashboard"})
	if !body.SSO || body.StartURL == "" {
		t.Fatalf("discovery did not route to SSO (body=%s)", raw)
	}

	// Follow the start_url. The fake GoTrue's /sso returns the IdP redirect; a 303
	// proves the provider_id discovery emitted resolves to a real, enabled
	// connection at the start endpoint.
	r.fake.ssoLocation = "https://login.microsoftonline.com/saml?SAMLRequest=abc"
	resp := r.serve(httptest.NewRequest("GET", body.StartURL, nil))
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("following start_url: status = %d; want 303 to the IdP (body=%s)", resp.StatusCode, readBody(resp))
	}
	if loc := resp.Header.Get("Location"); loc != "https://login.microsoftonline.com/saml?SAMLRequest=abc" {
		t.Errorf("Location = %q; want the IdP redirect", loc)
	}
}

// TestSSODiscover_UnknownDomain_FallsToGitHub: an unverified/unknown domain
// returns {sso:false} with no start_url, so the login page stays on GitHub.
func TestSSODiscover_UnknownDomain_FallsToGitHub(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-disco-unknown")
	seedVerifiedSSODomain(t, r.h, orgID.String(), "corp.com", true)

	body, _ := ssoDiscover(r, map[string]string{"email": "bob@other-co.com"})
	if body.SSO || body.StartURL != "" {
		t.Errorf("response = %+v; want {sso:false} for an unknown domain", body)
	}
}

// TestSSODiscover_PendingDomain_FallsToGitHub: a domain that's only *claimed*
// (pending, verified_at NULL) does not route — pending claims are inert.
func TestSSODiscover_PendingDomain_FallsToGitHub(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-disco-pending")
	seedPendingSSODomain(t, r.h, orgID.String(), "pending.com")

	body, raw := ssoDiscover(r, map[string]string{"email": "x@pending.com"})
	if body.SSO || body.StartURL != "" {
		t.Errorf("response = %+v; want {sso:false} for a pending-only claim (body=%s)", body, raw)
	}
}

// TestSSODiscover_DisabledConnection_FallsToGitHub: a verified domain whose
// connection is disabled must not route — a disabled connection 404s at the
// start endpoint, so discovery collapses it into {sso:false} (working GitHub
// login over a dead SSO redirect).
func TestSSODiscover_DisabledConnection_FallsToGitHub(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-disco-disabled")
	seedVerifiedSSODomain(t, r.h, orgID.String(), "disabled.com", false) // enabled=false

	body, raw := ssoDiscover(r, map[string]string{"email": "x@disabled.com"})
	if body.SSO || body.StartURL != "" {
		t.Errorf("response = %+v; want {sso:false} for a disabled connection (body=%s)", body, raw)
	}
}

// TestSSODiscover_ExactMatchOnly_NoSuffix: discovery matches the EXACT email
// domain. A verified parent (corp.com) never covers a subdomain address
// (alice@eng.corp.com), and a verified subdomain (eng.example.com) never covers
// the parent address (x@example.com). No suffix / wildcard, ever.
func TestSSODiscover_ExactMatchOnly_NoSuffix(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-disco-exact")
	seedVerifiedSSODomain(t, r.h, orgID.String(), "corp.com", true)
	seedVerifiedSSODomain(t, r.h, orgID.String(), "eng.example.com", true)

	// Parent claim does not cover a subdomain address.
	if body, raw := ssoDiscover(r, map[string]string{"email": "alice@eng.corp.com"}); body.SSO {
		t.Errorf("alice@eng.corp.com routed on a bare corp.com claim — suffix match leaked (body=%s)", raw)
	}
	// Subdomain claim does not cover the parent address.
	if body, raw := ssoDiscover(r, map[string]string{"email": "x@example.com"}); body.SSO {
		t.Errorf("x@example.com routed on an eng.example.com claim — parent match leaked (body=%s)", raw)
	}
	// Sanity: the exact domain DOES route, so the negatives above are real
	// (the rows exist and route), not a seeding miss.
	if body, _ := ssoDiscover(r, map[string]string{"email": "bob@corp.com"}); !body.SSO {
		t.Error("bob@corp.com did not route to its exact verified domain")
	}
}

// TestSSODiscover_CaseInsensitive: the domain match is case-insensitive
// (lower(domain) on both sides), so a mixed-case address still routes.
func TestSSODiscover_CaseInsensitive(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-disco-case")
	seedVerifiedSSODomain(t, r.h, orgID.String(), "corp.com", true)

	body, raw := ssoDiscover(r, map[string]string{"email": "Alice@CORP.Com"})
	if !body.SSO {
		t.Errorf("mixed-case address did not route (body=%s)", raw)
	}
}

// TestSSODiscover_Privacy_NoOrgIdentity: a match returns ONLY {sso, start_url}.
// The response must never reveal org identity — no org id, org slug, or
// connection id — only the opaque provider_id. This is the TFAC-422 trust-model
// bar: mutually-distrusting orgs on one deployment.
func TestSSODiscover_Privacy_NoOrgIdentity(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	const slug = "super-secret-acme"
	orgID, _ := r.seedOrg(owner, slug)
	providerID, connID := seedVerifiedSSODomain(t, r.h, orgID.String(), "corp.com", true)

	body, raw := ssoDiscover(r, map[string]string{"email": "alice@corp.com"})
	if !body.SSO {
		t.Fatalf("expected a match to assert the privacy shape against (body=%s)", raw)
	}
	// The opaque provider_id IS allowed (the start endpoint needs it; the My
	// Apps tile already exposes it). Everything that names the org is not.
	if !strings.Contains(raw, providerID) {
		t.Errorf("start_url should carry the provider_id %q (body=%s)", providerID, raw)
	}
	for _, leak := range []struct{ what, val string }{
		{"org id", orgID.String()},
		{"org slug", slug},
		{"connection id", connID},
	} {
		if leak.val != "" && strings.Contains(raw, leak.val) {
			t.Errorf("response leaked %s (%q): %s", leak.what, leak.val, raw)
		}
	}

	// And the JSON has exactly the two intended keys — no stray field could
	// carry org identity in a future change.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("decode response object: %v", err)
	}
	for k := range fields {
		if k != "sso" && k != "start_url" {
			t.Errorf("unexpected response field %q — discovery must return only sso + start_url", k)
		}
	}
}

// TestSSODiscover_MalformedEmail_FallsToGitHub: a well-formed request whose
// email isn't a real address is {sso:false} (a 200, NOT a 400 — that's reserved
// for a malformed BODY). The login page just keeps the GitHub button.
func TestSSODiscover_MalformedEmail_FallsToGitHub(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)
	owner := r.seedUser()
	orgID, _ := r.seedOrg(owner, "sso-disco-malformed")
	seedVerifiedSSODomain(t, r.h, orgID.String(), "corp.com", true)

	for _, email := range []string{"", "not-an-email", "@corp.com", "user@", "a@b@corp.com"} {
		// ssoDiscover asserts the 200 (a malformed email is sso:false, NOT the
		// 400 a malformed BODY gets); here we only check it didn't route.
		body, _ := ssoDiscover(r, map[string]string{"email": email})
		if body.SSO || body.StartURL != "" {
			t.Errorf("email %q: response = %+v; want {sso:false}", email, body)
		}
	}
}

// TestSSODiscover_CrossOrgIsolation: two orgs, each with its own verified
// domain + connection. A discover for org A's domain routes to A's provider and
// never to B's — the verified-domain → connection mapping is per-org.
func TestSSODiscover_CrossOrgIsolation(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeMulti)
	r := newAuthRig(t)

	ownerA := r.seedUser()
	orgA, _ := r.seedOrg(ownerA, "sso-disco-a")
	provA, _ := seedVerifiedSSODomain(t, r.h, orgA.String(), "a-corp.com", true)

	ownerB := r.seedUser()
	orgB, _ := r.seedOrg(ownerB, "sso-disco-b")
	provB, _ := seedVerifiedSSODomain(t, r.h, orgB.String(), "b-corp.com", true)

	bodyA, rawA := ssoDiscover(r, map[string]string{"email": "x@a-corp.com"})
	if !bodyA.SSO || !strings.Contains(bodyA.StartURL, provA) {
		t.Errorf("a-corp.com did not route to org A's provider %q (body=%s)", provA, rawA)
	}
	if strings.Contains(bodyA.StartURL, provB) {
		t.Errorf("a-corp.com leaked org B's provider %q (body=%s)", provB, rawA)
	}
}
