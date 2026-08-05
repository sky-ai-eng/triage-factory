package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestResolveTracesEndpoint(t *testing.T) {
	cases := []struct {
		name        string
		tfRaw       string
		otelRaw     string
		want        string
		wantIgnored bool
		wantErr     bool
	}{
		{name: "unset is disabled", want: ""},
		{name: "empty-ish is disabled", tfRaw: "   ", want: ""},
		// The enable gate is TF_TRACES_ENDPOINT and only TF_TRACES_ENDPOINT:
		// an OTLP endpoint inherited from a compose file or a sidecar's env
		// must not silently start a trace pipeline. It is worth a log line,
		// though, which is what the ignored flag drives.
		{name: "otel endpoint alone does not enable", otelRaw: "http://collector:4318", want: "", wantIgnored: true},
		{name: "tf endpoint wins over otel endpoint", tfRaw: "http://tempo:4318", otelRaw: "http://collector:4318", want: "http://tempo:4318"},
		{name: "off disables", tfRaw: "off", want: ""},
		{name: "false disables", tfRaw: "False", want: ""},
		{name: "disabled disables", tfRaw: "disabled", want: ""},
		{name: "none disables", tfRaw: "none", want: ""},
		{name: "zero disables", tfRaw: "0", want: ""},
		// An explicit disable still counts as "TF_TRACES_ENDPOINT was set",
		// so there is nothing surprising to warn about.
		{name: "explicit off suppresses the otel warning", tfRaw: "off", otelRaw: "http://collector:4318", want: "", wantIgnored: false},
		{name: "scheme-less value is plaintext http", tfRaw: "tempo:4318", want: "http://tempo:4318"},
		{name: "https preserved", tfRaw: "https://tempo.example.com:4318", want: "https://tempo.example.com:4318"},
		{name: "explicit path preserved", tfRaw: "http://collector:4318/v1/traces", want: "http://collector:4318/v1/traces"},
		{name: "whitespace trimmed", tfRaw: "  http://tempo:4318 ", want: "http://tempo:4318"},
		{name: "non-http scheme rejected", tfRaw: "grpc://tempo:4317", wantErr: true},
		{name: "scheme without host rejected", tfRaw: "http://", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveTracesEndpoint(tc.tfRaw, tc.otelRaw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("resolveTracesEndpoint(%q, %q) = %+v, nil; want an error", tc.tfRaw, tc.otelRaw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveTracesEndpoint(%q, %q): %v", tc.tfRaw, tc.otelRaw, err)
			}
			if got.endpoint != tc.want {
				t.Errorf("endpoint = %q; want %q", got.endpoint, tc.want)
			}
			if got.otelEndpointIgnored != tc.wantIgnored {
				t.Errorf("otelEndpointIgnored = %v; want %v", got.otelEndpointIgnored, tc.wantIgnored)
			}
		})
	}
}

