package slack

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Ingest outcome label values — one per return path in the ingest pipeline
// (ingest.go), so tf_slack_ingest_events_total{outcome=...} partitions every
// delivery that reached handleEventCallback: exactly one outcome is recorded
// per call, making the metric's sum the "received" total the drop/accept
// split is read against. Values are a bounded enum by construction (see the
// label-cardinality rule in internal/telemetry's package doc).
const (
	// outcomeAccepted is the single success path: the delivery published a
	// slack:message event.
	outcomeAccepted = "accepted"
	// outcomeError is any genuine pipeline failure (store error, marshal
	// failure) — the transport 5xxes / withholds its ack so Slack redelivers,
	// meaning the same event may be counted again under its retry.
	outcomeError = "error"

	dropUnsupportedType    = "unsupported_type"    // inner event type this pipeline doesn't ingest
	dropUnsupportedSubtype = "unsupported_subtype" // message edit/delete/system/bot subtype variants
	dropNotThreadReply     = "not_thread_reply"    // root-channel chatter; the bot listens only to threads it owns
	dropMalformed          = "malformed"           // missing event_id/channel/ts
	dropSelfOrBot          = "self_or_bot"         // authored by TF's own bot or another bot
	dropMentionDedup       = "mention_dedup"       // threaded @-mention owned by its app_mention twin delivery
	dropNotEngaged         = "not_engaged"         // no active bot-owned thread entity for this reply
	dropDuplicate          = "duplicate"           // slack_event_deliveries dedup hit (a Slack redelivery)
)

// ingestStats owns the Slack ingest metric instruments — the message-volume
// observability the message.* subscriptions made necessary (mentions were
// inherently low-volume; the channel firehose is not). One instance is
// shared by the pipeline and both transports (install.go); counters are
// keyed by api_app_id, the same per-app grain as the Socket Mode connection,
// because the app is the unit an operator acts on (its manifest, its rate
// limit, its subscription Slack would disable).
//
// Nil-safe on every method, like the pipeline's other optional
// collaborators (identity, channels, ...): tests that construct
// ingestPipeline directly without stats simply record nothing.
type ingestStats struct {
	events      metric.Int64Counter
	retries     metric.Int64Counter
	rateLimited metric.Int64Counter
}

// newIngestStats builds the instrument set against mp — production passes
// the global provider (install.go), tests pass an sdkmetric.MeterProvider
// wired to a ManualReader. Instrument-creation errors can only be
// programmer errors (invalid names); the API contract returns a usable
// no-op instrument alongside the error, so they're logged and ignored
// rather than propagated.
func newIngestStats(mp metric.MeterProvider) *ingestStats {
	meter := mp.Meter("github.com/sky-ai-eng/triage-factory/ee/slack")

	events, err := meter.Int64Counter("slack.ingest.events",
		metric.WithDescription("Inbound Slack event deliveries that reached the ingest pipeline, by outcome (accepted, or the drop reason)."))
	if err != nil {
		slackLog.Warn("ingest events counter setup failed", "error", err)
	}
	retries, err := meter.Int64Counter("slack.retry.deliveries",
		metric.WithDescription("Deliveries Slack marked as retries of an earlier attempt (X-Slack-Retry-Num > 0 / envelope retry_attempt > 0) — sustained retries on the Events API precede Slack disabling the subscription."))
	if err != nil {
		slackLog.Warn("retry deliveries counter setup failed", "error", err)
	}
	rateLimited, err := meter.Int64Counter("slack.app.rate.limited",
		metric.WithDescription("app_rate_limited notices from Slack — event deliveries for the app are being dropped at the source."))
	if err != nil {
		slackLog.Warn("app rate limited counter setup failed", "error", err)
	}

	return &ingestStats{events: events, retries: retries, rateLimited: rateLimited}
}

// recordIngest counts one delivery's pipeline outcome — exactly one call
// per handleEventCallback invocation, so sum(outcome=*) == received.
func (s *ingestStats) recordIngest(ctx context.Context, appID, outcome string) {
	if s == nil {
		return
	}
	s.events.Add(ctx, 1, metric.WithAttributes(
		attribute.String("app_id", appID),
		attribute.String("outcome", outcome),
	))
}

// recordRetry counts one delivery Slack marked as a retry, by transport.
func (s *ingestStats) recordRetry(ctx context.Context, appID, transport string) {
	if s == nil {
		return
	}
	s.retries.Add(ctx, 1, metric.WithAttributes(
		attribute.String("app_id", appID),
		attribute.String("transport", transport),
	))
}

// recordRateLimited counts one app_rate_limited notice.
func (s *ingestStats) recordRateLimited(ctx context.Context, appID string) {
	if s == nil {
		return
	}
	s.rateLimited.Add(ctx, 1, metric.WithAttributes(
		attribute.String("app_id", appID),
	))
}
