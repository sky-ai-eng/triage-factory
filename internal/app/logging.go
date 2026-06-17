package app

import "github.com/sky-ai-eng/triage-factory/internal/logging"

// Component loggers for the app package (see internal/logging). Each tags its
// records with component=<name>, the structured replacement for the old
// "[prefix]" log tags. One var per distinct component the package emits under:
//
//	ai             scorer-manager construction
//	classify       project-classifier construction + start
//	curator        stranded-turn sweep at boot
//	app            DB-pool teardown + run-credential/model resolution
//	poll-tracker   poll-completion sentinel handling
//	auth           JWKS verifier readiness (multi mode)
//	server         config-change reload / restart lifecycle
//	bootstrap      local-tenant skill import gating
//	clone-status   worktree clone-result callback (local)
//	kbwatcher      knowledge-base file watcher
//	worktree       bare-clone bootstrap
//	delegate       spawner readiness + App-registration read at boot
//	repoprofile    local profile→restart→score sequence
//	sandbox        orphan-sandbox reap at boot (multi mode)
var (
	aiLog          = logging.Component("ai")
	classifyLog    = logging.Component("classify")
	curatorLog     = logging.Component("curator")
	appLog         = logging.Component("app")
	pollTrackerLog = logging.Component("poll-tracker")
	authLog        = logging.Component("auth")
	serverLog      = logging.Component("server")
	bootstrapLog   = logging.Component("bootstrap")
	cloneStatusLog = logging.Component("clone-status")
	kbwatcherLog   = logging.Component("kbwatcher")
	worktreeLog    = logging.Component("worktree")
	delegateLog    = logging.Component("delegate")
	repoprofileLog = logging.Component("repoprofile")
	sandboxLog     = logging.Component("sandbox")
)
