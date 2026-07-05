package server

import (
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWithCSRFOriginCheck_DevFrontendOrigin pins the scope of
// AllowDevFrontendOrigin: it must trust exactly devFrontendOrigin, only
// once explicitly set, and must never let an actual cross-site Origin
// slip through alongside it.
func TestWithCSRFOriginCheck_DevFrontendOrigin(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	newServer := func(t *testing.T, trustDev bool) *Server {
		t.Helper()
		s := &Server{}
		var hmacKey [32]byte
		if _, err := rand.Read(hmacKey[:]); err != nil {
			t.Fatalf("generate hmac key: %v", err)
		}
		s.SetDeployConfig("http://localhost:3000", hmacKey)
		if trustDev {
			s.AllowDevFrontendOrigin()
		}
		return s
	}

	tests := []struct {
		name     string
		trustDev bool
		origin   string
		want     int
	}{
		{"publicURL always trusted", false, "http://localhost:3000", http.StatusOK},
		{"dev origin rejected when not allowed", false, devFrontendOrigin, http.StatusForbidden},
		{"dev origin trusted when allowed", true, devFrontendOrigin, http.StatusOK},
		{"publicURL still trusted when dev allowed", true, "http://localhost:3000", http.StatusOK},
		{"real attacker still rejected when dev allowed", true, "https://evil.example", http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newServer(t, tt.trustDev)
			req := httptest.NewRequest(http.MethodPost, "/api/anything", nil)
			req.Header.Set("Origin", tt.origin)
			rec := httptest.NewRecorder()
			s.withCSRFOriginCheck(next).ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

// TestAllowDevFrontendOrigin_NoopBeforeDeployConfig guards the call-order
// contract in its doc comment: AllowDevFrontendOrigin must not panic (or
// silently allocate a deployCfg) if called before SetDeployConfig.
func TestAllowDevFrontendOrigin_NoopBeforeDeployConfig(t *testing.T) {
	s := &Server{}
	s.AllowDevFrontendOrigin()
	if s.deployCfg != nil {
		t.Error("AllowDevFrontendOrigin created a deployCfg before SetDeployConfig ran")
	}
}
