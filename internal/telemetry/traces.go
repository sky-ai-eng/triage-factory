package telemetry

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

const (
	// envTracesEndpoint is the single knob that turns tracing on. Unset or
	// empty means disabled — in BOTH modes, unlike the metrics listener's
	// mode-dependent default. The asymmetry is deliberate: metrics open a
	// port (so the safe default differs between a laptop and a container),
	// while traces only push to a backend the operator had to name, so
	// "off unless configured" is the same answer everywhere.
	envTracesEndpoint = "TF_TRACES_ENDPOINT"

	// envOTLPEndpoint is the OTel-standard exporter endpoint. TF reads it
	// only to detect (and warn about) the case where an operator set the
	// conventional variable and expected tracing to come on — see
	// resolveTracesEndpoint. Every OTEL_EXPORTER_OTLP_* knob TF does not
	// wrap (headers, timeout, compression, TLS) still reaches the exporter
	// directly, because the SDK reads its own environment.
	envOTLPEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
)

// tracesMu guards tracerProvider — Init and ShutdownTraces run once each,
// at opposite ends of the process lifetime, but Close is reachable from a
// defer and (in tests) more than once, so the handle is taken under a lock
// and cleared in the same critical section.
var (
	tracesMu       sync.Mutex
	tracerProvider *sdktrace.TracerProvider
)

// tracesConfig is the resolved tracing configuration.
type tracesConfig struct {
	// endpoint is the OTLP/HTTP endpoint URL handed to the exporter; ""
	// means tracing is disabled and no exporter is built.
	//
	// Not necessarily the URL spans are POSTed to: a bare base URL
	// ("http://tempo:4318") carries an empty path, and the exporter fills
	// in the OTLP-standard /v1/traces for it. A path given here is used
	// verbatim instead. Both spellings are normal — a collector's docs
	// usually print the base — so this resolves the scheme and host, and
	// leaves the signal path to the exporter's own defaulting.
	endpoint string

	// otelEndpointIgnored records the one combination worth a log line:
	// OTEL_EXPORTER_OTLP_ENDPOINT set while TF_TRACES_ENDPOINT is not.
	// Tracing stays off (TF_TRACES_ENDPOINT is the only enable gate), but
	// an operator who set the conventional variable expected spans, so
	// silence would read as a bug in the exporter rather than a missing
	// knob.
	otelEndpointIgnored bool
}

// initTraces installs the SDK tracer provider (batch processor →
// OTLP/HTTP exporter) as the process-global provider when
// TF_TRACES_ENDPOINT names a backend, and does nothing at all otherwise —
// leaving the OTel global as the no-op provider, so every otel.Tracer call
// in the binary allocates nothing and no exporter goroutine or connection
// exists. res is the same resource the meter provider uses, so a process's
// metrics and spans carry identical service.name/service.version.
//
// Setup failure is logged and treated as disabled rather than failing
// boot, matching the metrics path: telemetry is diagnostics, never a boot
// dependency.
func initTraces(res *resource.Resource) {
	cfg, err := resolveTracesEndpoint(os.Getenv(envTracesEndpoint), os.Getenv(envOTLPEndpoint))
	if err != nil {
		telemetryLog.Error("invalid TF_TRACES_ENDPOINT; tracing disabled", "error", err)
		return
	}
	if cfg.otelEndpointIgnored {
		telemetryLog.Warn("OTEL_EXPORTER_OTLP_ENDPOINT is set but TF_TRACES_ENDPOINT is not; tracing stays disabled (set TF_TRACES_ENDPOINT to enable it)")
	}
	if cfg.endpoint == "" {
		return
	}

	// HTTP transport: New does not dial, so this cannot block boot and a
	// backend that is down yet (or ever) costs only retried exports.
	exporter, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(cfg.endpoint))
	if err != nil {
		telemetryLog.Error("otlp trace exporter setup failed; tracing disabled", "error", err)
		return
	}

	provider := sdktrace.NewTracerProvider(
		// Batched, not synchronous: an export must never sit in the
		// critical path of the work being traced. The flush obligation
		// this creates is ShutdownTraces' whole reason to exist.
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// Volume is modest (poll cycles every 30s, agent runs in the
		// tens), so every root is sampled. ParentBased is what makes a
		// later ratio sampler degrade whole traces rather than leaving
		// holes in the middle of one. This is also the SDK default —
		// stated explicitly because it is a decision, not an accident.
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)

	tracesMu.Lock()
	tracerProvider = provider
	tracesMu.Unlock()

	otel.SetTracerProvider(provider)
	// W3C trace context + baggage, the interop default. Installed only on
	// the enabled path, because with no provider there is nothing to
	// inject. Its consumer is outbound: TF's own HTTP clients carrying
	// trace context to services that understand it. The inbound direction
	// is deliberately unused on the user-facing API — honoring a
	// client-supplied parent on an internet-facing endpoint would let any
	// caller inject junk parents and, under parent-based sampling, choose
	// the sampling decision.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Route the SDK's own errors — export failures above all — through the
	// structured logger. Without this they go to stderr via the standard
	// log package, which in a JSON-logs deployment means an unparseable
	// line every batch interval for as long as the backend is unreachable.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		telemetryLog.Error("opentelemetry sdk error", "error", err)
	}))

	telemetryLog.Info("tracing enabled", "endpoint", cfg.redactedEndpoint())
}

