package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// gotrueSSOFunc is core's client for GoTrue's public SP-initiated /sso endpoint.
// The SSO feature itself lives in ee/sso, but this low-level GoTrue client is
// core (it backs ExtensionAPI.GotrueSSO), so its unit tests stay here.

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
