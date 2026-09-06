package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// loginMethodWire mirrors the handleMeIdentities response row for decoding.
type loginMethodWire struct {
	Provider      string `json:"provider"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	LinkedAt      string `json:"linked_at"`
	Current       bool   `json:"current"`
	IdP           string `json:"idp"`
	Login         string `json:"login"`
}

type identitiesWire struct {
	Methods []loginMethodWire `json:"methods"`
}

// TestMeIdentities_MultiReturnsLinkedRows_MarksCurrent_NoLeak is the multi-mode
// acceptance: a principal holding a GitHub + a SAML identity gets both rows back
// from GET /api/me/identities; the identity backing the session is flagged
// current (precisely — by auth_user_id, even though both identities share an
// email); a second principal's identities never leak; and the opaque bridge
// columns are never serialized.
func TestMeIdentities_MultiReturnsLinkedRows_MarksCurrent_NoLeak(t *testing.T) {
	r := newAuthRig(t)

	// Principal A: a GitHub identity (auth_user_id == A == principal, seeded by
	// seedUser) plus a SAML identity linked to the SAME principal (added below).
	alice := r.seedUser()
	aliceEmail := alice.String() + "@test"

	// The SAML login is its OWN auth.users row — GoTrue never merges SSO logins
	// (see sso_login_test.go's spike note); TF unifies the two at the
	// user_identities layer, which is what this read surfaces. Two facts shape the
	// seeding:
	//   - auth.users.email is globally unique in the harness image, so the SAML
	//     row needs its own auth-level email (the endpoint never reads it).
	//   - the verified email that LINKED the two to one principal is the same on
	//     both user_identities rows. That shared email is the point of the test:
	//     a "current" marker keyed on email would light up BOTH rows, so only an
	//     auth_user_id-keyed one (what the handler does) passes the checks below.
	samlAuthID := uuid.NewString()
	r.h.SeedAuthUser(t, samlAuthID, samlAuthID+"@test")
	if _, err := r.h.AdminDB.Exec(`
		INSERT INTO user_identities (auth_user_id, user_id, provider, email, email_verified)
		VALUES ($1, $2, 'saml', $3, true)`, samlAuthID, alice, aliceEmail); err != nil {
		t.Fatalf("seed saml identity: %v", err)
	}

	// Principal B: an unrelated user with its own identity. Must never appear in
	// A's response.
	bob := r.seedUser()
	bobEmail := bob.String() + "@test"

	// Log in as Alice via GitHub. email_verified=true keeps the GitHub identity
	// verified through the login-time refresh (validClaimsFor omits the claim,
	// which would otherwise unverify it).
	claims := validClaimsFor(alice)
	claims["email_verified"] = true
	resp, _ := r.driveCallbackClaims(claims)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	got := r.requestWithSid("GET", "/api/me/identities", sid)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/me/identities status=%d, want 200", got.StatusCode)
	}
	bodyBytes, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = got.Body.Close()
	body := string(bodyBytes)

	var out identitiesWire
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, body)
	}

	if len(out.Methods) != 2 {
		t.Fatalf("got %d methods, want 2 (github+saml); body=%s", len(out.Methods), body)
	}

	byProvider := map[string]loginMethodWire{}
	for _, m := range out.Methods {
		byProvider[m.Provider] = m
	}
	gh, okGH := byProvider["github"]
	saml, okSAML := byProvider["saml"]
	if !okGH || !okSAML {
		t.Fatalf("expected providers github+saml, got %v", byProvider)
	}

	// The session was opened with the GitHub login, so exactly that identity is
	// current — despite the SAML identity sharing the same email.
	if !gh.Current {
		t.Error("github identity should be marked current (it backs the session)")
	}
	if saml.Current {
		t.Error("saml identity must NOT be current (the session is the github login)")
	}

	// The github row is known by its handle at GitHub — the login claim's
	// user_name, stamped at sign-in — and the saml row has none.
	if want := "test-user-" + alice.String()[:8]; gh.Login != want {
		t.Errorf("github identity login = %q, want %q", gh.Login, want)
	}
	if saml.Login != "" {
		t.Errorf("saml identity login = %q, want none", saml.Login)
	}

	// Email + verified flag flow through faithfully.
	for _, m := range out.Methods {
		if m.Email != aliceEmail {
			t.Errorf("provider %s email=%q, want %q", m.Provider, m.Email, aliceEmail)
		}
		if !m.EmailVerified {
			t.Errorf("provider %s email_verified=false, want true", m.Provider)
		}
		if m.LinkedAt == "" {
			t.Errorf("provider %s linked_at empty, want an RFC3339 timestamp", m.Provider)
		}
	}

	// No cross-principal leak: Bob's identity / email must be absent.
	if strings.Contains(body, bobEmail) {
		t.Errorf("response leaked another principal's identity (bob email present); body=%s", body)
	}

	// The opaque bridge columns are never exposed — neither the field names nor
	// the SAML identity's auth_user_id (a UUID embedded in no email).
	for _, banned := range []string{"auth_user_id", "provider_subject", samlAuthID} {
		if strings.Contains(body, banned) {
			t.Errorf("response exposed %q; body=%s", banned, body)
		}
	}
}

// idpProbeExtension is the login extension the idp test installs: the no-op
// everywhere except the read seam, which answers from a fixed map and records
// what it was asked, so the test can pin both the wire result and the "asked
// once, distinct ids only" shape of the call.
type idpProbeExtension struct {
	noopLoginExtension
	idps  map[string]string
	asked [][]string
}

func (e *idpProbeExtension) IdPForProviders(_ context.Context, ids []string) (map[string]string, error) {
	e.asked = append(e.asked, append([]string(nil), ids...))
	return e.idps, nil
}

// TestMeIdentities_SamlRowsCarryIdPFromExtension pins the vendor-mark contract:
// a saml identity stamped with a provider id the extension recognises carries
// `idp`; a github row never does; a saml row the extension cannot place (an
// unstamped one, or a provider with no recorded IdP) omits the field rather
// than sending "" — absent is the wire spelling of "plain SSO".
func TestMeIdentities_SamlRowsCarryIdPFromExtension(t *testing.T) {
	r := newAuthRig(t)
	const knownProvider = "aaaaaaaa-0000-0000-0000-000000000001"
	const unplacedProvider = "aaaaaaaa-0000-0000-0000-000000000002"
	probe := &idpProbeExtension{idps: map[string]string{knownProvider: "okta"}}
	r.srv.loginExt = probe

	alice := r.seedUser()
	aliceEmail := alice.String() + "@test"
	seedSAML := func(providerID string) string {
		id := uuid.NewString()
		r.h.SeedAuthUser(t, id, id+"@test")
		if _, err := r.h.AdminDB.Exec(`
			INSERT INTO user_identities (auth_user_id, user_id, provider, sso_provider_id, email, email_verified)
			VALUES ($1, $2, 'saml', NULLIF($3, ''), $4, true)`, id, alice, providerID, aliceEmail); err != nil {
			t.Fatalf("seed saml identity: %v", err)
		}
		return id
	}
	known := seedSAML(knownProvider)
	unplaced := seedSAML(unplacedProvider)
	unstamped := seedSAML("")

	claims := validClaimsFor(alice)
	claims["email_verified"] = true
	resp, _ := r.driveCallbackClaims(claims)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("callback status=%d, want 302", resp.StatusCode)
	}
	sid := r.sidFromResp(resp)

	got := r.requestWithSid("GET", "/api/me/identities", sid)
	if got.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/me/identities status=%d, want 200", got.StatusCode)
	}
	bodyBytes, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	_ = got.Body.Close()

	// Decode into raw maps as well as the wire struct: the struct cannot tell
	// an absent idp from an empty one, and absent is the contract.
	var raw struct {
		Methods []map[string]any `json:"methods"`
	}
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, bodyBytes)
	}
	if len(raw.Methods) != 4 {
		t.Fatalf("got %d methods, want 4 (github + 3 saml); body=%s", len(raw.Methods), bodyBytes)
	}
	// Rows are keyed by linked_at order, which the seeds share to the second;
	// identify them by (provider, idp) instead, since that is the assertion.
	var withIdP, samlWithout, github int
	for _, m := range raw.Methods {
		v, has := m["idp"]
		switch m["provider"] {
		case "github":
			github++
			if has {
				t.Errorf("github row carries idp=%v; a github login has no IdP", v)
			}
		case "saml":
			if has {
				withIdP++
				if v != "okta" {
					t.Errorf("saml idp=%v, want okta", v)
				}
			} else {
				samlWithout++
			}
		}
	}
	if github != 1 || withIdP != 1 || samlWithout != 2 {
		t.Errorf("rows: github=%d saml-with-idp=%d saml-without=%d, want 1/1/2 (known=%s unplaced=%s unstamped=%s); body=%s",
			github, withIdP, samlWithout, known, unplaced, unstamped, bodyBytes)
	}

	// One call, the distinct stamped ids only — the unstamped row contributes
	// nothing to ask about.
	if len(probe.asked) != 1 {
		t.Fatalf("extension asked %d times, want 1: %v", len(probe.asked), probe.asked)
	}
	askedSet := map[string]bool{}
	for _, id := range probe.asked[0] {
		askedSet[id] = true
	}
	if len(probe.asked[0]) != 2 || !askedSet[knownProvider] || !askedSet[unplacedProvider] {
		t.Errorf("extension asked %v, want exactly {%s, %s}", probe.asked[0], knownProvider, unplacedProvider)
	}
}

// TestMeIdentities_LocalReturnsSyntheticRow pins the N=1 stub: SQLite has no
// user_identities table and no GoTrue, so the endpoint returns a single
// synthetic "local" row, marked current, keeping the wire contract uniform.
func TestMeIdentities_LocalReturnsSyntheticRow(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)

	rec := doJSON(t, s, "GET", "/api/me/identities", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s, want 200", rec.Code, rec.Body.String())
	}

	var out identitiesWire
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, rec.Body.String())
	}
	if len(out.Methods) != 1 {
		t.Fatalf("got %d methods, want exactly 1 synthetic row; body=%s", len(out.Methods), rec.Body.String())
	}
	if out.Methods[0].Provider != "local" {
		t.Errorf("local row provider=%q, want \"local\"", out.Methods[0].Provider)
	}
	if !out.Methods[0].Current {
		t.Error("local row should be marked current (it's the only identity)")
	}
}
