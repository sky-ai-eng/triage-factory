package server

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ws "github.com/coder/websocket"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// newLocalDeployServer is newTestServer configured the way local-mode boot
// configures it (buildServer): publicURL is the browser URL, the Vite dev
// origin is trusted, and foreign Hosts are refused.
func newLocalDeployServer(t *testing.T) *Server {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	s := newTestServer(t)
	var hmacKey [32]byte
	if _, err := rand.Read(hmacKey[:]); err != nil {
		t.Fatalf("generate hmac key: %v", err)
	}
	s.SetDeployConfig("http://localhost:3000", hmacKey)
	s.AllowDevFrontendOrigin()
	s.RejectForeignHosts()
	return s
}

// TestHandleWS_OriginGate pins that the websocket handshake runs the same
// origin predicate as the mutating routes. Browsers open websockets
// cross-origin without CORS, so without this any page the user visits could
// subscribe to a local instance's broadcasts, transcripts included.
func TestHandleWS_OriginGate(t *testing.T) {
	s := newLocalDeployServer(t)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/api/ws"

	tests := []struct {
		name   string
		header http.Header
		want   int // 0 = handshake succeeds
	}{
		{"no Origin (non-browser client)", http.Header{}, 0},
		{"publicURL", http.Header{"Origin": {"http://localhost:3000"}}, 0},
		{"dev frontend", http.Header{"Origin": {devFrontendOrigin}}, 0},
		{"foreign site", http.Header{"Origin": {"https://evil.example"}}, http.StatusForbidden},
		{"publicURL host on another port", http.Header{"Origin": {"http://localhost:4000"}}, http.StatusForbidden},
		// httptest listens on 127.0.0.1:<port>, so this is the address the
		// handshake is sent to, which publicURL does not name.
		{"the address dialed", http.Header{"Origin": {ts.URL}}, 0},
		{"Bearer credential skips the origin gate", http.Header{
			"Origin":        {"https://evil.example"},
			"Authorization": {"Bearer tf_anything"},
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, resp, err := ws.Dial(context.Background(), wsURL, &ws.DialOptions{HTTPHeader: tt.header})
			if tt.want == 0 {
				if err != nil {
					t.Fatalf("dial: %v", err)
				}
				_ = conn.Close(ws.StatusNormalClosure, "")
				return
			}
			if err == nil {
				_ = conn.Close(ws.StatusNormalClosure, "")
				t.Fatal("handshake succeeded, want it refused")
			}
			if resp == nil || resp.StatusCode != tt.want {
				t.Fatalf("handshake response = %v, want status %d", resp, tt.want)
			}
		})
	}
}

// TestForeignOrigin_LocalAddress: in local mode an Origin naming the address the
// request was sent to is not foreign, even when publicURL names another, but
// only for a Host the foreign-host check accepts, which a rebinding page fails.
func TestForeignOrigin_LocalAddress(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	newServer := func(publicURL string, local bool) *Server {
		s := &Server{}
		s.SetDeployConfig(publicURL, [32]byte{})
		if local {
			s.RejectForeignHosts()
		}
		return s
	}

	tests := []struct {
		name      string
		publicURL string
		local     bool
		host      string
		origin    string
		want      int
	}{
		{"LAN IP of a 0.0.0.0 bind", "http://0.0.0.0:3000", true, "192.168.1.20:3000", "http://192.168.1.20:3000", http.StatusOK},
		{"127.0.0.1 beside a localhost publicURL", "http://localhost:3000", true, "127.0.0.1:3000", "http://127.0.0.1:3000", http.StatusOK},
		{"IPv6 loopback", "http://localhost:3000", true, "[::1]:3000", "http://[::1]:3000", http.StatusOK},
		{"the --host name", "http://devbox.lan:3000", true, "devbox.lan:3000", "http://devbox.lan:3000", http.StatusOK},
		{"another local port", "http://localhost:3000", true, "127.0.0.1:3000", "http://127.0.0.1:8080", http.StatusForbidden},
		{"rebinding page", "http://localhost:3000", true, "attacker.example:3000", "http://attacker.example:3000", http.StatusForbidden},
		{"opaque origin", "http://localhost:3000", true, "localhost:3000", "null", http.StatusForbidden},
		{"multi mode trusts publicURL alone", "https://tf.example.com", false, "10.0.0.5:3000", "http://10.0.0.5:3000", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/anything", nil)
			req.Host = tt.host
			req.Header.Set("Origin", tt.origin)
			rec := httptest.NewRecorder()
			newServer(tt.publicURL, tt.local).withCSRFOriginCheck(next).ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// TestWithForeignHostCheck pins the DNS-rebinding defense: once enabled, only
// a Host naming this machine reaches the mux, across GET routes (which the
// CSRF origin check never covers) and the SPA alike.
func TestWithForeignHostCheck(t *testing.T) {
	s := newLocalDeployServer(t)
	h := s.Handler()

	tests := []struct {
		host string
		want int
	}{
		{"localhost:3000", http.StatusOK},
		{"LOCALHOST:3000", http.StatusOK},
		{"localhost", http.StatusOK},
		{"127.0.0.1:3000", http.StatusOK},
		{"[::1]:3000", http.StatusOK},
		{"192.168.1.20:3000", http.StatusOK},
		{"attacker.example:3000", http.StatusForbidden},
		{"localhost.attacker.example:3000", http.StatusForbidden},
		{"", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

// TestWithForeignHostCheck_PublicURLHost covers an instance whose browser URL
// names the machine by a DNS name (a --host override): that name is accepted
// alongside the loopback set, and nothing else is.
func TestWithForeignHostCheck_PublicURLHost(t *testing.T) {
	s := &Server{}
	s.SetDeployConfig("http://devbox.lan:3000", [32]byte{})
	s.RejectForeignHosts()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	for host, want := range map[string]int{
		"devbox.lan:3000":    http.StatusOK,
		"DevBox.LAN:3000":    http.StatusOK,
		"localhost:3000":     http.StatusOK,
		"otherbox.lan:3000":  http.StatusForbidden,
		"devbox.lan.evil:80": http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		s.withForeignHostCheck(next).ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: status = %d, want %d", host, rec.Code, want)
		}
	}
}

// TestWithForeignHostCheck_OffUnlessEnabled pins that the check is opt-in:
// multi-mode boot never calls RejectForeignHosts, because a reverse proxy may
// rewrite Host to an upstream name.
func TestWithForeignHostCheck_OffUnlessEnabled(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for name, s := range map[string]*Server{
		"no deploy config": {},
		"deploy config without the flag": func() *Server {
			s := &Server{}
			s.SetDeployConfig("https://tf.example.com", [32]byte{})
			return s
		}(),
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = "tf-control.internal:3000"
		rec := httptest.NewRecorder()
		s.withForeignHostCheck(next).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", name, rec.Code)
		}
	}
}

// TestRejectForeignHosts_NoopBeforeDeployConfig guards the call-order contract
// in its doc comment, as TestAllowDevFrontendOrigin_NoopBeforeDeployConfig
// does for its sibling.
func TestRejectForeignHosts_NoopBeforeDeployConfig(t *testing.T) {
	s := &Server{}
	s.RejectForeignHosts()
	if s.deployCfg != nil {
		t.Error("RejectForeignHosts created a deployCfg before SetDeployConfig ran")
	}
}
