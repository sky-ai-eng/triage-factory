package ghchannel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func okConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		ConversationID: "conv-1",
		Upstream:       "https://api.github.com",
		BinDir:         t.TempDir(),
		TokenSource:    func(context.Context) (string, error) { return "ghs_real", nil },
	}
}

// Start validates before it allocates: a caller missing any of the three
// required inputs gets an error, not a half-built channel.
func TestStart_RequiresInputs(t *testing.T) {
	paths.SetForTest(t, t.TempDir())

	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"no conversation id", func(c *Config) { c.ConversationID = "" }, "ConversationID"},
		{"no bin dir", func(c *Config) { c.BinDir = "" }, "BinDir"},
		{"no token source", func(c *Config) { c.TokenSource = nil }, "TokenSource"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := okConfig(t)
			tc.mutate(&cfg)
			ch, err := Start(cfg)
			if err == nil {
				_ = ch.Close()
				t.Fatalf("Start succeeded with %s missing, want an error", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// TestStart_UnresolvableStateRootErrors is the guard for the panic seam. The
// path resolvers panic when local mode can't resolve $HOME, and this is an
// error-returning constructor — so it pre-flights the state root and reports a
// missing home rather than taking down the process that called it. A panic here
// would kill the whole server, not just the run's gh channel.
func TestStart_UnresolvableStateRootErrors(t *testing.T) {
	if runtime.GOOS == "windows" || runtime.GOOS == "plan9" {
		t.Skip("home resolution on this platform doesn't key off $HOME")
	}
	if runmode.Current() != runmode.ModeLocal {
		t.Skip("state-root home resolution is local-mode only")
	}
	// No paths.SetForTest here on purpose: the override would short-circuit
	// the very resolution under test.
	t.Setenv("HOME", "")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Start panicked instead of returning an error: %v", r)
		}
	}()
	ch, err := Start(okConfig(t))
	if err == nil {
		_ = ch.Close()
		t.Fatal("Start succeeded with an unresolvable state root, want an error")
	}
	if !strings.Contains(err.Error(), "state root") {
		t.Errorf("error = %v, want it to name the state root", err)
	}
}

// The trust file is the run's own, mode 0600, and carries more than the leaf:
// SSL_CERT_FILE is process-global in the agent subprocess, so a leaf-only
// bundle would narrow every other TLS client it runs.
func TestStart_TrustFileIsPrivateAndComplete(t *testing.T) {
	paths.SetForTest(t, t.TempDir())

	ch, err := Start(okConfig(t))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	info, err := os.Stat(ch.CertPath)
	if err != nil {
		t.Fatalf("stat trust file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("trust file mode = %v, want 0600", perm)
	}
	body, err := os.ReadFile(ch.CertPath)
	if err != nil {
		t.Fatalf("read trust file: %v", err)
	}
	if n := strings.Count(string(body), "BEGIN CERTIFICATE"); n < 2 {
		t.Errorf("trust file carries %d certificates, want the system roots alongside the run's leaf", n)
	}

	// The config dir sits under the channel directory, so Close reclaims it
	// with everything else.
	if !strings.HasPrefix(ch.ConfigDir, paths.GHChannelDir("conv-1")+string(filepath.Separator)) {
		t.Errorf("ConfigDir = %q, want it under the channel directory", ch.ConfigDir)
	}
}

// Each run gets its own placeholder, so one run cannot spend another's
// credential by presenting a token it happens to hold.
func TestStart_PlaceholderIsPerRun(t *testing.T) {
	paths.SetForTest(t, t.TempDir())

	first, err := Start(okConfig(t))
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })

	second := okConfig(t)
	second.ConversationID = "conv-2"
	other, err := Start(second)
	if err != nil {
		t.Fatalf("Start second: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })

	if first.Token == "" || first.Token == other.Token {
		t.Errorf("placeholders = %q / %q, want distinct non-empty per-run values", first.Token, other.Token)
	}
	if first.Host == other.Host {
		t.Errorf("both runs bound %q, want separate listeners", first.Host)
	}
}

// A nil channel is safe to Close — callers defer it before knowing whether
// Start succeeded.
func TestClose_NilSafe(t *testing.T) {
	var ch *Channel
	if err := ch.Close(); err != nil {
		t.Errorf("(*Channel)(nil).Close() = %v, want nil", err)
	}
}

// Observe is optional: a caller that doesn't record artifacts must still get a
// working channel rather than a nil-func panic on the first mutation.
func TestStart_ObserveOptional(t *testing.T) {
	paths.SetForTest(t, t.TempDir())

	cfg := okConfig(t)
	cfg.Observe = nil
	ch, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	if ch.Host == "" {
		t.Error("channel bound no listener")
	}
}

// TestStart_ObserveWriteReachesInjector pins the local half of the write audit:
// a mutating REST call through the channel reaches the caller's ObserveWrite
// hook with the upstream's outcome. Local mode has no relay hop, so this
// callback IS the audit path — an unwired one loses every gh-channel write on
// the local surface while multi keeps recording them.
func TestStart_ObserveWriteReachesInjector(t *testing.T) {
	paths.SetForTest(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	writes := make(chan ghinjector.ObservedWrite, 2)
	cfg := okConfig(t)
	cfg.Upstream = upstream.URL
	cfg.ObserveWrite = func(_ context.Context, w ghinjector.ObservedWrite) { writes <- w }
	ch, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	pem, err := os.ReadFile(ch.CertPath)
	if err != nil {
		t.Fatalf("read trust file: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("trust file carried no usable cert")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	req, _ := http.NewRequest(http.MethodPatch, "https://"+ch.Host+"/api/v3/repos/octo/repo/pulls/7", nil)
	req.Header.Set("Authorization", "token "+ch.Token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request through channel: %v", err)
	}
	_ = resp.Body.Close()

	select {
	case w := <-writes:
		if w.Method != http.MethodPatch || w.Status != http.StatusNotFound ||
			!strings.Contains(w.Path, "/repos/octo/repo/pulls/7") {
			t.Errorf("observed write = %+v, want the PATCH, its path, and the 404", w)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a gh-channel write never reached ObserveWrite")
	}
}
