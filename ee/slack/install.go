// Package slack is the Enterprise Edition Slack workspace-connect surface:
// the org-admin connect/disconnect/manifest HTTP handlers, mounted into the
// core server through the route-extension seam and gated on the `slack`
// entitlement. Core holds zero Slack symbols: store access goes through
// ee/slack/store's typed view of core's opaque extension slot, and HTTP
// helpers come from the shared httpx package — mirrors ee/sso.
//
// This leaf adds event ingest: the transport-neutral pipeline (ingest.go),
// the Events API (webhook) receiver (webhook.go), and the Socket Mode
// receiver (socket.go/socket_conn.go) — both transports publish slack:*
// events onto the bus. install registers routing.RegisterSource("slack",
// ...) (routing.go, TFAC-542), which flips routing.RouterBound("slack:")
// true: a mention is now durably enqueued and routed through the owner
// ladder, resolving to its channel's primary tracking team (or, absent one,
// recorded taskless and visible only to applies_to_unowned watchers). The
// pipeline also dispatches best-effort sender identity capture (identity.go,
// TFAC-531), channel-name resolution (channels.go, TFAC-542), and — on
// entity creation only — thread permalink resolution (permalink.go,
// TFAC-595) after each publish — all detached, never gating.
//
// Enterprise Edition — governed by the repository-root LICENSE (Triage Factory
// License 1.0); enabling its features requires a valid license key.
package slack

import (
	"net/http"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
	"github.com/sky-ai-eng/triage-factory/internal/domain"
	"github.com/sky-ai-eng/triage-factory/internal/domain/events"
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/server"
	"github.com/sky-ai-eng/triage-factory/internal/server/httpx"
	"github.com/sky-ai-eng/triage-factory/internal/ttlcache"
	"go.opentelemetry.io/otel"

	// Link the Slack store factories (postgres + sqlite) so the Slack
	// bundle is built into every TxStores / Stores once this package is in
	// the binary.
	_ "github.com/sky-ai-eng/triage-factory/ee/slack/store/lite"
	_ "github.com/sky-ai-eng/triage-factory/ee/slack/store/pg"
)

// init registers the Slack route installer. It runs when package main
// blank-imports ee/slack; installExtensions() invokes install during
// routes() unconditionally — install's own handlers gate per-request on the
// `slack` entitlement at their own org-resolution seam (see
// workspacesHandler.memberGate / adminGate, webhookHandler.handleWebhook).
func init() {
	server.RegisterExtension("slack", install)
}

// install mounts the Slack connection, install and channel surfaces plus both
// ingest transports on the core server through the extension API
// (mountRoutes). The connection and channel routes go through
// API/APIMutating (session + CSRF, like core's own routes); the two webhook
// routes are pre-auth (Slack has no session) so they mount through Raw +
// SignedWebhookRateLimit and authorize themselves per-request: an org's own
// app's by the workspace's signing secret, the deployment app's by the one in
// the environment. It deliberately does NOT use
// PreAuthRateLimit: that tier's 1 req/s budget is sized for anonymous human
// login flows, and a subscribed workspace's own delivery volume routinely
// clears it — the signing-secret check downstream already makes the
// anti-recon rationale for a tight budget moot, so sharing that tier would
// only 429 legitimate deliveries (Slack retries, amplifying the flood,
// until it disables the app's event subscription entirely). The Socket
// Mode connection manager has no HTTP surface of its own — it starts via
// api.OnReady once app wiring is complete and its status is read out
// through workspacesHandler.sockets.
func install(api server.ExtensionAPI) {
	stores := api.Stores()

	bundle := slackstore.FromStores(stores)
	routing.RegisterSource("slack", routing.SourceHooks{
		ResolveOwner: slackChannelOwner(bundle),
		TracksScope:  slackTeamTracksChannel(bundle),
	})
	events.RegisterCurrentView(domain.EventSlackMessage, slackMessageCurrentView(bundle.Channels))

	// One stats instance spans the pipeline and both transports: outcome
	// counts land in the shared pipeline; retry / rate-limit signals are
	// transport-specific and recorded at their receivers. Instruments come
	// off the global provider — app boot installs the real SDK (or leaves
	// the no-op) before routes() runs this installer.
	stats := newIngestStats(otel.GetMeterProvider())
	pipeline := &ingestPipeline{
		tx:          api.Tx(),
		connections: bundle.Connections,
		secrets:     stores.Secrets,
		deliveries:  bundle.Deliveries,
		channels:    bundle.Channels,
		publish:     api.PublishPreEnqueued,
		identity:    NewIdentityResolver(stores),
		channelName: NewChannelResolver(stores),
		permalink:   NewPermalinkResolver(stores),
		title:       NewTitleResolver(stores),
		stats:       stats,
	}

	// Read once and handed to every handler that needs it, so no two of them
	// can disagree about it. A misconfigured app is logged and the deployment
	// runs without one, as with the deployment GitHub and Linear apps: a
	// deployment whose orgs bring their own apps must still start, and no
	// deployment app fails closed.
	deployment, err := deploymentAppFromEnv()
	if err != nil {
		slackLog.Error("read the deployment Slack app from the environment failed; the deployment runs without one", "error", err)
		deployment = deploymentApp{}
	}
	registerDeploymentAppGauge(otel.GetMeterProvider(), deployment, err != nil, api.PublicURL)

	sockets := newSocketManager(stores, pipeline, slackHTTPClient, socketConfigChanged)
	sockets.stats = stats
	sockets.deployment = deployment
	registerSocketGauge(otel.GetMeterProvider(), sockets)

	ch := &channelsHandler{
		tx:         api.Tx(),
		stores:     stores,
		az:         api.Authz(),
		client:     slackHTTPClient,
		candidates: ttlcache.New[slackCandidateSet](slackCandidatesTTL),
	}
	h := &workspacesHandler{
		tx:         api.Tx(),
		az:         api.Authz(),
		client:     slackHTTPClient,
		publicURL:  api.PublicURL,
		sockets:    sockets,
		deployment: deployment,
		stores:     stores,
		channels:   ch,
	}
	dh := &deploymentAppHandler{app: deployment, publicURL: api.PublicURL}
	ih := &installHandler{tx: api.Tx(), az: api.Authz(), client: slackHTTPClient, app: deployment, ceremony: api, stats: stats}
	wh := &webhookHandler{stores: stores, pipeline: pipeline, stats: stats, deployment: deployment}
	mountRoutes(api, h, dh, ih, ch, wh)

	// Agent-facing exec verbs (TFAC-596) register from this package's init()
	// (stores-free — the handler relays for policy and selects its token from
	// the sealed bundle), so they are present in the sidecar process too. See
	// exec_host.go's package doc.

	// Lifecycle adapters (TFAC-597): 👀 acknowledge, the live setStatus
	// driver, the failure note, and the no-match reply — everything the bot
	// does in Slack without the agent asking. api.Bus is passed as an
	// accessor (not called here), same read-through-at-hook-time posture as
	// api.PublicURL: ExtensionAPI.Bus() is nil until app wiring completes,
	// and OnReady hooks are the seam that fires after it does (see
	// lifecycle.go's package doc).
	lifecycle := newLifecycleAdapter(stores, slackHTTPClient, api.PublicURL, api.Bus)
	lifecycle.stats = stats
	api.OnReady(lifecycle.run)

	api.OnReady(sockets.run)
}

