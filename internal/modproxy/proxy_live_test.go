//go:build live

// Live tests in this file exercise the relay against the REAL public Go
// module proxy and checksum database, with the production vetted dialer.
//
// They exist because the bug this package fixes is invisible to a mocked
// test: proxy.golang.org serves small module zips inline but 302-redirects
// large ones to a Google Cloud Storage host, and only a fetch against the
// real service exercises that split. A relay that passes unit tests can
// still hand the redirect to the sandbox and fix nothing.
//
// Build-tagged so CI doesn't reach the network by default. No credentials
// and no API spend — just bandwidth (the toolchain case pulls ~70MB).
//
// To run: `go test -tags live ./internal/modproxy/ -v -run TestLive`

package modproxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestLiveRelay_ServesRedirectedArtifacts is the end-to-end proof.
//
// Each case below is a real artifact whose upstream behavior differs: the
// toolchain and aws-sdk-go exceed the inline-serving threshold and redirect
// to the CDN, while go-keyring is served inline. All three must arrive as
// 200 with a non-empty body through the relay — a 302 reaching the client
// means the redirect was passed through instead of resolved host-side,
// which is precisely the failure this package exists to prevent.
func TestLiveRelay_ServesRedirectedArtifacts(t *testing.T) {
	// Production config: default upstreams, default (vetted) dialer. The
	// listener is loopback but the DIAL targets are public, so the denylist
	// is satisfied — exactly the production posture.
	s, err := New(Config{RunID: "live"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	front := httptest.NewServer(s.Handler())
	defer front.Close()

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	for _, tc := range []struct {
		name    string
		path    string
		minSize int64
	}{
		{
			name:    "toolchain_redirects_to_cdn",
			path:    "/golang.org/toolchain/@v/v0.0.1-go1.26.5.linux-amd64.zip",
			minSize: 50 << 20,
		},
		{
			name:    "large_module_redirects_to_cdn",
			path:    "/github.com/aws/aws-sdk-go/@v/v1.55.5.zip",
			minSize: 20 << 20,
		},
		{
			name:    "small_module_served_inline",
			path:    "/github.com/zalando/go-keyring/@v/v0.2.8.zip",
			minSize: 1 << 10,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := client.Get(front.URL + tc.path)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (a 3xx means the CDN redirect reached the client)", resp.StatusCode)
			}
			n, err := io.Copy(io.Discard, resp.Body)
			if err != nil {
				t.Fatalf("stream body: %v", err)
			}
			if n < tc.minSize {
				t.Errorf("body = %d bytes, want >= %d", n, tc.minSize)
			}
			t.Logf("%s: %d bytes", tc.path, n)
		})
	}
}

// TestLiveSumDB_RelaysVerificationData proves the checksum path survives the
// indirection. The upstream module proxy answers 404 for /sumdb/ (that arm
// is for private proxies), so the relay must synthesize the capability probe
// and forward the rest to the checksum database itself — otherwise cmd/go
// falls back to contacting sum.golang.org directly, which the jail cannot
// reach, and every build fails at verification rather than at download.
func TestLiveSumDB_RelaysVerificationData(t *testing.T) {
	s, err := New(Config{RunID: "live"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	front := httptest.NewServer(s.Handler())
	defer front.Close()

	if resp, err := http.Get(front.URL + "/sumdb/sum.golang.org/supported"); err != nil {
		t.Fatalf("supported probe: %v", err)
	} else {
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("supported = %d, want 200; cmd/go would bypass the relay for checksums", resp.StatusCode)
		}
	}

	resp, err := http.Get(front.URL + "/sumdb/sum.golang.org/lookup/github.com/zalando/go-keyring@v0.2.8")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("lookup status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	// A real signed lookup names the module and carries the go.mod hash.
	if !contains(body, "github.com/zalando/go-keyring v0.2.8") {
		t.Errorf("lookup body does not name the module:\n%s", body)
	}
}

func contains(haystack []byte, needle string) bool {
	return len(haystack) > 0 && stringsContains(string(haystack), needle)
}

func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestLiveGoCommand_DownloadsThroughRelay drives the relay with the real
// cmd/go rather than a hand-built HTTP request.
//
// This is the case the unit tests cannot reach: it exercises the actual
// GOPROXY protocol sequence (@v/list, .info, .mod, .zip, and the /sumdb/
// lookups behind checksum verification) against the route table, with a
// clean module cache so nothing is served from disk. If the routing,
// path-escaping, or sumdb arm were wrong, `go mod download` fails here even
// though every direct fetch above succeeds.
func TestLiveGoCommand_DownloadsThroughRelay(t *testing.T) {
	s, err := New(Config{RunID: "live"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	addr, err := s.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Shutdown(context.Background()) }()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module livetest\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	cmd := exec.Command("go", "mod", "download", "-x", "github.com/zalando/go-keyring@v0.2.8")
	cmd.Dir = dir
	// A pristine cache and GOPROXY pointed ONLY at the relay: no ",direct"
	// fallback, so a routing bug cannot be masked by cmd/go quietly going
	// around us. GONOSUMDB/GOPRIVATE are cleared so checksum verification
	// stays on and actually exercises the /sumdb/ arm.
	cmd.Env = append(os.Environ(),
		"GOPROXY=http://"+addr,
		"GOMODCACHE="+filepath.Join(dir, "modcache"),
		"GOFLAGS=",
		"GOPRIVATE=",
		"GONOPROXY=",
		"GONOSUMDB=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod download through relay failed: %v\n%s", err, out)
	}
	t.Logf("go mod download succeeded through relay at %s", addr)
}
