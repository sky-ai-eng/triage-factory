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
	"github.com/sky-ai-eng/triage-factory/internal/routing"
	"github.com/sky-ai-eng/triage-factory/internal/server"

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

// install mounts the org-admin Slack workspace surface plus both ingest
// transports on the core server through the extension API. The workspace
// routes go through API/APIMutating (session + CSRF, like core's own
// routes); the webhook is pre-auth (Slack has no session) so it mounts
// through Raw + PreAuthRateLimit and authorizes itself per-request via the
// workspace's signing secret. The Socket Mode connection manager has no
// HTTP surface of its own — it starts via api.OnReady once app wiring is
// complete and its status is read out through workspacesHandler.sockets.
func install(api server.ExtensionAPI) {
	stores := api.Stores()

	bundle := slackstore.FromStores(stores)
	routing.RegisterSource("slack", routing.SourceHooks{
		ResolveOwner: slackChannelOwner(bundle),
		TracksScope:  slackTeamTracksChannel(bundle),
	})

	pipeline := &ingestPipeline{
		entities:    stores.Entities,
		deliveries:  bundle.Deliveries,
		channels:    bundle.Channels,
		publish:     api.PublishEvent,
		identity:    NewIdentityResolver(stores),
		channelName: NewChannelResolver(stores),
		permalink:   NewPermalinkResolver(stores),
	}
	sockets := newSocketManager(stores, pipeline, slackHTTPClient, socketConfigChanged)

	h := &workspacesHandler{
		tx:        api.Tx(),
		az:        api.Authz(),
		client:    slackHTTPClient,
		publicURL: api.PublicURL,
		sockets:   sockets,
	}
	api.API("GET /api/slack/workspaces", h.handleList)
	api.APIMutating("POST /api/slack/workspaces", h.handleConnect)
	api.APIMutating("DELETE /api/slack/workspaces/{workspace_id}/{api_app_id}", h.handleDelete)
	api.API("GET /api/slack/manifest", h.handleManifest)

	ch := &channelsHandler{
		tx:     api.Tx(),
		stores: stores,
		az:     api.Authz(),
		client: slackHTTPClient,
	}
	api.API("GET /api/slack/teams/{team_id}/channels", ch.handleList)
	api.APIMutating("PUT /api/slack/teams/{team_id}/channels", ch.handlePut)
	api.APIMutating("POST /api/slack/channels/{channel_id}/primary", ch.handlePrimary)

	wh := &webhookHandler{stores: stores, pipeline: pipeline}
	api.Raw("POST /api/webhooks/slack/{org_id}", api.PreAuthRateLimit(http.HandlerFunc(wh.handleWebhook)))

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
	api.OnReady(lifecycle.run)

	api.OnReady(sockets.run)
}
