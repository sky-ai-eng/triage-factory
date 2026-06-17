package postgres

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the postgres store (see internal/logging).
var (
	dashboardLog   = logging.Component("dashboard")
	dbPgLog        = logging.Component("db/pg")
	promptStatsLog = logging.Component("prompt_stats")
	memoryLog      = logging.Component("memory")
)
