package worktree

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component logger for the worktree package (see internal/logging). Covers
// the bare-clone cache, worktree materialization, bootstrap warming, and
// startup cleanup.
var worktreeLog = logging.Component("worktree")
