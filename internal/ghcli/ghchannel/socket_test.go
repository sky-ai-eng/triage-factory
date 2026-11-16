package ghchannel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghcli/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// shortStateRoot is a state root whose channel socket path fits the
// platform's limit whatever the test is called: t.TempDir names its directory
// after the test, and macOS's temporary directory is long before that.
func shortStateRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tfgh")
	if err != nil {
		t.Fatalf("make state root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// configuredSocket is the socket the channel's gh config names, "" when it
// names none. It reads the line the way gh's YAML reader would for any path
// strconv.Quote spells.
func configuredSocket(t *testing.T, ch *Channel) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(ch.ConfigDir, "config.yml"))
	if errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatalf("read gh config: %v", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		value, ok := strings.CutPrefix(line, "http_unix_socket: ")
		if !ok {
			continue
		}
		path, err := strconv.Unquote(value)
		if err != nil {
			t.Fatalf("http_unix_socket value %q is not a quoted string: %v", value, err)
		}
		return path
	}
	return ""
}

// socketClient sends every request down the socket the channel's gh config
// names, in plain HTTP whatever the URL says, as gh does.
func socketClient(t *testing.T, ch *Channel) *http.Client {
	t.Helper()
	path := configuredSocket(t, ch)
	if path == "" {
		t.Fatalf("the channel's gh config names no socket (Host %q); a state root this short must fit one", ch.Host)
	}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	}
	// The plain connection stands in for the TLS one too, so an https URL is
	// sent down the socket unencrypted, as gh sends it.
	return &http.Client{Transport: &http.Transport{DialContext: dial, DialTLSContext: dial}}
}

// tlsClient trusts the channel's TLS listener the way git does, through the
// trust file.
func tlsClient(t *testing.T, ch *Channel) *http.Client {
	t.Helper()
	pem, err := os.ReadFile(ch.CertPath)
	if err != nil {
		t.Fatalf("read trust file: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		t.Fatal("trust file carried no usable cert")
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

// recordingUpstream answers the gate's repository lookups and records the
// path of every other request it is sent, with its Authorization.
type recordingUpstream struct {
	srv *httptest.Server

	mu   sync.Mutex
	seen []string
	auth []string
}

func newRecordingUpstream(t *testing.T) *recordingUpstream {
	t.Helper()
	u := &recordingUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/octo/repo":
			_, _ = w.Write([]byte(`{"id": 1, "full_name": "octo/repo", "private": true}`))
			return
		case "/repos/acme/payments":
			_, _ = w.Write([]byte(`{"id": 2, "full_name": "acme/payments", "private": true}`))
			return
		}
		u.mu.Lock()
		u.seen = append(u.seen, r.URL.Path)
		u.auth = append(u.auth, r.Header.Get("Authorization"))
		u.mu.Unlock()
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *recordingUpstream) requests() (paths, auth []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.seen), slices.Clone(u.auth)
}

func get(t *testing.T, client *http.Client, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// A channel whose socket fits serves gh on it: GH_HOST is the TLS listener's
// address without its port, which is what gh matches a rewritten remote
// against, the gh config names the socket, only this account may connect to
// it, and a request down it reaches GitHub with the real credential.
func TestStart_ServesGHOnThePrivateSocket(t *testing.T) {
	paths.SetForTest(t, shortStateRoot(t))
	upstream := newRecordingUpstream(t)
	cfg := okConfig(t)
	cfg.Upstream = upstream.srv.URL
	ch, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	if ch.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want the portless 127.0.0.1", ch.Host)
	}
	if host, port, err := net.SplitHostPort(ch.GitHost); err != nil || host != "127.0.0.1" || port == "" {
		t.Errorf("GitHost = %q, want the TLS listener's 127.0.0.1:<port>", ch.GitHost)
	}

	socket := configuredSocket(t, ch)
	if want := filepath.Join(paths.GHChannelDir("conv-1"), "gh.sock"); socket != want {
		t.Errorf("gh config names socket %q, want %q", socket, want)
	}
	info, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	// Windows keeps no POSIX permission bits on a socket file; the per-run
	// directory's ACL is what restricts it there.
	if info.Mode()&fs.ModeSocket == 0 || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Errorf("socket mode = %v, want a socket with mode 0600", info.Mode())
	}

	if status := get(t, socketClient(t, ch), "https://127.0.0.1/api/v3/repos/octo/repo/pulls/7/files", ch.Token); status != http.StatusOK {
		t.Fatalf("status down the socket = %d, want 200", status)
	}
	seen, auth := upstream.requests()
	if !slices.Equal(seen, []string{"/repos/octo/repo/pulls/7/files"}) || !slices.Equal(auth, []string{"token ghs_real"}) {
		t.Errorf("upstream saw %v with %v, want the request with the real credential", seen, auth)
	}
}

