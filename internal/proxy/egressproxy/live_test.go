//go:build live

// Live tests in this file run real package managers on this host against the
// real registries through the interceptor, with the production resolver,
// vetted dial and upstream verification: the stand-in registries the unit
// tests use cannot show that npm's audit, pip's index requests or a real
// registry's response framing pass through untouched.
//
// Build-tagged so CI doesn't reach the network by default. No credentials and
// no spend: the publish case carries a made-up token, and the interceptor
// refuses the request before it leaves this process. A client the host
// doesn't have is skipped.
//
// To run: `go test -tags live ./internal/proxy/egressproxy/ -v -run TestLive`
//
// internal/agentproc's TestIntegration_EgressIntercept runs the same fetches
// inside a jail.

package egressproxy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/cabundle"
)

// liveProxy is the production proxy on loopback, with the env a sandbox
// client gets: the proxy URL with its token, and the trust variables pointing
// at a bundle of the system roots and the engagement's CA.
type liveProxy struct {
	srv *Server
	env []string

	mu      sync.Mutex
	denials []DeniedConnect
}

func newLiveProxy(t *testing.T) *liveProxy {
	t.Helper()
	lp := &liveProxy{}
	token := "live-token"
	s, err := New(Config{
		Registries:    DefaultRegistries(),
		IncomingToken: token,
		RecordDenial: func(_ context.Context, d DeniedConnect) {
			lp.mu.Lock()
			lp.denials = append(lp.denials, d)
			lp.mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	})
	bundle := filepath.Join(t.TempDir(), "ca-bundle.crt")
	if err := os.WriteFile(bundle, cabundle.WithSystemRoots(s.CACertPEM()), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://" + BasicUser + ":" + token + "@" + addr
	home := t.TempDir()
	lp.srv = s
	lp.env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"npm_config_cache=" + filepath.Join(home, ".npm"),
		"npm_config_update_notifier=false",
		"HTTPS_PROXY=" + proxyURL, "https_proxy=" + proxyURL,
		"HTTP_PROXY=" + proxyURL, "http_proxy=" + proxyURL,
		"NO_PROXY=", "no_proxy=",
		"SSL_CERT_FILE=" + bundle,
		"NODE_EXTRA_CA_CERTS=" + bundle,
		"PIP_CERT=" + bundle,
	}
	return lp
}

func (lp *liveProxy) run(t *testing.T, dir string, name string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = lp.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		lp.mu.Lock()
		for _, d := range lp.denials {
			t.Logf("denial: %s %s", d.Target, d.Reason)
		}
		lp.mu.Unlock()
	}
	return string(out), err
}

func (lp *liveProxy) denialsMatching(prefix string) []DeniedConnect {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	var out []DeniedConnect
	for _, d := range lp.denials {
		if strings.HasPrefix(d.Reason, prefix) {
			out = append(out, d)
		}
	}
	return out
}

func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not on PATH", name)
	}
}

