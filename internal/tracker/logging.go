package tracker

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component logger for the tracker package (see internal/logging). Covers the
// GitHub + Jira discover/refresh/diff/emit cycle and reviewer resolution.
var trackerLog = logging.Component("tracker")
