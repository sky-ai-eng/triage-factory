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
	// outcomeChannelMoved is a channel_id_changed delivery that moved the
	// channel's rows to its new id, or found them already moved.
	outcomeChannelMoved = "channel_moved"
	// outcomeDisconnected is an app_uninstalled or tokens_revoked delivery
	// that removed the (workspace, app) row and its secrets.
	outcomeDisconnected = "disconnected"
	// outcomeError is any genuine pipeline failure (store error, marshal
	// failure) — the transport 5xxes / withholds its ack so Slack redelivers,
	// meaning the same event may be counted again under its retry.
	outcomeError = "error"

	dropUnsupportedType    = "unsupported_type"    // inner event type this pipeline doesn't ingest
	dropUnsupportedSubtype = "unsupported_subtype" // message edit/delete/system/bot subtype variants
	dropNotThreadReply     = "not_thread_reply"    // root-channel chatter; the bot listens only to threads it owns
	dropMalformed          = "malformed"           // missing event_id/channel/ts, or a channel_id_changed missing either id
	dropSelfOrBot          = "self_or_bot"         // authored by TF's own bot or another bot
	dropMentionDedup       = "mention_dedup"       // threaded @-mention owned by its app_mention twin delivery
	dropNotEngaged         = "not_engaged"         // no active bot-owned thread entity for this reply
	dropDuplicate          = "duplicate"           // slack_event_deliveries dedup hit (a Slack redelivery)
	dropUserTokensOnly     = "user_tokens_only"    // tokens_revoked naming no bot token; TF holds no user tokens
	dropReconnected        = "reconnected"         // app removal Slack generated before the row's current connection
	dropRowChanged         = "row_changed"         // app removal whose row changed after the receiver read it
	dropInactive           = "inactive"            // delivered through a connection that is not the workspace's active one
)

// ingestStats owns the Slack ingest metric instruments — the message-volume
// observability the message.* subscriptions made necessary (mentions were
// inherently low-volume; the channel firehose is not). One instance is
// shared by the pipeline and both transports (install.go); the delivery
// counters are keyed by api_app_id, the same per-app grain as the Socket Mode
// connection, because the app is the unit an operator acts on (its manifest,
// its rate limit, its subscription Slack would disable).
//
// Nil-safe on every method, like the pipeline's other optional
// collaborators (identity, channels, ...): tests that construct
// ingestPipeline directly without stats simply record nothing.
//
// It also holds the counters for the deployment app's install ceremony and the
// lifecycle adapter's dropped events, so every Slack counter comes from one
// meter and one constructor.
type ingestStats struct {
	events              metric.Int64Counter
	retries             metric.Int64Counter
	rateLimited         metric.Int64Counter
	signatureMismatches metric.Int64Counter
	orgDeliveries       metric.Int64Counter
	installs            metric.Int64Counter
	lifecycleDropped    metric.Int64Counter
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

	signatureMismatches, err := meter.Int64Counter("slack.webhook.signature.mismatches",
		metric.WithDescription("Deliveries to the deployment Slack app's Events API endpoint with a fresh, well-formed signature that TF_SLACK_SIGNING_SECRET does not produce, by the app the unverified body names (deployment, other, or none). A wrong signing secret fails every delivery this way; so does a forged request."))
	if err != nil {
		slackLog.Warn("signature mismatches counter setup failed", "error", err)
	}

	orgDeliveries, err := meter.Int64Counter("slack.webhook.org.deliveries",
		metric.WithDescription("Deliveries to an org's Events API endpoint that name a workspace and app connected to that org, by app and by verdict: the signature check's (verified, mismatch, stale, malformed), or no_secret when the connection's signing secret could not be read. One app's refusals are its configuration; most apps' at once are the deployment's: its secret store, a proxy changing request bodies or headers, or a clock more than five minutes off."))
	if err != nil {
		slackLog.Warn("org deliveries counter setup failed", "error", err)
	}

	installs, err := meter.Int64Counter("slack.install.attempts",
		metric.WithDescription("Ends of the deployment Slack app's install callback for a signed-in person, by outcome: installed or staged, or why it stored nothing. A request with no session is refused before anything is counted."))
	if err != nil {
		slackLog.Warn("install attempts counter setup failed", "error", err)
	}
	lifecycleDropped, err := meter.Int64Counter("slack.lifecycle.dropped",
		metric.WithDescription("Events the lifecycle adapter dropped because its dispatch backlog was full, by event type: each is a status line, reaction or reply in Slack that was never made."))
	if err != nil {
		slackLog.Warn("lifecycle dropped counter setup failed", "error", err)
	}

	return &ingestStats{
		events:              events,
		retries:             retries,
		rateLimited:         rateLimited,
		signatureMismatches: signatureMismatches,
		orgDeliveries:       orgDeliveries,
		installs:            installs,
		lifecycleDropped:    lifecycleDropped,
	}
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

// recordSignatureMismatch counts one delivery to the deployment endpoint that
// the deployment app's signing secret did not sign. namesApp is one of the
// mismatchNames* values.
func (s *ingestStats) recordSignatureMismatch(ctx context.Context, appID, namesApp string) {
	if s == nil {
		return
	}
	s.signatureMismatches.Add(ctx, 1, metric.WithAttributes(
		attribute.String("app_id", appID),
		attribute.String("names_app", namesApp),
	))
}

// orgDeliveryNoSecret is the verdict on slack.webhook.org.deliveries for a
// delivery whose connection's signing secret could not be read, so its
// signature was never checked.
const orgDeliveryNoSecret = "no_secret"

// recordOrgDelivery counts one delivery to an org's Events API endpoint that
// named a connection with a signing secret. appID is the connection's app, and
// verdict a signatureCheck's label or orgDeliveryNoSecret.
func (s *ingestStats) recordOrgDelivery(ctx context.Context, appID, verdict string) {
	if s == nil {
		return
	}
	s.orgDeliveries.Add(ctx, 1, metric.WithAttributes(
		attribute.String("app_id", appID),
		attribute.String("signature", verdict),
	))
}

// The outcomes of slack.install.attempts, one per way the deployment app's
// install callback ends for a signed-in person. A request with no session is
// not counted, because anyone can send one and the count would then say
// nothing about the people installing. installed and staged stored a
// connection. The rest
// stored nothing; their names follow the slack_error codes the callback
// redirects with, with install_failed split by its cause, because the causes
// an operator fixes (wrong_app, missing_scopes, token_rotation) are what the
// alert on this counter selects.
const (
	installInstalled         = "installed"
	installStaged            = "staged"
	installState             = "state"
	installSSORequired       = "sso_required"
	installDenied            = "denied"
	installNoApp             = "no_app"
	installSlackError        = "slack_error"
	installNoCode            = "no_code"
	installExchangeFailed    = "exchange_failed"
	installWrongApp          = "wrong_app"
	installEnterpriseInstall = "enterprise_install"
	installMissingScopes     = "missing_scopes"
	installTokenRotation     = "token_rotation"
	installNoWorkspace       = "no_workspace"
	installWorkspaceTaken    = "workspace_taken"
	installError             = "error"
)

// recordInstall counts one end of the install callback. outcome is one of the
// install* values.
func (s *ingestStats) recordInstall(ctx context.Context, outcome string) {
	if s == nil {
		return
	}
	s.installs.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

// recordLifecycleDropped counts one event the lifecycle adapter dropped.
// eventType is a catalog event type, which keeps the label bounded.
func (s *ingestStats) recordLifecycleDropped(ctx context.Context, eventType string) {
	if s == nil {
		return
	}
	s.lifecycleDropped.Add(ctx, 1, metric.WithAttributes(attribute.String("event_type", eventType)))
}

// registerSocketGauge reports each Socket Mode connection m runs as
// slack.socket.connections: 1 for the connection, labelled with its app,
// whether that app is the deployment's, and its state. The connections run
// on the pod holding the background brain and are stopped on demotion, so
// one pod reports them. An instrument error can only be a programmer error,
// and is logged.
func registerSocketGauge(mp metric.MeterProvider, m *socketManager) {
	meter := mp.Meter("github.com/sky-ai-eng/triage-factory/ee/slack")
	gauge, err := meter.Int64ObservableGauge("slack.socket.connections",
		metric.WithDescription("Socket Mode connections, by app, by whether the app is the deployment's, and by state (dialing, open, draining, backing_off, auth_failed); 1 for each connection. Reported by the pod holding the background brain."))
	if err != nil {
		slackLog.Warn("socket connections gauge setup failed", "error", err)
		return
	}
	if _, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		m.mu.Lock()
		conns := make([]*appConnection, 0, len(m.conns))
		for _, c := range m.conns {
			conns = append(conns, c)
		}
		m.mu.Unlock()
		for _, c := range conns {
			o.ObserveInt64(gauge, 1, metric.WithAttributes(
				attribute.String("app_id", c.appID),
				attribute.Bool("deployment_app", c.deployment),
				attribute.String("state", c.snapshot().State),
			))
		}
		return nil
	}, gauge); err != nil {
		slackLog.Warn("socket connections gauge callback registration failed", "error", err)
	}
}

