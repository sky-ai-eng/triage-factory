package repoprofile

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component logger for the repoprofile package (see internal/logging).
// "repoprofile" covers the AI repo-profiling run (doc fetch + model
// summarization + persistence).
var repoprofileLog = logging.Component("repoprofile")
