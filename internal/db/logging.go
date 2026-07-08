package db

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component logger for the internal/db package (see internal/logging).
// Scoped narrowly to migration coordination for now — most of this
// package is plumbing whose callers already log; migrations.go is the
// first spot here with a genuinely best-effort warning to surface.
var migrateLog = logging.Component("db/migrate")
