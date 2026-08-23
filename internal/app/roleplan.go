package app

import "github.com/sky-ai-eng/triage-factory/internal/runmode"

// subsystemPlan is the exclusion-based inventory of which subsystem groups
// this process's deployment role (runmode.Role) starts. It is computed once
// in New from the resolved role; every boot/worker branch reads it rather
// than re-deriving role predicates ad hoc.
//
// The design is deliberately exclusion-based (spec §0 / the ticket's
// "verified by test" decision): rather than hand-list what an executor
// starts, the wiring asserts what it must NOT — so a new subsystem added to
// app.Run later defaults onto the brain/HTTP side, and the executor
// exclusion test (roleplan_test.go) fails if it leaks onto executors. This
// is the guard against the "every replica runs every background job" hazard
// re-growing.
//
// Two subsystem groups are deliberately NOT represented here because they
// are gated on something other than role alone:
//
//   - The cap-broker + sandbox substrate is gated on agentproc.WillSandbox()
//     (multi + Linux) AND on the role hosting sandboxes at all: an executor
//     sandboxes delegated runs and an all-in-one box sandboxes both, but a
//     control pod never launches a sandbox — sandboxed work homes to
//     executors and the brain's own LLM work is toolless direct API calls,
//     so nothing on control needs a jail. Control therefore starts no
//     broker and installs no privileged ops, and a stray sandbox launch
//     that somehow reached it fails loudly at construction rather than
//     running unjailed. The gate lives in internal/app/privsep.go, not this
//     plan, because it also depends on mode+GOOS (WillSandbox), which a
//     role predicate alone can't express.
//   - The instance registry (Register + heartbeat), the worktree cache
//     reaper, git-hooks materialization, and orphaned-worktree cleanup run
//     in every role: registry membership is fleet-wide, and every role
//     keeps a per-pod worktree cache under its own TF_STATE_ROOT — run
//     worktrees on an executor (or, at role=all, on the single local-mode
//     process). A multi-mode control pod never writes into its own copy —
//     the dispatcher that materializes run worktrees is executor/all-only
//     (see the dispatcher field below) — so the reaper there sweeps an
//     empty directory; it still runs uniformly rather than special-cased
//     per role, since a no-op sweep costs nothing and keeps the eviction
//     path exercised in local dev.
type subsystemPlan struct {
	role runmode.DeployRole

	// serveHTTP starts the user-facing HTTP/API/WS server on the main port,
	// plus everything that only exists alongside it: auth wiring, the
	// embedded SPA, server extension workers, the dashboard backfiller, and
	// the spawner/reconciler server handles. control + all. An executor
	// serves no user routes (its only listener is the localhost healthz).
	serveHTTP bool

	// brain marks this role as BRAIN-CAPABLE: control + all construct the
	// leader-elected background brain's objects (pollers + tracker, the
	// event router, the AI managers — scorer/profiler/
	// reconciler/marketplace-stats —, the
	// poll-completion bus subscribers) and participate in the
	// background-brain lease election. An executor never does.
	//
	// This does NOT mean the brain is currently running on this process —
	// that's dynamic, driven by lease-holder state (TFAC-583,
	// internal/app/brain.go): at role=all (local-only — multi rejects all
	// at boot) the single process always self-holds and starts the brain
	// once at boot, unconditionally; at role=control
	// in multi mode, the brain starts only while this pod actually holds
	// the "background-brain" lease, and stops on demotion. A standby
	// control pod still builds every brain object (buildAI/buildRouting) —
	// config-save handlers need them to relay Trigger/PollSoon calls to
	// whichever pod IS the holder — it just never starts their background
	// loops.
	brain bool

	// dispatcher starts the delegated-run dispatcher (claims + executes
	// queued runs) and the workspace-snapshot retention reaper. executor +
	// all. A control pod never claims or executes delegated runs — that
	// absence is the executor/control split.
	dispatcher bool

	// executorHealthz starts the localhost-only executor healthz listener.
	// executor only: control + all already expose /api/health and /readyz
	// on the main HTTP port, so a second listener would be redundant there.
	executorHealthz bool
}

// planForRole resolves the subsystem inventory for a deployment role.
//
// The derivations, stated positively for the reader:
//   - all      = everything — local mode's single-process shape (the only
//     place all still boots; multi rejects it at role resolution).
//   - control  = serve HTTP + brain, but NO run dispatcher.
//   - executor = run dispatcher + healthz, but NO HTTP, NO brain.
func planForRole(role runmode.DeployRole) subsystemPlan {
	return subsystemPlan{
		role:            role,
		serveHTTP:       role == runmode.RoleAll || role == runmode.RoleControl,
		brain:           role == runmode.RoleAll || role == runmode.RoleControl,
		dispatcher:      role == runmode.RoleAll || role == runmode.RoleExecutor,
		executorHealthz: role == runmode.RoleExecutor,
	}
}
