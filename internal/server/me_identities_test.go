package server

import (
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
	// seedUser) plus a SAML identity linked to the SAME principal. Both carry
	// A's email — the verified-email link case — so current-marking can only be
	// right if it keys on auth_user_id, not email.
	alice := r.seedUser()
	aliceEmail := alice.String() + "@test"

	samlAuthID := uuid.NewString()
	r.h.SeedAuthUser(t, samlAuthID, aliceEmail)
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
	bodyBytes, _ := io.ReadAll(got.Body)
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
