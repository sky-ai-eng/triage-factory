package app

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the app package (see internal/logging). Each tags its
// records with component=<name>, the structured replacement for the old
// "[prefix]" log tags. One var per distinct component the package emits under:
//
//	ai             scorer-manager construction
//	app            DB-pool teardown + run-credential/model resolution
//	poll-tracker   poll-completion sentinel handling
//	auth           JWKS verifier readiness (multi mode)
//	server         config-change reload / restart lifecycle
//	bootstrap      local-tenant skill import gating
//	clone-status   worktree clone-result callback (local)
//	worktree       bare-clone bootstrap
//	repoprofile    repo-profiling manager construction
//	sandbox        orphan-sandbox reap at boot (multi mode)
//	reconcile      artifact-reconciler manager construction + poll gating
//	reachcache     reachable-repo cache manager construction
var (
	aiLog          = logging.Component("ai")
	appLog         = logging.Component("app")
	pollTrackerLog = logging.Component("poll-tracker")
	authLog        = logging.Component("auth/app")
	serverLog      = logging.Component("server")
	bootstrapLog   = logging.Component("bootstrap")
	cloneStatusLog = logging.Component("clone-status")
	worktreeLog    = logging.Component("worktree")
	repoprofileLog = logging.Component("repoprofile")
	sandboxLog     = logging.Component("sandbox")
	reconcileLog   = logging.Component("reconcile")
	reachLog       = logging.Component("reachcache")
)
