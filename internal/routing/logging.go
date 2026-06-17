package routing

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the routing package (see internal/logging). "router"
// covers event ingestion, task creation/dedup, auto-delegation, and the
// durable event-queue worker; "lifecycle" covers entity-close cascades.
var (
	routerLog    = logging.Component("router")
	lifecycleLog = logging.Component("lifecycle")
)
