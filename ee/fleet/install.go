// Package fleet is the Enterprise-Edition sandbox-fleet administration console
// — the Fleet page's rich backend (TFAC-589, spec §8). It reads the core
// operability substrate (the instances registry, the instance_stats telemetry
// samples, the runs timing columns, and llm_spend) and serves it as the
// operator-gated /api/fleet/* surfaces.
//
// EE module shape (docs/ee-feature-packaging.md): core never imports this
// package; package main blank-imports it, whose init() registers the route
// installer. Every handler gates per-request — but on the DEPLOYMENT-scoped
// gate, not the per-org one: fleet administration is a cross-org console, so it
// composes is_operator (the deployment-operator identity) AND
// entitlements.Active().Has(FeatureFleet) (the deployment license), never
// For(orgID) which cannot name an org for a cross-org view. Both failures
// 404-and-hide (non-disclosure), matching core's posture.
//
// Operability itself is core and ungated (the registry read, drain via the
// `instance` CLI, the operator CLI) — only this console is EE.
package fleet

import (
	"github.com/sky-ai-eng/triage-factory/internal/server"
)

// init registers the fleet route installer. It runs when package main
// blank-imports ee/fleet; installExtensions() invokes install during routes()
// unconditionally — install's handlers gate per-request on operator identity +
// the FeatureFleet deployment entitlement at their own seam (see gate.go).
func init() {
	server.RegisterExtension("fleet", install)
}

// install mounts the operator-gated fleet console surfaces on the core server
// through the extension API, with the same withSession/CSRF discipline as
// core's own routes. Reads go through api.API (session-wrapped GETs); the drain
// control is a mutating POST (CSRF-guarded).
func install(api server.ExtensionAPI) {
	h := &handler{stores: api.Stores()}
	api.API("GET /api/fleet/overview", h.handleOverview)
	api.API("GET /api/fleet/instances", h.handleInstances)
	api.APIMutating("POST /api/fleet/instances/{id}/drain", h.handleDrain)
	api.API("GET /api/fleet/timeseries", h.handleTimeseries)
	// The per-executor sandbox breakdown and one sandbox's in-run series —
	// the operator lens whole-host telemetry can't provide (instance_stats
	// samples the box, these read per-run cgroups). Both are cross-org reads
	// authorized by the same operator + FeatureFleet gate as their siblings.
	api.API("GET /api/fleet/instances/{id}/sandboxes", h.handleInstanceSandboxes)
	api.API("GET /api/fleet/claims/{id}/series", h.handleClaimSeries)
	// The operator backlog (fleet-wide oldest-waiting + per-org shares) is a
	// distinct surface from core's org-facing GET /api/fleet/queue (the per-org
	// cap read-out, internal/server/fleet_queue.go) — hence /api/fleet/backlog,
	// not /api/fleet/queue.
	api.API("GET /api/fleet/backlog", h.handleBacklog)
}
