// Package slack is the Enterprise Edition Slack workspace-connect surface:
// the org-admin connect/disconnect/manifest HTTP handlers, mounted into the
// core server through the route-extension seam and gated on the `slack`
// entitlement. Core holds zero Slack symbols: store access goes through
// ee/slack/store's typed view of core's opaque extension slot, and HTTP
// helpers come from the shared httpx package — mirrors ee/sso.
//
// This leaf adds event ingest: the transport-neutral pipeline (ingest.go)
// and the Events API (webhook) receiver (webhook.go), publishing slack:*
// events onto the bus. Until TFAC-510 registers routing.RegisterSource,
// published events reach the in-memory bus only — routing.RouterBound
// stays false for "slack:", so nothing is durably enqueued or routed yet.
// The Socket Mode transport (feeding the same pipeline) is the next leaf.
// The pipeline also dispatches best-effort sender identity capture
// (identity.go, TFAC-531) after each publish — detached, never gating.
//
// Enterprise Edition — governed by the repository-root LICENSE (Triage Factory
// License 1.0); enabling its features requires a valid license key.
package slack

import (
	"net/http"

	slackstore "github.com/sky-ai-eng/triage-factory/ee/slack/store"
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

// install mounts the org-admin Slack workspace surface plus the Events API
// ingest receiver on the core server through the extension API. The
// workspace routes go through API/APIMutating (session + CSRF, like core's
// own routes); the webhook is pre-auth (Slack has no session) so it mounts
// through Raw + PreAuthRateLimit and authorizes itself per-request via the
// workspace's signing secret.
func install(api server.ExtensionAPI) {
	h := &workspacesHandler{
		tx:        api.Tx(),
		az:        api.Authz(),
		client:    slackHTTPClient,
		publicURL: api.PublicURL,
	}
	api.API("GET /api/slack/workspaces", h.handleList)
	api.APIMutating("POST /api/slack/workspaces", h.handleConnect)
	api.APIMutating("DELETE /api/slack/workspaces/{workspace_id}", h.handleDelete)
	api.API("GET /api/slack/manifest", h.handleManifest)

	stores := api.Stores()
	wh := &webhookHandler{
		stores: stores,
		pipeline: &ingestPipeline{
			entities:   stores.Entities,
			deliveries: slackstore.FromStores(stores).Deliveries,
			publish:    api.PublishEvent,
			identity:   NewIdentityResolver(stores),
		},
	}
	api.Raw("POST /api/webhooks/slack/{org_id}", api.PreAuthRateLimit(http.HandlerFunc(wh.handleWebhook)))
}
