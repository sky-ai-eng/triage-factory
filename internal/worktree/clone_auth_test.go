package worktree

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestCloneAuthFor_HTTPSOnly pins the constructor's gate: a token is attached
// only for an https:// URL, so SSH remotes (local-mode default) and the
// no-token path inject nothing. SKY-391.
func TestCloneAuthFor_HTTPSOnly(t *testing.T) {
	cases := []struct {
		name     string
		cloneURL string
		token    string
		wantOn   bool
		wantPref string
	}{
		{"https github.com", "https://github.com/acme/repo.git", "ghs_x", true, "https://github.com/"},
		{"https GHES with port", "https://github.example.com:8443/acme/repo.git", "ghs_x", true, "https://github.example.com:8443/"},
		{"ssh scp form", "git@github.com:acme/repo.git", "ghs_x", false, ""},
		{"ssh url form", "ssh://git@github.com/acme/repo.git", "ghs_x", false, ""},
		{"http (not https)", "http://github.com/acme/repo.git", "ghs_x", false, ""},
		{"empty token", "https://github.com/acme/repo.git", "", false, ""},
		{"empty url", "", "ghs_x", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := CloneAuthFor(tc.cloneURL, tc.token)
			if a.active() != tc.wantOn {
				t.Fatalf("active()=%v, want %v (auth=%+v)", a.active(), tc.wantOn, a)
			}
			if a.urlPrefix != tc.wantPref {
				t.Errorf("urlPrefix=%q, want %q", a.urlPrefix, tc.wantPref)
			}
		})
	}
}

// TestCloneAuth_ExtraEnv pins the exact GIT_CONFIG_* env the injection emits:
// a host-scoped extraHeader carrying the base64 of "x-access-token:<token>",
// matching the gitproxy's Basic-auth encoding. An inert auth emits nothing.
func TestCloneAuth_ExtraEnv(t *testing.T) {
	if env := (CloneAuth{}).extraEnv(); env != nil {
		t.Errorf("zero CloneAuth.extraEnv() = %v, want nil", env)
	}

	a := CloneAuthFor("https://github.com/acme/repo.git", "ghs_tok")
	env := a.extraEnv()
	want := map[string]string{
		"GIT_CONFIG_COUNT":   "1",
		"GIT_CONFIG_KEY_0":   "http.https://github.com/.extraHeader",
		"GIT_CONFIG_VALUE_0": "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_tok")),
	}
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
}

// startRefsCaptureServer stands in for a private GitHub host: it records the
// Authorization header and path of the first smart-HTTP ref-discovery request
// (GET <repo>/info/refs?service=git-upload-pack), then 404s so git stops
// immediately. TLS because CloneAuthFor only injects for https:// URLs; the
// caller sets GIT_SSL_NO_VERIFY so git accepts the self-signed cert.
func startRefsCaptureServer(t *testing.T) (url string, authHeader func() string) {
	t.Helper()
	var mu sync.Mutex
	var gotAuth, gotPath string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if strings.HasSuffix(r.URL.Path, "/info/refs") && gotPath == "" {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
		}
		mu.Unlock()
		http.Error(w, "no repo", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() string {
		mu.Lock()
		defer mu.Unlock()
		if gotPath == "" {
			t.Fatal("git never issued the info/refs handshake")
		}
		return gotAuth
	}
}

// TestCloneAuth_InjectsHeaderOnClone is the end-to-end proof that a populated
// CloneAuth reaches the wire: the real `git clone` EnsureBareClone shells out
// carries the host-scoped Authorization header on the info/refs request. The
// clone itself fails (the server serves no repo) — we assert only the header,
// which is what authenticates a private clone. SKY-391.
func TestCloneAuth_InjectsHeaderOnClone(t *testing.T) {
	withTestHome(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true") // accept the httptest self-signed cert

	base, authHeader := startRefsCaptureServer(t)
	cloneURL := base + "/acme/repo.git"

	_, _ = EnsureBareClone(context.Background(), "acme", "repo-auth-inject", cloneURL,
		WithCloneAuth(CloneAuthFor(cloneURL, "ghs_clone_token")))

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:ghs_clone_token"))
	if got := authHeader(); got != want {
		t.Errorf("info/refs Authorization = %q, want %q", got, want)
	}
}

// TestCloneAuth_NoHeaderWithoutAuth is the negative control: without a
// CloneAuth option, the clone sends no Authorization header (the pre-SKY-391
// behavior, and the local-mode / public path).
func TestCloneAuth_NoHeaderWithoutAuth(t *testing.T) {
	withTestHome(t)
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	base, authHeader := startRefsCaptureServer(t)
	cloneURL := base + "/acme/repo.git"

	_, _ = EnsureBareClone(context.Background(), "acme", "repo-noauth", cloneURL)

	if got := authHeader(); got != "" {
		t.Errorf("info/refs Authorization = %q, want empty (no auth injected)", got)
	}
}