// The reasons slack.deployment_app.up reports with 0.
const (
	// deploymentAppMisconfigured is a set of TF_SLACK_* variables short of a
	// complete one, so the deployment runs without an app.
	deploymentAppMisconfigured = "misconfigured"
	// deploymentAppInsecureURL is a configured app behind a public URL that
	// is not HTTPS, which Slack refuses as a redirect URL.
	deploymentAppInsecureURL = "insecure_public_url"
)

// registerDeploymentAppGauge reports whether the deployment's Slack app can
// be installed, as slack.deployment_app.up: 1 when it is configured behind an
// HTTPS public URL, 0 with a reason when the operator set it up and it cannot
// be, and no series when no TF_SLACK_* variable is set. misconfigured is
// whether reading the variables failed. Each pod reports the configuration it
// read, so a pod configured differently from the others shows as its own
// series.
func registerDeploymentAppGauge(mp metric.MeterProvider, app deploymentApp, misconfigured bool, publicURL func() string) {
	if !app.Configured() && !misconfigured {
		return
	}
	meter := mp.Meter("github.com/sky-ai-eng/triage-factory/ee/slack")
	gauge, err := meter.Int64ObservableGauge("slack.deployment_app.up",
		metric.WithDescription("Whether the deployment Slack app can be installed: 1 when it is configured behind an HTTPS public URL; 0, with a reason (misconfigured, insecure_public_url), when it is set up and cannot be. No series without a deployment app."))
	if err != nil {
		slackLog.Warn("deployment app gauge setup failed", "error", err)
		return
	}
	if _, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		switch {
		case misconfigured:
			o.ObserveInt64(gauge, 0, metric.WithAttributes(attribute.String("reason", deploymentAppMisconfigured)))
		case !installablePublicURL(publicURL()):
			o.ObserveInt64(gauge, 0, metric.WithAttributes(
				attribute.String("app_id", app.AppID),
				attribute.String("reason", deploymentAppInsecureURL),
			))
		default:
			o.ObserveInt64(gauge, 1, metric.WithAttributes(attribute.String("app_id", app.AppID)))
		}
		return nil
	}, gauge); err != nil {
		slackLog.Warn("deployment app gauge callback registration failed", "error", err)
	}
}