// routeMounter is the part of server.ExtensionAPI the route table mounts
// through.
type routeMounter interface {
	API(pattern string, h http.HandlerFunc)
	APIMutating(pattern string, h http.HandlerFunc)
	Raw(pattern string, h http.Handler)
	Op(op httpx.Operation, h http.HandlerFunc)
	RawOp(op httpx.Operation, h http.Handler)
	SignedWebhookRateLimit(h http.Handler) http.Handler
}

// mountRoutes is the Slack route table. An org's connections, manifest and
// install start are addressed by the org in the path; a team's channels are
// the team's resource, under its own path, with the org the credential's as on
// every core team route. The list and facets reads mount through APIMutating
// because they are POSTs, and CSRF follows the method.
func mountRoutes(api routeMounter, h *workspacesHandler, dh *deploymentAppHandler, ih *installHandler, ch *channelsHandler, wh *webhookHandler) {
	api.API("GET /api/orgs/{org_id}/slack/manifest", h.handleManifest)
	api.APIMutating("POST /api/orgs/{org_id}/slack/connections/import", h.handleConnect)
	api.Op(opList, h.handleList)
	api.API("GET /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}", h.handleGet)
	api.APIMutating("DELETE /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}", h.handleDelete)
	api.API("GET /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}/activation-preview", h.handleActivationPreview)
	api.APIMutating("POST /api/orgs/{org_id}/slack/connections/{workspace_id}/{api_app_id}/activate", h.handleActivate)

	api.API("GET /api/slack/deployment-app", dh.handleGet)

	// The deployment app's install ceremony (deployment_install.go). Both legs
	// are top-level navigations, the start from Settings and the callback from
	// Slack's redirect, so they mount through API (session, no CSRF wrap) and
	// carry their own state-cookie CSRF defense. The callback path is the
	// redirect URL registered on the deployment's Slack app, so renaming it
	// breaks every install.
	api.API("GET /api/orgs/{org_id}/slack/install/start", ih.handleStart)
	api.API("GET "+installCallbackPath, ih.handleCallback)

	api.APIMutating("PUT /api/teams/{team_id}/slack-channels", ch.handlePut)
	api.APIMutating("POST /api/teams/{team_id}/slack-channels/list", ch.handleList)
	api.APIMutating("POST /api/teams/{team_id}/slack-channels/facets", ch.handleFacets)
	api.API("GET /api/teams/{team_id}/slack-channels/{channel_id}", ch.handleGet)
	api.APIMutating("PATCH /api/teams/{team_id}/slack-channels/{channel_id}", ch.handlePatch)

	api.Raw("POST /api/webhooks/slack/{org_id}", api.SignedWebhookRateLimit(http.HandlerFunc(wh.handleWebhook)))
	api.Raw("POST /api/webhooks/slack", api.SignedWebhookRateLimit(http.HandlerFunc(wh.handleDeploymentWebhook)))
}
