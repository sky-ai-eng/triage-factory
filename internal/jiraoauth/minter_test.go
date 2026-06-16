package jiraoauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/jira"
)

func testApp() jira.OAuthApp {
	return jira.OAuthApp{ClientID: "client-123", ClientSecret: "secret-xyz"}
}

// TestExchangeCode_ParsesTokenSet pins that ExchangeCode posts the
// authorization_code grant with the app credentials and parses the token set,
// computing the expiry from expires_in off the injected clock.
func TestExchangeCode_ParsesTokenSet(t *testing.T) {
	now := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"acc1","refresh_token":"ref1","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()

	m := &Minter{httpClient: srv.Client(), tokenURL: srv.URL, now: func() time.Time { return now }}
	tok, err := m.ExchangeCode(context.Background(), testApp(), "the-code", "https://tf.example/callback")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "acc1" || tok.RefreshToken != "ref1" {
		t.Fatalf("token = %+v, want acc1/ref1", tok)
	}
	if want := now.Add(3600 * time.Second); !tok.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", tok.ExpiresAt, want)
	}
	if gotForm.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q, want authorization_code", gotForm.Get("grant_type"))
	}
	if gotForm.Get("client_id") != "client-123" || gotForm.Get("client_secret") != "secret-xyz" {
		t.Errorf("client creds not sent: %v", gotForm)
	}
	if gotForm.Get("code") != "the-code" || gotForm.Get("redirect_uri") != "https://tf.example/callback" {
		t.Errorf("code/redirect_uri not sent: %v", gotForm)
	}
}

// TestRefresh_RotatesAndUsesGrant pins the refresh_token grant + that the
// rotated refresh token comes back.
func TestRefresh_RotatesAndUsesGrant(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotForm = r.PostForm
		_, _ = w.Write([]byte(`{"access_token":"acc2","refresh_token":"ref2","expires_in":3600}`))
	}))
	defer srv.Close()

	m := &Minter{httpClient: srv.Client(), tokenURL: srv.URL}
	tok, err := m.Refresh(context.Background(), testApp(), "ref1")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.RefreshToken != "ref2" {
		t.Errorf("rotated refresh = %q, want ref2", tok.RefreshToken)
	}
	if gotForm.Get("grant_type") != "refresh_token" || gotForm.Get("refresh_token") != "ref1" {
		t.Errorf("refresh grant not sent correctly: %v", gotForm)
	}
}

// TestRequestToken_EndpointError surfaces a logical error (invalid_grant) as
// ErrTokenEndpoint, distinct from a transport failure.
func TestRequestToken_EndpointError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token is invalid"}`))
	}))
	defer srv.Close()

	m := &Minter{httpClient: srv.Client(), tokenURL: srv.URL}
	_, err := m.Refresh(context.Background(), testApp(), "dead")
	if !errors.Is(err, ErrTokenEndpoint) {
		t.Fatalf("err = %v, want ErrTokenEndpoint", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("err = %v, want it to carry invalid_grant", err)
	}
}

// TestRequestToken_MissingRefreshToken fails loudly when offline_access wasn't
// granted (no refresh token to store).
func TestRequestToken_MissingRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"acc","expires_in":3600}`))
	}))
	defer srv.Close()

	m := &Minter{httpClient: srv.Client(), tokenURL: srv.URL}
	_, err := m.ExchangeCode(context.Background(), testApp(), "c", "r")
	if !errors.Is(err, ErrTokenEndpoint) {
		t.Fatalf("err = %v, want ErrTokenEndpoint", err)
	}
}

// TestAccessibleResources_ParsesSites pins the bearer-auth GET + parse.
func TestAccessibleResources_ParsesSites(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer acc1" {
			t.Errorf("auth header = %q, want Bearer acc1", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`[{"id":"cloud-1","url":"https://acme.atlassian.net","name":"Acme"}]`))
	}))
	defer srv.Close()

	m := &Minter{httpClient: srv.Client(), resourcesURL: srv.URL}
	got, err := m.AccessibleResources(context.Background(), "acc1")
	if err != nil {
		t.Fatalf("AccessibleResources: %v", err)
	}
	if len(got) != 1 || got[0].ID != "cloud-1" || got[0].URL != "https://acme.atlassian.net" {
		t.Fatalf("resources = %+v", got)
	}
}

// TestAuthorizeURL pins the consent-redirect shape: the required query params
// (audience, offline_access scope, prompt=consent, the state nonce).
func TestAuthorizeURL(t *testing.T) {
	raw := AuthorizeURL(testApp(), "https://tf.example/cb", "nonce-42")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Host != "auth.atlassian.com" || u.Path != "/authorize" {
		t.Errorf("endpoint = %s%s", u.Host, u.Path)
	}
	q := u.Query()
	if q.Get("audience") != "api.atlassian.com" {
		t.Errorf("audience = %q", q.Get("audience"))
	}
	if q.Get("client_id") != "client-123" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("response_type") != "code" || q.Get("prompt") != "consent" {
		t.Errorf("response_type/prompt = %q/%q", q.Get("response_type"), q.Get("prompt"))
	}
	if q.Get("state") != "nonce-42" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if !strings.Contains(q.Get("scope"), "offline_access") {
		t.Errorf("scope missing offline_access: %q", q.Get("scope"))
	}
}
