package runmode

// Deployment role — TF_ROLE — is the horizontal-scaling companion to
// TF_MODE (see the package doc in runmode.go). It is a MULTI-MODE-ONLY
// input naming which half of the split this process runs:
//
//   - control  the API + brain half: HTTP/API/WS, migrations, the
//              leader-elected background brain.
//              Does NOT run the delegated-run dispatcher.
//   - executor a shared-nothing sandbox worker: the run dispatcher,
//              sandboxes, per-run credential sidecars, and the
//              worktree/snapshot reapers against its own TF_STATE_ROOT.
//              No user HTTP, no pollers, no router, no AI managers, no
//              migrations (it asserts the schema instead — see
//              internal/db).
//
// "all" is NOT an input: it is the internal name of local mode's resolved
// single-process shape (planForRole's everything-row, the value local
// registers into the instance registry). Local mode ignores TF_ROLE
// entirely — the single-process shape is gated on the MODE, not on a role
// an operator sets — and multi mode accepts only control/executor, so
// there is no environment value anywhere that yields a fused multi
// process.
//
// Read once at startup via InitRoleFromEnv(), called in main() right
// after InitFromEnv() and before the argv-dispatch switch, so every
// consumer — including the `migrate` subcommand — sees the same resolved
// role. This replaces internal/db/migrations.go's env-per-call
// isExecutorRole() placeholder: the migration gate now reads Role().
//
// Boot-safety rules, all deliberate:
//
//   - In multi mode TF_ROLE is REQUIRED and must be control or executor;
//     unset, "all", or a typo (TF_ROLE=exectuor silently running a full
//     stack on a sandbox host is exactly the misconfiguration this flag
//     exists to prevent) all fail boot loudly with a pointer at the
//     split blueprint. Multi is always the split: the credential-
//     isolation boundary hangs on the MODE (multi ⇒ every run's
//     credentials live only in its per-run sidecar), never on a
//     deployment knob an operator could forget.
//   - In local mode TF_ROLE is IGNORED — any value, valid or garbage,
//     logs one warning and the process runs the single-process shape.
//     Local is structurally single-process (SQLite, N=1, never
//     sandboxes), so a role is meaningless there, and a stray env var
//     must never brick a laptop.

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
	// RoleAll is the single-process shape: every subsystem in one binary.
	// NOT an input — TF_ROLE never resolves to it (ParseRole rejects
	// "all", local ignores TF_ROLE, multi requires a split role). It
	// exists as the internal name of local mode's resolved inventory:
	// what Role() answers there, what planForRole expands to everything,
	// and what a local process stamps into the instance registry.
	RoleAll DeployRole = "all"

	// RoleControl is the API + background-brain role. Serves user HTTP/WS,
	// runs migrations, hosts the leader-elected brain — but never claims or
	// executes delegated runs.
	RoleControl DeployRole = "control"

	// RoleExecutor is the shared-nothing sandbox-worker role: it drains the
	// run queue and executes delegated runs, and nothing else. Multi-mode
	// only.
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
// the mode policies InitRoleFromEnv applies, because those are
// boot-policy decisions, not the raw role. An unset or unparseable value
// degrades to RoleAll in that fallback (the safe single-process
// default); boot itself surfaces a bad value loudly via InitRoleFromEnv.
// Production initializes the role as the second line of main(), so this
// branch is test-only in practice.
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

// ParseRole parses a TF_ROLE env-var string into a Role. Only the two
// split roles are inputs: "all" is not a value an operator can set (the
// single-process shape is local mode's, gated on TF_MODE, and multi has
// no fused shape at all), and empty errors so multi's "forgot to set it"
// fails loudly — local never calls this (InitRoleFromEnv ignores TF_ROLE
// there). Case-insensitive and whitespace-trimmed, mirroring
// ModeFromEnv's conventions.
func ParseRole(s string) (DeployRole, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "control":
		return RoleControl, nil
	case "executor":
		return RoleExecutor, nil
	case "":
		return "", fmt.Errorf("TF_ROLE is required in multi mode: multi always runs as a control+executor split, so every process — including CLI subcommands like `migrate` — must set TF_ROLE=%q or TF_ROLE=%q explicitly. The default docker-compose.yml brings up both; see docs/self-hosting/scaling.md", RoleControl, RoleExecutor)
	case "all":
		return "", fmt.Errorf("TF_ROLE=all is not a deployable role: the single-process shape is local mode's (gated on TF_MODE, no role needed), and multi mode always runs as a control+executor split — set TF_ROLE=%q or TF_ROLE=%q; see docs/self-hosting/scaling.md", RoleControl, RoleExecutor)
	default:
		return "", fmt.Errorf("unknown TF_ROLE=%q (want %q or %q)", s, RoleControl, RoleExecutor)
	}
}

// InitRole sets the process-wide role. Only the split roles are valid in
// multi mode; RoleAll is accepted solely as local mode's resolved shape
// (InitRoleFromEnv passes it there) and rejected in multi so no
// programmatic caller can construct a fused multi process either. Same
// idempotency contract as Init: first call sets state and returns nil; a
// subsequent call with the same resolved role is a no-op; a subsequent
// call with a different role errors without mutating state.
func InitRole(requested DeployRole) error {
	if requested != RoleAll && requested != RoleControl && requested != RoleExecutor {
		return fmt.Errorf("unknown role %q (want %q or %q)", requested, RoleControl, RoleExecutor)
	}
	resolved := requested
	// Local mode is structurally single-process — whatever the caller
	// asked for, local resolves to the everything-shape.
	if Current() == ModeLocal {
		resolved = RoleAll
	}
	// Multi mode is always the control+executor split — a fused single
	// process would hold the secret store and host sandboxes at once,
	// which is exactly the shape the split exists to make undeployable.
	// Unreachable via TF_ROLE (ParseRole already rejects "all"/unset);
	// this guards programmatic callers.
	if Current() == ModeMulti && resolved == RoleAll {
		return fmt.Errorf("role %q is not valid in multi mode: multi always runs as a control+executor split (%q or %q)", RoleAll, RoleControl, RoleExecutor)
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

// InitRoleFromEnv installs the process-wide role per the mode policies.
// Local mode IGNORES TF_ROLE: any value — split role, "all", garbage —
// resolves to the single-process shape, with ignored=true (plus the raw
// value) returned when the var was set so main() can log one warning;
// runmode has no logger dependency of its own. A stray env var must
// never brick a laptop. Multi mode parses strictly: control or executor,
// anything else (including unset and "all") fails boot loudly.
//
// Call once in main(), right after InitFromEnv() and before the argv
// dispatch, so the migrate subcommand and every subsystem see the same
// resolved role.
func InitRoleFromEnv() (ignored bool, rawValue string, err error) {
	raw := os.Getenv("TF_ROLE")
	if Current() == ModeLocal {
		if err := InitRole(RoleAll); err != nil {
			return false, raw, err
		}
		return strings.TrimSpace(raw) != "", raw, nil
	}
	parsed, err := ParseRole(raw)
	if err != nil {
		return false, raw, err
	}
	return false, raw, InitRole(parsed)
}
