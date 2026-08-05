// Package telemetry owns the process's OpenTelemetry SDK wiring: the meter
// provider behind the Prometheus /metrics listener, and the tracer provider
// behind the OTLP span exporter. It is deliberately a socket, not an
// engine: TF instruments itself with the OTel API and ships no collector,
// no dashboards, and no storage — operators point whatever scraper they
// already run (Prometheus, an OTel Collector's prometheus receiver, a
// vendor agent) at /metrics, and whatever trace backend they already run
// (Tempo, Jaeger, a collector, a vendor) at TF_TRACES_ENDPOINT.
//
// The two halves differ in direction, and that difference drives most of
// their asymmetries. Metrics are *pulled*: the process holds current values
// and a scraper comes to get them, so nothing is ever lost by exiting and
// the only configuration that matters is a bind address. Traces are
// *pushed*: finished spans are batched and sent, so an unflushed batch dies
// with the process (hence ShutdownTraces, which the binary's shutdown path
// must call) and the configuration that matters is a destination. Metrics
// therefore default on in multi mode, where opening a port is free; traces
// default off everywhere, since there is no sensible default destination.
//
// Conventions for instrumenting a subsystem:
//
//   - Create instruments from the global provider (otel.Meter) or accept a
//     metric.MeterProvider for testability. Init installs the real provider
//     before any subsystem constructs its instruments (app.New calls it
//     first), so no delegation subtleties apply — but the OTel global is
//     delegation-safe regardless.
//   - Name instruments in OTel dot form WITHOUT a vendor prefix
//     ("slack.ingest.events"); the exporter's namespace option prefixes
//     every metric with "tf_" uniformly (→ tf_slack_ingest_events_total).
//   - Labels must be low-cardinality and bounded: enum-like strings
//     (outcome, transport), opaque IDs with a small stable population
//     (api_app_id, org_id). Never per-entity, per-run, per-user, or
//     free-text label values.
//   - Metric values must be safe to expose unauthenticated on an internal
//     network: counts, durations, opaque IDs — never repo names, usernames,
//     message text, or other tenant data. Same posture as GET /readyz.
//
// Conventions for tracing a subsystem:
//
//   - Get a tracer from the global provider, named for the Go package that
//     owns the spans (otel.Tracer("internal/poller")). No package needs to
//     know whether tracing is on: disabled means the no-op provider, and a
//     no-op span costs an interface call.
//   - Name spans noun.verb or subsystem.stage in OTel dot form
//     ("poll.github.org", "route.event", "engagement.setup"). Span names
//     are a low-cardinality dimension exactly like metric names — never
//     interpolate an ID into one.
//   - Attribute hygiene is the metric-label rule, verbatim: opaque IDs
//     (org.id, event.id, conversation.id, task.id) yes; repo names, PR
//     titles, usernames, message text, file paths never. Spans leave the
//     process, so this is a data-egress boundary, not a cardinality
//     preference.
//   - An anticipated skip (provider backoff, dedup, quiet hours) is an
//     outcome attribute, not a span error status. Reserve the error status
//     for things that are actually wrong, so "traces with errors" stays a
//     usable filter.
//   - Log correlation is automatic but not free: internal/logging stamps
//     trace_id/span_id from the record's context, which slog only carries
//     on the *Context call variants. Under a span, prefer
//     log.InfoContext(ctx, ...) over log.Info(...).
package telemetry

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"

	"github.com/sky-ai-eng/triage-factory/internal/logging"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

var telemetryLog = logging.Component("telemetry")

// DefaultMetricsPort is the port the metrics listener binds when
// TF_METRICS_ADDR is unset in multi mode — the OTel ecosystem's
// conventional Prometheus-exporter port. The listener is its own port
// (never the user-facing HTTP server) so the default posture is safe: an
// orchestrator doesn't route or publish a port nobody asked it to, which
// keeps the unauthenticated endpoint network-internal unless the operator
// deliberately exposes it.
const DefaultMetricsPort = "9464"

// metricsNamespace prefixes every OTel-instrumented metric at export time
// ("slack.ingest.events" → "tf_slack_ingest_events_total"), so instrument
// names stay vendor-free at the call site and the exported set is
// greppable as tf_*. The client_golang runtime collectors keep their
// standard go_* / process_* names — dashboards and mixins expect those.
const metricsNamespace = "tf"

// handler is the /metrics HTTP handler Init builds and Serve mounts.
// Package-level (mirroring OTel's own global-provider posture) because the
// meter provider Init installs is itself a process-global; nil until Init
// runs with metrics enabled.
var handler http.Handler

// Init resolves the telemetry configuration and installs whichever of the
// two SDK providers the environment enables: the meter provider (backed by
// a Prometheus exporter + registry with the Go runtime collectors) and the
// tracer provider (batch processor → OTLP/HTTP exporter, see traces.go).
// The two are independent — either, both, or neither — and share one
// resource, so a process's metrics and spans agree on service identity.
// Returns the resolved metrics bind address, "" when metrics are disabled —
// the caller keeps it and hands it to Serve after its workers are up.
// Tracing needs no such second step: it pushes, so there is nothing to
// serve.
//
// Called once at boot, before any subsystem constructs instruments or
// tracers. When disabled, the corresponding OTel global stays the no-op
// provider, so every instrument and span in the binary costs nothing —
// local mode's default for metrics, and every mode's default for traces.
// A setup failure is logged and reported as disabled rather than failing
// boot: telemetry is diagnostics, never a boot dependency.
//
// The caller owns one obligation in return: call ShutdownTraces during
// shutdown, or the final batch of spans is dropped.
func Init(version string) (addr string) {
	res := buildResource(version)
	initTraces(res)
	return initMetrics(res)
}

