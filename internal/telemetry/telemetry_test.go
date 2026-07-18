package telemetry

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestResolveAddr(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		mode runmode.Mode
		want string
	}{
		{"unset multi defaults on", "", runmode.ModeMulti, ":" + DefaultMetricsPort},
		{"unset local defaults off", "", runmode.ModeLocal, ""},
		{"off disables in multi", "off", runmode.ModeMulti, ""},
		{"disabled disables in multi", "Disabled", runmode.ModeMulti, ""},
		{"zero disables", "0", runmode.ModeMulti, ""},
		{"false disables in local too", "false", runmode.ModeLocal, ""},
		{"bare port multi binds all interfaces", "9911", runmode.ModeMulti, ":9911"},
		{"bare port local binds loopback", "9911", runmode.ModeLocal, "127.0.0.1:9911"},
		{"out-of-range port is not a port", "99999", runmode.ModeMulti, "99999"},
		{"host:port verbatim", "10.0.0.5:9464", runmode.ModeLocal, "10.0.0.5:9464"},
		{"whitespace trimmed", "  :9000 ", runmode.ModeMulti, ":9000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveAddr(tc.raw, tc.mode); got != tc.want {
				t.Errorf("resolveAddr(%q, %s) = %q; want %q", tc.raw, tc.mode, got, tc.want)
			}
		})
	}
}

// TestInitAndServe_EndToEnd covers the whole enabled path: Init installs
// the SDK provider, an instrument recorded through the OTel global comes
// out of GET /metrics under the tf_ namespace alongside the Go runtime
// collectors and the service resource, and cancellation shuts the listener
// down cleanly.
func TestInitAndServe_EndToEnd(t *testing.T) {
	t.Setenv("TF_METRICS_ADDR", "127.0.0.1:0")
	if addr := Init("v-test"); addr != "127.0.0.1:0" {
		t.Fatalf("Init returned %q; want the configured addr", addr)
	}

	// Record through the global provider exactly like an instrumented
	// subsystem would.
	counter, err := otel.Meter("telemetry/test").Int64Counter("test.probe.events")
	if err != nil {
		t.Fatalf("create counter: %v", err)
	}
	counter.Add(context.Background(), 3)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serveOn(ctx, ln) }()

	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", ln.Addr()))
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrape status = %d; want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read scrape body: %v", err)
	}
	body := string(raw)

	for _, want := range []string{
		"tf_test_probe_events_total", // namespaced OTel instrument, counter suffix intact
		"go_goroutines",              // client_golang Go runtime collector
		"target_info",                // resource: service name/version
		"triagefactory",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scrape body missing %q", want)
		}
	}
	if !strings.Contains(body, "tf_test_probe_events_total 3") {
		t.Errorf("expected tf_test_probe_events_total datapoint of 3 in scrape body:\n%s", body)
	}

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serveOn returned %v after cancel; want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serveOn did not return after cancel")
	}
}

// TestServe_WithoutInit refuses to serve a nil handler rather than
// panicking in the goroutine app.Run fires.
func TestServe_WithoutInit(t *testing.T) {
	prev := handler
	handler = nil
	t.Cleanup(func() { handler = prev })
	if err := Serve(context.Background(), "127.0.0.1:0"); err == nil {
		t.Fatal("Serve without Init: want error")
	}
}

// TestInit_Disabled: with no TF_METRICS_ADDR in local mode, Init reports
// disabled and leaves the global provider alone.
func TestInit_Disabled(t *testing.T) {
	t.Setenv("TF_METRICS_ADDR", "off")
	if addr := Init("v-test"); addr != "" {
		t.Fatalf("Init = %q; want disabled", addr)
	}
}