// TestInitTraces_ExportsAndFlushesOnShutdown covers the enabled path end to
// end against a stub OTLP collector: Init installs a real provider, a span
// recorded through the OTel global is batched (nothing exported yet), and
// ShutdownTraces — the SIGTERM path, via App.Close — is what actually
// delivers it. Without that flush the batch processor drops the batch on
// exit, which is the whole reason the shutdown handle exists.
func TestInitTraces_ExportsAndFlushesOnShutdown(t *testing.T) {
	restoreTraceGlobals(t)

	type export struct {
		path string
		body []byte
	}
	exports := make(chan export, 8)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		exports <- export{path: r.URL.Path, body: body}
		// An empty 200 decodes as an ExportTraceServiceResponse with no
		// partial-success field, which is what the exporter treats as a
		// clean accept.
		w.WriteHeader(http.StatusOK)
	}))
	defer collector.Close()

	t.Setenv("TF_METRICS_ADDR", "off")
	t.Setenv("TF_TRACES_ENDPOINT", collector.URL)
	if addr := Init("v-test"); addr != "" {
		t.Fatalf("Init returned metrics addr %q; want metrics disabled in this test", addr)
	}

	span := startSpan(t, "test.span.flushed")

	// Batched, so nothing should have left the process yet.
	select {
	case e := <-exports:
		t.Fatalf("span exported before shutdown (batching not in effect): %d bytes to %s", len(e.body), e.path)
	case <-time.After(200 * time.Millisecond):
	}

	span.End()
	if err := ShutdownTraces(context.Background()); err != nil {
		t.Fatalf("ShutdownTraces: %v", err)
	}

	select {
	case e := <-exports:
		if e.path != "/v1/traces" {
			t.Errorf("exported to %q; want the default OTLP traces path", e.path)
		}
		// Assert on the raw protobuf rather than decoding it: string fields
		// are length-delimited UTF-8 on the wire, so a substring check is
		// reliable here and keeps the OTLP protobuf packages out of this
		// module's direct dependencies for the sake of one assertion.
		for _, want := range []string{"test.span.flushed", "triagefactory", "v-test"} {
			if !bytes.Contains(e.body, []byte(want)) {
				t.Errorf("exported payload missing %q (%d bytes)", want, len(e.body))
			}
		}
	default:
		t.Fatal("no span exported after ShutdownTraces")
	}

	// Idempotent: Close is reachable from a defer, and a second call must
	// not panic on the shut-down provider.
	if err := ShutdownTraces(context.Background()); err != nil {
		t.Fatalf("second ShutdownTraces: %v", err)
	}
}

// TestInitTraces_ModeAgnostic: TF_TRACES_ENDPOINT is the whole switch, in
// both run modes. The metrics listener's default flips with the mode
// because it opens a port, and what counts as a safe default port differs
// between a laptop and a container; a trace exporter only pushes to an
// address the operator typed, so there is nothing for the mode to decide.
// Gating this to multi would also mean the full Postgres + GoTrue + runsc
// stack were a prerequisite for seeing a single span.
func TestInitTraces_ModeAgnostic(t *testing.T) {
	for _, mode := range []runmode.Mode{runmode.ModeLocal, runmode.ModeMulti} {
		t.Run(string(mode), func(t *testing.T) {
			restoreTraceGlobals(t)
			runmode.SetForTest(t, mode)

			exported := make(chan struct{}, 4)
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				exported <- struct{}{}
				w.WriteHeader(http.StatusOK)
			}))
			defer collector.Close()

			t.Setenv("TF_METRICS_ADDR", "off")
			t.Setenv("TF_TRACES_ENDPOINT", collector.URL)
			Init("v-test")

			startSpan(t, "mode.probe").End()
			if err := ShutdownTraces(context.Background()); err != nil {
				t.Fatalf("ShutdownTraces: %v", err)
			}
			select {
			case <-exported:
			default:
				t.Fatalf("no span exported in %s mode", mode)
			}
		})
	}
}

// TestInitTraces_Disabled: with no TF_TRACES_ENDPOINT the global tracer
// stays the no-op provider — spans carry no span context, so nothing can
// be sampled, exported, or correlated, and there is no exporter to shut
// down.
func TestInitTraces_Disabled(t *testing.T) {
	restoreTraceGlobals(t)

	t.Setenv("TF_METRICS_ADDR", "off")
	t.Setenv("TF_TRACES_ENDPOINT", "")
	// Set to prove it is not an enable gate on its own.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.invalid:4318")

	Init("v-test")

	_, span := otel.Tracer("telemetry/test").Start(context.Background(), "noop")
	defer span.End()
	if sc := span.SpanContext(); sc.IsValid() {
		t.Errorf("span context is valid with tracing disabled: %s", sc.TraceID())
	}
	if span.IsRecording() {
		t.Error("span is recording with tracing disabled")
	}
	if err := ShutdownTraces(context.Background()); err != nil {
		t.Fatalf("ShutdownTraces with tracing disabled: %v", err)
	}
}