// TestLive_NpmInstallWithAudit pins the read side: an install, with the audit
// npm runs by default, completes through the interceptor.
func TestLive_NpmInstallWithAudit(t *testing.T) {
	requireTool(t, "npm")
	lp := newLiveProxy(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"probe","version":"0.0.0","private":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := lp.run(t, dir, "npm", "install", "--no-fund", "left-pad@1.3.0")
	t.Logf("npm install:\n%s", out)
	if err != nil {
		t.Fatalf("npm install through the interceptor failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "left-pad", "package.json")); err != nil {
		t.Errorf("left-pad not installed: %v", err)
	}
	if !strings.Contains(out, "audited") && !strings.Contains(out, "vulnerabilit") {
		t.Error("npm printed no audit result; the audit request did not complete")
	}
	if d := lp.denialsMatching("POST "); len(d) != 0 {
		t.Errorf("the audit POST was refused: %v", d)
	}
}

// TestLive_NpmPublishRefused pins the write side: `npm publish` is refused at
// the interceptor with a reason naming the rule, and recorded.
func TestLive_NpmPublishRefused(t *testing.T) {
	requireTool(t, "npm")
	lp := newLiveProxy(t)
	dir := t.TempDir()
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	pkg := `{"name":"tf-intercept-publish-probe-` + hex.EncodeToString(suffix) + `","version":"0.0.1"}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".npmrc"), []byte("//registry.npmjs.org/:_authToken=npm_NOTAREALTOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := lp.run(t, dir, "npm", "publish")
	t.Logf("npm publish:\n%s", out)
	if err == nil {
		t.Fatal("npm publish succeeded through the interceptor")
	}
	if !strings.Contains(out, "read-only") {
		t.Error("npm's error does not carry the interceptor's reason")
	}
	if len(lp.denialsMatching("PUT ")) == 0 {
		t.Error("no denial recorded for the publish PUT")
	}
}

// TestLive_PipInstallInVenv pins pip the way the rootfs ships it: a venv's
// pip, installing from PyPI through the interceptor.
func TestLive_PipInstallInVenv(t *testing.T) {
	requireTool(t, "python3")
	lp := newLiveProxy(t)
	dir := t.TempDir()
	venv := filepath.Join(dir, "venv")
	if out, err := lp.run(t, dir, "python3", "-m", "venv", venv); err != nil {
		t.Skipf("python3 -m venv unavailable here: %v\n%s", err, out)
	}
	out, err := lp.run(t, dir, filepath.Join(venv, "bin", "pip"), "install", "--no-cache-dir", "--disable-pip-version-check", "six==1.16.0")
	t.Logf("pip install:\n%s", out)
	if err != nil {
		t.Fatalf("pip install through the interceptor failed: %v", err)
	}
}

// TestLive_CurlThroughInterceptor pins curl, which reads SSL_CERT_FILE.
func TestLive_CurlThroughInterceptor(t *testing.T) {
	requireTool(t, "curl")
	lp := newLiveProxy(t)
	out, err := lp.run(t, t.TempDir(), "curl", "-fsS", "-o", "/dev/null", "-w", "%{http_code}", "https://pypi.org/simple/six/")
	if err != nil || strings.TrimSpace(out) != "200" {
		t.Fatalf("curl through the interceptor: %v, output %q", err, out)
	}
}

// TestLive_PnpmAdd pins pnpm, whose undici client reads NODE_EXTRA_CA_CERTS.
// It installs pnpm the way the rootfs does, an npm install of a pinned
// release (internal/sandbox pnpmVersion), through the interceptor too, rather
// than trusting whatever pnpm shim the host has.
func TestLive_PnpmAdd(t *testing.T) {
	requireTool(t, "npm")
	lp := newLiveProxy(t)
	cli := t.TempDir()
	if out, err := lp.run(t, cli, "npm", "install", "--no-fund", "--prefix", cli, "pnpm@11.9.0"); err != nil {
		t.Fatalf("install pnpm through the interceptor: %v\n%s", err, out)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"probe","version":"0.0.0","private":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := lp.run(t, dir, filepath.Join(cli, "node_modules", ".bin", "pnpm"), "add", "left-pad@1.3.0")
	t.Logf("pnpm add:\n%s", out)
	if err != nil {
		t.Fatalf("pnpm add through the interceptor failed: %v", err)
	}
}

// TestLive_GoClient pins a Go program's net/http, which reads SSL_CERT_FILE
// and HTTPS_PROXY. It runs as its own process because a test binary's system
// roots are already loaded.
func TestLive_GoClient(t *testing.T) {
	requireTool(t, "go")
	lp := newLiveProxy(t)
	dir := t.TempDir()
	const prog = `package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	resp, err := http.Get("https://registry.npmjs.org/left-pad")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Println(resp.StatusCode)
}
`
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(prog), 0o600); err != nil {
		t.Fatal(err)
	}
	lp.env = append(lp.env, "GOTOOLCHAIN=local", "GOCACHE="+filepath.Join(dir, "gocache"), "GOPATH="+filepath.Join(dir, "gopath"))
	out, err := lp.run(t, dir, "go", "run", "main.go")
	if err != nil || strings.TrimSpace(out) != "200" {
		t.Fatalf("Go client through the interceptor: %v, output %q", err, out)
	}
}