// buildResource assembles the service identity both providers export
// under.
func buildResource(version string) *resource.Resource {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName("triagefactory"),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		// Merge only errors on schema-URL conflicts, which NewSchemaless
		// can't produce — but if it ever does, a default resource still
		// yields a working exporter, so degrade rather than disable.
		telemetryLog.Warn("telemetry resource merge failed; using default resource", "error", err)
		return resource.Default()
	}
	return res
}

// initMetrics installs the meter provider and builds the /metrics handler
// Serve mounts, returning the resolved bind address ("" = disabled).
func initMetrics(res *resource.Resource) (addr string) {
	addr = resolveAddr(os.Getenv("TF_METRICS_ADDR"), runmode.Current())
	if addr == "" {
		return ""
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	exporter, err := otelprom.New(
		otelprom.WithRegisterer(registry),
		otelprom.WithNamespace(metricsNamespace),
		otelprom.WithoutScopeInfo(),
		// Full Prometheus-style naming: dots → underscores, counters get
		// _total. Instruments here carry no units, so no unit suffixes ever
		// materialize. Pinned explicitly because the exporter's default
		// currently varies with prometheus/common's validation scheme.
		otelprom.WithTranslationStrategy(otlptranslator.UnderscoreEscapingWithSuffixes),
	)
	if err != nil {
		telemetryLog.Error("prometheus exporter setup failed; metrics disabled", "error", err)
		return ""
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(exporter),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(provider)

	// OpenMetrics content negotiation is what makes exemplars reachable:
	// the SDK records them by default whenever a sampled span is active
	// (its trace-based exemplar filter), but the Prometheus text format
	// has nowhere to put them, so a scraper only sees them if it can ask
	// for OpenMetrics and this handler is willing to answer in it. That is
	// the one genuine metrics↔traces bridge — a histogram bucket that
	// hands you the trace ID of a request that landed in it. Scrapers that
	// don't negotiate still get plain Prometheus text, unchanged.
	handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{EnableOpenMetrics: true})
	return addr
}

// Serve blocks serving GET /metrics on addr until ctx is cancelled,
// mirroring the executor healthz listener's shutdown discipline. Callers
// run it in a goroutine; a bind failure (port in use, bad addr) is logged
// and returned but must not take the process down — the caller launches
// and forgets.
func Serve(ctx context.Context, addr string) error {
	if handler == nil {
		return errors.New("telemetry: Serve called without a successful Init")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		telemetryLog.Error("metrics listener bind failed; metrics unavailable", "addr", addr, "error", err)
		return err
	}
	return serveOn(ctx, ln)
}

// serveOn serves /metrics on an already-bound listener — split from Serve
// so tests can bind :0 and read the real address back.
func serveOn(ctx context.Context, ln net.Listener) error {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", handler)
	httpSrv := &http.Server{
		Handler: mux,
		// Same slowloris posture as the per-run proxy listeners (apiproxy/
		// llmproxy/gitproxy): this endpoint is unauthenticated, so a client
		// must never be able to hold a connection open for free. Unlike the
		// proxies, nothing here streams — a scrape is one small GET — so the
		// whole exchange is bounded too, not just the header read.
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	serverExited := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutdownCtx)
		case <-serverExited:
		}
	}()

	telemetryLog.Info("metrics listening", "addr", ln.Addr().String())
	err := httpSrv.Serve(ln)
	close(serverExited)
	<-shutdownDone

	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// resolveAddr turns the TF_METRICS_ADDR value into a bind address, "" for
// disabled. Unset/empty falls back to the mode default: on in multi
// (":"+DefaultMetricsPort — an unpublished container port until the
// operator routes it), off in local (a laptop process doesn't open extra
// ports unasked). An explicit "off"/"false"/"disabled"/"0" disables in any
// mode. A bare port number binds all interfaces in multi and loopback in
// local (matching local mode's loopback-only bind posture); anything else
// is used verbatim as a host:port.
//
// Deliberate overload, worth naming: bare "0" means disable (the common
// env-var idiom), NOT Go's "OS picks an ephemeral port". The Go spelling
// still works — ":0" (or "127.0.0.1:0") is not a bare port, so it passes
// through verbatim to net.Listen — but an ephemeral port is almost never
// what a metrics endpoint wants (scrape configs need a stable target).
func resolveAddr(raw string, mode runmode.Mode) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	switch v {
	case "":
		if mode == runmode.ModeMulti {
			return ":" + DefaultMetricsPort
		}
		return ""
	case "off", "false", "disabled", "none", "0":
		return ""
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 65535 {
		if mode == runmode.ModeLocal {
			return net.JoinHostPort("127.0.0.1", v)
		}
		return ":" + v
	}
	return strings.TrimSpace(raw)
}
