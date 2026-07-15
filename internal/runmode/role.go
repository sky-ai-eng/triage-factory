package runmode

// Deployment role — TF_ROLE — is the horizontal-scaling companion to
// TF_MODE (see the package doc in runmode.go). It splits the single
// process into cooperating pods:
//
//   - all      the single-process shape: HTTP+WS, pollers, event
//              drainer/router, AI managers, curator, run dispatcher,
//              sweepers — one box runs everything. LOCAL-ONLY: it is what
//              local mode always is (see below), and it is rejected at
//              boot in multi mode, where the deployment is always a
//              control+executor split.
//   - control  the API + brain half: HTTP/API/WS, migrations, the
//              leader-elected background brain, curator chat sandboxes.
//              Does NOT run the delegated-run dispatcher.
//   - executor a shared-nothing sandbox worker: the run dispatcher,
//              sandboxes, per-run proxies, and the worktree/snapshot
//              reapers against its own TF_STATE_ROOT. No user HTTP, no
//              pollers, no router, no AI managers, no migrations (it
//              asserts the schema instead — see internal/db).
//
// Read once at startup from TF_ROLE via InitRoleFromEnv(), called in
// main() right after InitFromEnv() and before the argv-dispatch switch,
// so every consumer — including the `migrate` subcommand — sees the
// resolved role. This replaces internal/db/migrations.go's env-per-call
// isExecutorRole() placeholder: the migration gate now reads Role().
//
// Three boot-safety rules, all deliberate:
//
//   - An invalid TF_ROLE value fails boot loudly (InitRoleFromEnv returns
//     an error) rather than defaulting to all. A typo'd TF_ROLE=exectuor
//     silently running a full stack on a sandbox host is exactly the
//     misconfiguration this flag exists to prevent.
//   - Local mode forces all. Local is structurally single-process (SQLite,
//     N=1, never sandboxes), so a control/executor split is meaningless
//     there; rather than brick a laptop over a stray env var, we log a
//     warning and run as all.
//   - Multi mode rejects all (and unset, which parses to all). Multi is
//     always the control+executor split: the credential-isolation
//     boundary hangs on the MODE (multi ⇒ every run's credentials live
//     only in its per-run sidecar), not on a deployment knob an operator
//     could forget. A fused single process would quietly recouple the
//     secret store with the sandbox host, so it is not a deployable
//     shape — boot fails with a pointer at the split blueprint.

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// DeployRole names the deployment role this process plays within a fleet.
// Set once at startup and never reassigned outside tests. The accessor is
// Role() (the type is DeployRole to avoid the name collision).
type DeployRole string

const (
	// RoleAll is the single-process role: every subsystem in one binary.
	// Local-only — it is the only role local mode ever runs as (and what
	// an unset TF_ROLE parses to), but multi mode refuses to boot with it
	// (InitRole): multi is always the control+executor split.
	RoleAll DeployRole = "all"

	// RoleControl is the API + background-brain role. Serves user HTTP/WS,
	// runs migrations, hosts the leader-elected brain, sandboxes curator
	// chats — but never claims or executes delegated runs.
	RoleControl DeployRole = "control"

	// RoleExecutor is the shared-nothing sandbox-worker role: it drains the
	// run queue and executes delegated runs, and nothing else. Multi-mode
	// only (local forces all).
	RoleExecutor DeployRole = "executor"
)

// currentRole + roleInitialized + roleMu mirror the mode state above.
// Reads through Role() take an RLock so SetRoleForTest is race-free
// against them.
//
// roleInitialized distinguishes "boot resolved the role" from "nobody
// has called InitRoleFromEnv yet". Production always initializes at
// boot; a bare test that never inits falls back to a live env parse in
// Role() so a test's t.Setenv("TF_ROLE", ...) is honored without every
// such test having to call SetRoleForTest — this is the reset hook the
// internal/db migration-gate tests lean on.
var (
	currentRole     DeployRole = RoleAll
	roleInitialized bool
	roleMu          sync.RWMutex
)

