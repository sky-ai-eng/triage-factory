package githubapp_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/githubapp"
	"github.com/sky-ai-eng/triage-factory/internal/upstream"
)

// TestAppTokenCalls_FailuresCarryAClass pins that the two App-JWT calls a poll
// cycle makes against the org's host, the installation-token mint and the
// installation listing, fail with an error the poller can classify. A failure
// with no class is a fault of TF's own to the poller, which records nothing
// about the connection and logs it at Error every cycle.
func TestAppTokenCalls_FailuresCarryAClass(t *testing.T) {
	htmlPage := "<!DOCTYPE html><html><body><h1>502 Bad Gateway</h1>" + strings.Repeat("<p>upstream unavailable</p>", 500) + "</body></html>"
	responses := []struct {
		name      string
		status    int
		body      string
		wantClass upstream.Class
	}{
		{name: "proxy 502", status: http.StatusBadGateway, body: htmlPage, wantClass: upstream.Transient},
		{name: "proxy 403", status: http.StatusForbidden, body: "<html><body>Connect to the VPN</body></html>", wantClass: upstream.Transient},
		{name: "bad credentials", status: http.StatusUnauthorized, body: `{"message":"A JSON web token could not be decoded"}`, wantClass: upstream.Auth},
	}
	calls := []struct {
		name string
		call func(context.Context, *githubapp.Minter) error
	}{
		{name: "mint", call: func(ctx context.Context, m *githubapp.Minter) error {
			_, err := m.MintInstallationToken(ctx, 1)
			return err
		}},
		{name: "list installations", call: func(ctx context.Context, m *githubapp.Minter) error {
			_, err := m.ListInstallations(ctx)
			return err
		}},
	}
	for _, call := range calls {
		for _, resp := range responses {
			t.Run(call.name+"/"+resp.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(resp.status)
					_, _ = w.Write([]byte(resp.body))
				}))
				defer srv.Close()

				err := call.call(context.Background(), minterAgainst(t, srv.URL, srv.Client()))
				if err == nil {
					t.Fatalf("%s against a %d returned no error", call.name, resp.status)
				}
				if class, ok := upstream.ClassOf(err); !ok || class != resp.wantClass {
					t.Errorf("ClassOf(%v) = (%q, %v), want %q", err, class, ok, resp.wantClass)
				}
				var status *githubapp.APIStatusError
				if !errors.As(err, &status) || status.StatusCode != resp.status {
					t.Errorf("err = %v, want an *APIStatusError with status %d", err, resp.status)
				}
				if msg := err.Error(); strings.Contains(msg, "<") || len(msg) > 300 {
					t.Errorf("the error carries the response body: %q", msg)
				}
			})
		}
		t.Run(call.name+"/refused", func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			addr := ln.Addr().String()
			_ = ln.Close()

			err = call.call(context.Background(), minterAgainst(t, "http://"+addr, &http.Client{Transport: &http.Transport{}}))
			if class, ok := upstream.ClassOf(err); !ok || class != upstream.Transient {
				t.Errorf("ClassOf(%v) = (%q, %v), want transient", err, class, ok)
			}
		})
	}
}

func minterAgainst(t *testing.T, apiBase string, hc *http.Client) *githubapp.Minter {
	t.Helper()
	m, err := githubapp.NewMinter(githubapp.Config{
		PrivateKey: newTestKey(t),
		AppID:      1,
		APIBase:    apiBase,
		HTTPClient: hc,
	})
	if err != nil {
		t.Fatalf("NewMinter: %v", err)
	}
	return m
}

// TestAppTokenCalls_ReportProgress: the mint and the listing are requests a
// poll cycle makes against its org's host, so each one that ends, answered
// or not, reports progress. A cycle on an App org whose host times out would
// otherwise spend minutes in these calls with no heartbeat, and its liveness
// check would fail while it handles the outage.
func TestAppTokenCalls_ReportProgress(t *testing.T) {
	calls := map[string]func(context.Context, *githubapp.Minter) error{
		"mint": func(ctx context.Context, m *githubapp.Minter) error {
			_, err := m.MintInstallationToken(ctx, 1)
			return err
		},
		"list installations": func(ctx context.Context, m *githubapp.Minter) error {
			_, err := m.ListInstallations(ctx)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name+"/answered", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer srv.Close()
			reports := 0
			ctx := upstream.WithProgress(context.Background(), func() { reports++ })
			_ = call(ctx, minterAgainst(t, srv.URL, srv.Client()))
			if reports != 1 {
				t.Errorf("an answered request reported progress %d times, want 1", reports)
			}
		})
		t.Run(name+"/timed out", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				<-r.Context().Done()
			}))
			defer srv.Close()
			reports := 0
			ctx := upstream.WithProgress(context.Background(), func() { reports++ })
			_ = call(ctx, minterAgainst(t, srv.URL, &http.Client{Timeout: 100 * time.Millisecond}))
			if reports != 1 {
				t.Errorf("a timed-out request reported progress %d times, want 1", reports)
			}
		})
	}
}
