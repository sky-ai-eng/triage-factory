package reachcache

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component logger for the reachcache package (see internal/logging).
// "reachcache" covers the TTL-gated refresh of the reachable-repo mirror on
// both credential tiers.
var reachLog = logging.Component("reachcache")
