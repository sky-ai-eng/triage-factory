package sqlite

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the sqlite store (see internal/logging).
var (
	dashboardLog   = logging.Component("dashboard")
	dbLog          = logging.Component("db")
	promptStatsLog = logging.Component("prompt_stats")
	memoryLog      = logging.Component("memory")
)