// A process that dies mid-run leaves its channel directory behind, socket file
// and gh config included. The next channel for the conversation still serves
// gh on its socket rather than failing the bind and leaving gh pointed at a
// socket nothing listens on.
func TestStart_ReplacesADeadChannelsDirectory(t *testing.T) {
	paths.SetForTest(t, shortStateRoot(t))
	dir := paths.GHChannelDir("conv-1")
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o700); err != nil {
		t.Fatalf("make leftover dir: %v", err)
	}
	socket := filepath.Join(dir, "gh.sock")
	dead, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("bind leftover socket: %v", err)
	}
	// A killed process never unlinks its socket file.
	dead.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = dead.Close()
	if err := os.WriteFile(filepath.Join(dir, "config", "config.yml"),
		[]byte("http_unix_socket: "+strconv.Quote(socket)+"\n"), 0o600); err != nil {
		t.Fatalf("write leftover gh config: %v", err)
	}

	upstream := newRecordingUpstream(t)
	cfg := okConfig(t)
	cfg.Upstream = upstream.srv.URL
	ch, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	if ch.Host != "127.0.0.1" {
		t.Errorf("Host = %q, want the portless 127.0.0.1", ch.Host)
	}
	if status := get(t, socketClient(t, ch), "https://127.0.0.1/api/v3/repos/octo/repo/pulls/7/files", ch.Token); status != http.StatusOK {
		t.Errorf("status down the configured socket = %d, want 200", status)
	}
}

// The socket is the same front door as the TLS listener: a request without
// the run's placeholder is refused on both, and neither forwards it.
func TestStart_SocketRefusesARequestWithoutThePlaceholder(t *testing.T) {
	paths.SetForTest(t, shortStateRoot(t))
	upstream := newRecordingUpstream(t)
	cfg := okConfig(t)
	cfg.Upstream = upstream.srv.URL
	ch, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	transports := map[string]struct {
		client *http.Client
		url    string
	}{
		"socket": {socketClient(t, ch), "https://127.0.0.1/api/v3/repos/octo/repo/pulls/7"},
		"tls":    {tlsClient(t, ch), "https://" + ch.GitHost + "/api/v3/repos/octo/repo/pulls/7"},
	}
	for name, tr := range transports {
		for _, token := range []string{"", "another-runs-placeholder"} {
			if status := get(t, tr.client, tr.url, token); status != http.StatusUnauthorized {
				t.Errorf("%s with token %q: status %d, want 401", name, token, status)
			}
		}
	}
	if seen, _ := upstream.requests(); len(seen) != 0 {
		t.Errorf("upstream was forwarded %v, want nothing", seen)
	}
}

// The repository gate stands behind the socket: a request naming a repository
// outside the run is refused there and its denial recorded, and nothing of it
// reaches GitHub but the gate's own lookup of the name.
func TestStart_SocketRequestOutsideTheRunIsRefusedAndRecorded(t *testing.T) {
	paths.SetForTest(t, shortStateRoot(t))
	upstream := newRecordingUpstream(t)
	denials := make(chan ghinjector.DeniedRepository, 2)
	cfg := okConfig(t)
	cfg.Upstream = upstream.srv.URL
	cfg.Repositories = func(context.Context) ([]ghinjector.AuthorizedRepository, bool, error) {
		return []ghinjector.AuthorizedRepository{{Name: "octo/repo", ID: "1"}}, true, nil
	}
	cfg.DenyRepository = func(_ context.Context, d ghinjector.DeniedRepository) { denials <- d }
	ch, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	if status := get(t, socketClient(t, ch), "https://127.0.0.1/api/v3/repos/acme/payments/pulls/12", ch.Token); status != http.StatusForbidden {
		t.Fatalf("status down the socket = %d, want the gate's 403", status)
	}
	select {
	case d := <-denials:
		if d.Name() != "acme/payments" || d.Method != http.MethodGet {
			t.Errorf("recorded denial = %+v, want the GET of acme/payments", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the refusal was never recorded")
	}
	if seen, _ := upstream.requests(); len(seen) != 0 {
		t.Errorf("upstream was forwarded %v, want nothing", seen)
	}
}

// A socket path the platform cannot hold leaves the channel as it is without
// one: GH_HOST is the TLS listener's ported address, which still works, and
// the gh config sends nothing to a socket.
func TestStart_SocketPathOverTheLimitKeepsThePortedHost(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("d", socketPathLimit))
	paths.SetForTest(t, root)
	upstream := newRecordingUpstream(t)
	cfg := okConfig(t)
	cfg.Upstream = upstream.srv.URL
	ch, err := Start(cfg)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = ch.Close() })

	if ch.Host != ch.GitHost {
		t.Errorf("Host = %q, want the TLS listener's %q", ch.Host, ch.GitHost)
	}
	if _, port, err := net.SplitHostPort(ch.Host); err != nil || port == "" {
		t.Errorf("Host = %q, want it to carry the listener's port", ch.Host)
	}
	body, err := os.ReadFile(filepath.Join(ch.ConfigDir, "config.yml"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read gh config: %v", err)
	}
	if strings.Contains(string(body), "http_unix_socket") {
		t.Errorf("gh config = %q, want no http_unix_socket line", body)
	}
	if _, err := os.Lstat(filepath.Join(paths.GHChannelDir("conv-1"), "gh.sock")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat socket = %v, want none bound", err)
	}

	if status := get(t, tlsClient(t, ch), "https://"+ch.Host+"/api/v3/repos/octo/repo/pulls/7/files", ch.Token); status != http.StatusOK {
		t.Errorf("status over TLS = %d, want 200", status)
	}
}
