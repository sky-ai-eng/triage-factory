package curator

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the curator package (see internal/logging). "curator"
// covers the session lifecycle, dispatch, and pinned-repo/skill
// materialization; "kbwatcher" covers the knowledge-base filesystem watcher.
var (
	curatorLog   = logging.Component("curator")
	kbwatcherLog = logging.Component("kbwatcher")
	kbsyncLog    = logging.Component("kbsync")
)
