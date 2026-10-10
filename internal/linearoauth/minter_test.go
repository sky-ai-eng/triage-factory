package linearoauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/linear"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

func testApp() linear.OAuthApp {
	return linear.OAuthApp{ClientID: "client-123", ClientSecret: "secret-xyz"}
}

// formServer answers every POST with status and body, recording the form.
func formServer(t *testing.T, status int, body string) (*httptest.Server, *url.Values) {
	t.Helper()
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		_ = r.ParseForm()
		got = r.PostForm
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestInstallAuthorizeURL(t *testing.T) {
	u, err := url.Parse(InstallAuthorizeURL(testApp(), "https://tf.example/api/linear/install/callback", "nonce-1"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Scheme+"://"+u.Host+u.Path != authorizeEndpoint {
		t.Errorf("endpoint = %s, want %s", u.Scheme+"://"+u.Host+u.Path, authorizeEndpoint)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":     "client-123",
		"redirect_uri":  "https://tf.example/api/linear/install/callback",
		"response_type": "code",
		"scope":         "read,write,issues:create,comments:create",
		"actor":         "app",
		"state":         "nonce-1",
		"prompt":        "consent",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if q.Has("client_secret") {
		t.Error("the authorize URL carries the client secret")
	}
}

func TestExchangeCode_ParsesTokenPair(t *testing.T) {
	now := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)
	srv, form := formServer(t, http.StatusOK,
		`{"access_token":"acc1","refresh_token":"ref1","expires_in":86399,"token_type":"Bearer","scope":"read write"}`)
	m := &Minter{httpClient: srv.Client(), tokenURL: srv.URL, now: func() time.Time { return now }}

	tok, err := m.ExchangeCode(context.Background(), testApp(), "the-code", "https://tf.example/cb")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if tok.AccessToken != "acc1" || tok.RefreshToken != "ref1" {
		t.Errorf("token = %+v, want acc1/ref1", tok)
	}
	if want := now.Add(86399 * time.Second); !tok.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v, want %v", tok.ExpiresAt, want)
	}
	for k, want := range map[string]string{
		"grant_type": "authorization_code", "code": "the-code", "redirect_uri": "https://tf.example/cb",
		"client_id": "client-123", "client_secret": "secret-xyz",
	} {
		if got := form.Get(k); got != want {
			t.Errorf("form %s = %q, want %q", k, got, want)
		}
	}
}

func TestRefresh_SendsTheRefreshGrant(t *testing.T) {
	srv, form := formServer(t, http.StatusOK, `{"access_token":"acc2","refresh_token":"ref2","expires_in":86399}`)
	m := &Minter{httpClient: srv.Client(), tokenURL: srv.URL}

	tok, err := m.Refresh(context.Background(), testApp(), "ref1")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if tok.RefreshToken != "ref2" {
		t.Errorf("RefreshToken = %q, want the rotated ref2", tok.RefreshToken)
	}
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "ref1" {
		t.Errorf("form = %v, want the refresh_token grant for ref1", *form)
	}
}

// TestRefresh_InvalidGrantIsRefused pins the revoked-install signal: a 400
// invalid_grant is a refusal (ErrTokenEndpoint) that RefusedGrant names.
func TestRefresh_InvalidGrantIsRefused(t *testing.T) {
	srv, _ := formServer(t, http.StatusBadRequest, `{"error":"invalid_grant","error_description":"revoked"}`)
	m := &Minter{httpClient: srv.Client(), tokenURL: srv.URL}

	_, err := m.Refresh(context.Background(), testApp(), "ref1")
	if !RefusedGrant(err) {
		t.Errorf("RefusedGrant(%v) = false, want true", err)
	}
	if !errors.Is(err, ErrTokenEndpoint) {
		t.Errorf("err = %v, want ErrTokenEndpoint", err)
	}
}

// TestRefresh_UnavailableIsNotARefusal pins that a 5xx says nothing about the
// token: it is neither a refused grant nor a token-endpoint refusal, so the
// cache never removes an install over an outage.
func TestRefresh_UnavailableIsNotARefusal(t *testing.T) {
	srv, _ := formServer(t, http.StatusBadGateway, `upstream down`)
	m := &Minter{httpClient: srv.Client(), tokenURL: srv.URL}

	_, err := m.Refresh(context.Background(), testApp(), "ref1")
	if err == nil || RefusedGrant(err) || errors.Is(err, ErrTokenEndpoint) {
		t.Errorf("err = %v, want a non-refusal", err)
	}
	if class, ok := upstream.ClassOf(err); !ok || class != upstream.Transient {
		t.Errorf("class = %v (%v), want transient", class, ok)
	}
}

func TestExchangeCode_MissingRefreshTokenFails(t *testing.T) {
	srv, _ := formServer(t, http.StatusOK, `{"access_token":"acc1","expires_in":86399}`)
	m := &Minter{httpClient: srv.Client(), tokenURL: srv.URL}

	if _, err := m.ExchangeCode(context.Background(), testApp(), "c", "https://tf.example/cb"); !errors.Is(err, ErrTokenEndpoint) {
		t.Errorf("err = %v, want ErrTokenEndpoint for a pair with no refresh token", err)
	}
}

func TestRevoke(t *testing.T) {
	srv, form := formServer(t, http.StatusOK, ``)
	m := &Minter{httpClient: srv.Client(), revokeURL: srv.URL}

	if err := m.Revoke(context.Background(), testApp(), "ref1", HintRefreshToken); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	for k, want := range map[string]string{
		"token": "ref1", "token_type_hint": "refresh_token", "client_id": "client-123", "client_secret": "secret-xyz",
	} {
		if got := form.Get(k); got != want {
			t.Errorf("form %s = %q, want %q", k, got, want)
		}
	}

	failing, _ := formServer(t, http.StatusBadRequest, `{"error":"invalid_request"}`)
	m = &Minter{httpClient: failing.Client(), revokeURL: failing.URL}
	if err := m.Revoke(context.Background(), testApp(), "ref1", HintRefreshToken); err == nil {
		t.Error("Revoke answered 400 reported success")
	}
}