// TestInitTraces_ExemplarsExposed: with both signals on, a counter
// incremented under a sampled span carries that span's trace ID as a
// Prometheus exemplar — but only for a scraper that negotiates
// OpenMetrics, since the plain text format has nowhere to put one. This is
// the metrics↔traces bridge, and it is entirely a function of the
// handler's content negotiation.
func TestInitTraces_ExemplarsExposed(t *testing.T) {
	restoreTraceGlobals(t)
	prevMeter := otel.GetMeterProvider()
	prevHandler := handler
	t.Cleanup(func() {
		otel.SetMeterProvider(prevMeter)
		handler = prevHandler
	})

	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	// Registered before the shutdown cleanup below so it runs after it
	// (cleanups are LIFO): the final flush must still find a live
	// collector, or it logs a connection error mid-test.
	t.Cleanup(collector.Close)

	t.Setenv("TF_METRICS_ADDR", "127.0.0.1:0")
	t.Setenv("TF_TRACES_ENDPOINT", collector.URL)
	if addr := Init("v-test"); addr == "" {
		t.Fatal("Init reported metrics disabled; want enabled")
	}
	t.Cleanup(func() { _ = ShutdownTraces(context.Background()) })

	ctx, span := otel.Tracer("telemetry/test").Start(context.Background(), "exemplar.probe")
	traceID := span.SpanContext().TraceID().String()
	if !span.SpanContext().IsValid() {
		t.Fatal("span context invalid with tracing enabled")
	}
	counter, err := otel.Meter("telemetry/test").Int64Counter("exemplar.probe.events")
	if err != nil {
		t.Fatalf("create counter: %v", err)
	}
	counter.Add(ctx, 1)
	span.End()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = serveOn(serveCtx, ln) }()

	body := scrape(t, ln.Addr().String(), "application/openmetrics-text; version=1.0.0")
	if !strings.Contains(body, traceID) {
		t.Errorf("openmetrics scrape missing exemplar trace id %s:\n%s", traceID, body)
	}

	// A plain-Prometheus scraper is unaffected: same series, no exemplar
	// syntax it could not parse.
	plain := scrape(t, ln.Addr().String(), "text/plain;version=0.0.4")
	if !strings.Contains(plain, "tf_exemplar_probe_events_total") {
		t.Errorf("plain scrape missing the counter:\n%s", plain)
	}
	if strings.Contains(plain, traceID) {
		t.Errorf("plain-text scrape leaked exemplar syntax:\n%s", plain)
	}
}

// restoreTraceGlobals returns the process-global tracing state to the
// no-op defaults after the test, so a later test in this package does not
// inherit a live (or shut-down) SDK provider.
//
// Not a snapshot-and-restore: otel.GetTracerProvider hands back the global
// *delegating* provider, which is the same object before and after Init, so
// setting it back would restore nothing. Pointing it at a fresh no-op is
// what actually undoes the install.
func restoreTraceGlobals(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		_ = ShutdownTraces(context.Background())
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})
}

// startSpan starts a span through the OTel global exactly as an
// instrumented subsystem would, and fails the test if tracing is not
// actually live.
func startSpan(t *testing.T, name string) trace.Span {
	t.Helper()
	_, span := otel.Tracer("telemetry/test").Start(context.Background(), name)
	if !span.SpanContext().IsValid() {
		t.Fatalf("span %q has no span context; the SDK provider was not installed", name)
	}
	return span
}

func scrape(t *testing.T, addr, accept string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/metrics", addr), nil)
	if err != nil {
		t.Fatalf("build scrape request: %v", err)
	}
	req.Header.Set("Accept", accept)
	resp, err := http.DefaultClient.Do(req)
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
	return string(raw)
}

// TestRedactedEndpoint: a collector behind basic auth is a legitimate
// configuration, and the boot log line must not print its password.
func TestRedactedEndpoint(t *testing.T) {
	cfg, err := resolveTracesEndpoint("https://otel:hunter2@collector.example.com:4318", "")
	if err != nil {
		t.Fatalf("resolveTracesEndpoint: %v", err)
	}
	if !strings.Contains(cfg.endpoint, "hunter2") {
		t.Fatalf("endpoint lost its credentials; the exporter needs them: %q", cfg.endpoint)
	}
	if got := cfg.redactedEndpoint(); strings.Contains(got, "hunter2") {
		t.Errorf("redactedEndpoint leaked the password: %q", got)
	}
}
