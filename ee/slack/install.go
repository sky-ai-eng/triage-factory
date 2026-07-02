// Package slack is the Enterprise Edition Slack workspace-connect surface:
// the org-admin connect/disconnect/manifest HTTP handlers, mounted into the
// core server through the route-extension seam and gated on the `slack`
// entitlement. Core holds zero Slack symbols: store access goes through
// ee/slack/store's typed view of core's opaque extension slot, and HTTP
// helpers come from the shared httpx package — mirrors ee/sso.
//
// This leaf covers connect only: schema, AEAD credential storage, transport
// selection, the app-manifest connect UX, and the settings card. No event
// ingest yet (no webhook receiver, no socket connection, no slack:* event
// types) — those are follow-on leaves that build on the workspace rows this
// one creates.
//
// Licensed under the Enterprise Edition License (see ee/LICENSE), not the
// repository-root BSL.
package slack

import (
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
// workspacesHandler.memberGate / adminGate).
func init() {
	server.RegisterExtension("slack", install)
}

// install mounts the org-admin Slack workspace surface on the core server
// through the extension API, with the same withSession/CSRF discipline and
// authorization as core's own routes.
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
}