// redactedEndpoint is the endpoint in the form that is safe to log: an
// operator may legitimately have put basic-auth credentials in the URL for
// a collector that wants them, and url.Redacted masks the password. Cheap
// enough to re-parse — this runs once, at boot.
func (c tracesConfig) redactedEndpoint() string {
	u, err := url.Parse(c.endpoint)
	if err != nil {
		return ""
	}
	return u.Redacted()
}

// ShutdownTraces flushes spans still sitting in the batch processor and
// stops the exporter. Callers pass a bounded context: shutdown runs during
// process exit, when the run context is already cancelled, so it needs a
// fresh deadline of its own rather than an inherited dead one.
//
// This is the only flush point in the binary — pull-based metrics never
// needed one, and without it a batch processor drops its final, unexported
// batch on SIGTERM: precisely the spans covering the shutdown that is
// being investigated. No-op (nil) when tracing is disabled, and safe to
// call more than once.
func ShutdownTraces(ctx context.Context) error {
	tracesMu.Lock()
	tp := tracerProvider
	tracerProvider = nil
	tracesMu.Unlock()
	if tp == nil {
		return nil
	}
	// The provider's tracers go no-op on Shutdown, so a late log line or
	// a straggling goroutine that starts a span after this point records
	// nothing rather than panicking or blocking.
	return tp.Shutdown(ctx)
}

// resolveTracesEndpoint turns the two environment values into a resolved
// tracing configuration. Pure, so the precedence rules below are table
// tests rather than a deployment experiment — same shape as resolveAddr.
//
// Precedence:
//
//   - TF_TRACES_ENDPOINT set → that value, always. It reaches the exporter
//     as an explicit option, which by OTel's own contract outranks
//     OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_TRACES_ENDPOINT.
//   - TF_TRACES_ENDPOINT unset/empty → disabled, even when
//     OTEL_EXPORTER_OTLP_ENDPOINT is set. A shared collector address
//     inherited from a compose file or a sidecar's env must not silently
//     start a trace pipeline nobody asked this process for; enabling
//     tracing is one deliberate variable, everywhere.
//   - TF_TRACES_ENDPOINT set to off/false/disabled/none/0 → disabled, the
//     escape hatch for overriding an inherited value.
//
// A value with no scheme is read as plaintext ("tempo:4318" →
// "http://tempo:4318"), since the in-cluster collector is the overwhelming
// case; spell https:// explicitly for a TLS backend. A path is passed
// through as given; this function never invents one, leaving the empty
// case to the exporter, which fills in the OTLP-standard /v1/traces.
func resolveTracesEndpoint(tfRaw, otelRaw string) (tracesConfig, error) {
	raw := strings.TrimSpace(tfRaw)
	switch strings.ToLower(raw) {
	case "":
		return tracesConfig{otelEndpointIgnored: strings.TrimSpace(otelRaw) != ""}, nil
	case "off", "false", "disabled", "none", "0":
		return tracesConfig{}, nil
	}

	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return tracesConfig{}, fmt.Errorf("parse %q: %w", tfRaw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return tracesConfig{}, fmt.Errorf("endpoint %q: scheme must be http or https, got %q", tfRaw, u.Scheme)
	}
	if u.Host == "" {
		return tracesConfig{}, fmt.Errorf("endpoint %q: missing host", tfRaw)
	}
	return tracesConfig{endpoint: u.String()}, nil
}
