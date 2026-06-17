package projectclassify

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component logger for the projectclassify package (see internal/logging).
// "classify" covers the two-stage entity→project classification loop.
var classifyLog = logging.Component("classify")