// Role returns the active deployment role. Safe from any goroutine.
//
// When the role has not been initialized (a bare test that never called
// InitRoleFromEnv), Role falls back to parsing TF_ROLE live — WITHOUT
// the local-forces-all coupling InitRoleFromEnv applies, because that
// coupling is a boot-policy decision, not the raw role. An unparseable
// value degrades to RoleAll in that fallback (the safe default); boot
// itself surfaces the parse error loudly via InitRoleFromEnv. Production
// initializes the role as the second line of main(), so this branch is
// test-only in practice.
func Role() DeployRole {
	roleMu.RLock()
	initialized := roleInitialized
	r := currentRole
	roleMu.RUnlock()
	if initialized {
		return r
	}
	parsed, err := ParseRole(os.Getenv("TF_ROLE"))
	if err != nil {
		return RoleAll
	}
	return parsed
}

// ParseRole parses a TF_ROLE env-var string into a Role. Empty maps to
// RoleAll (the safe default); known values map to their constants;
// anything else errors so a typo fails boot loudly. Case-insensitive and
// whitespace-trimmed, mirroring ModeFromEnv's conventions.
//
// ParseRole is the RAW parse — it does NOT apply the local-forces-all
// policy. InitRoleFromEnv layers that on once it knows the mode.
func ParseRole(s string) (DeployRole, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return RoleAll, nil
	case "all":
		return RoleAll, nil
	case "control":
		return RoleControl, nil
	case "executor":
		return RoleExecutor, nil
	default:
		return "", fmt.Errorf("unknown TF_ROLE=%q (want %q, %q, or %q, or empty for %q)", s, RoleAll, RoleControl, RoleExecutor, RoleAll)
	}
}

// InitRole sets the process-wide role, applying the mode policies:
// local forces all; multi rejects all. Same idempotency contract as
// Init: first call sets state and returns nil; a subsequent call with
// the same resolved role is a no-op; a subsequent call with a different
// role errors without mutating state.
//
// The local-forces-all coercion is silent here — the caller
// (InitRoleFromEnv) surfaces whether it happened so main() can log it,
// keeping this function free of a logging dependency.
func InitRole(requested DeployRole) error {
	if requested != RoleAll && requested != RoleControl && requested != RoleExecutor {
		return fmt.Errorf("unknown role %q (want %q, %q, or %q)", requested, RoleAll, RoleControl, RoleExecutor)
	}
	resolved := requested
	// Local mode is structurally single-process — coerce any split role to
	// all. The caller logs the coercion; here we just resolve it.
	if Current() == ModeLocal {
		resolved = RoleAll
	}
	// Multi mode is always the control+executor split — a fused single
	// process would hold the secret store and host sandboxes at once,
	// which is exactly the shape the split exists to make undeployable.
	// Unset TF_ROLE parses to all, so this also catches "forgot to set
	// it". Boot fails here, legibly, before anything opens a DB.
	if Current() == ModeMulti && resolved == RoleAll {
		return fmt.Errorf("TF_ROLE=all (or unset) is not valid in multi mode: multi always runs as a control+executor split, so every process — including CLI subcommands like `migrate` — must set TF_ROLE=%q or TF_ROLE=%q explicitly. The default docker-compose.yml brings up both; see docs/self-hosting/scaling.md", RoleControl, RoleExecutor)
	}
	roleMu.Lock()
	defer roleMu.Unlock()
	if roleInitialized {
		if currentRole == resolved {
			return nil
		}
		return fmt.Errorf("role already initialized as %q; cannot re-init as %q", currentRole, resolved)
	}
	currentRole = resolved
	roleInitialized = true
	return nil
}

// InitRoleFromEnv reads TF_ROLE, parses it, and installs the process-wide
// role. An invalid value returns an error (fail boot loudly), and so does
// all/unset in multi mode (see InitRole). In local mode a non-all role is
// coerced to all and a warning is returned via coerced=true + the
// requested role, so main() can log it — runmode has no logger dependency
// of its own.
//
// Call once in main(), right after InitFromEnv() and before the argv
// dispatch, so the migrate subcommand and every subsystem see the same
// resolved role.
func InitRoleFromEnv() (coerced bool, requested DeployRole, err error) {
	requested, err = ParseRole(os.Getenv("TF_ROLE"))
	if err != nil {
		return false, "", err
	}
	coerced = Current() == ModeLocal && requested != RoleAll
	if err := InitRole(requested); err != nil {
		return false, requested, err
	}
	return coerced, requested, nil
}
