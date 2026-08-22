package runmode

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
)

// Local-mode OS sandbox posture. A single knob — TF_LOCAL_SANDBOX (on/off)
// — that decides whether a local delegated agent run executes inside a
// bubblewrap mount namespace instead of as a plain child of this process.
//
// It is courtesy isolation, not a security boundary: same uid, shared
// network, shared /proc. What it buys is that a confused or wandering agent
// cannot read a sibling run's worktree, the SQLite DB, or the operator's
// home directory. Multi mode's gVisor jail is the boundary that backs a
// multi-tenant promise; this one backs no promise, which is exactly why it
// keeps an escape hatch.
//
// Three resolutions, and the difference between them is who made the claim:
//
//   - off — the operator opted out. Agents run with their full user's
//     powers, as they did before this knob existed.
//   - on — the operator asked for the sandbox. Honored in local mode on
//     Linux; refused at boot anywhere it cannot be honored, because
//     silently ignoring an explicit security request is the one outcome a
//     safety knob must never produce.
//   - unset — TF picks. Local mode on Linux resolves to on (an opt-in
//     safety feature protects only the users who least need protecting);
//     everything else resolves to off, since the sandbox is bubblewrap and
//     bubblewrap is Linux-only.
//
// Whether bubblewrap is actually usable on this host is a separate
// question, answered by a boot probe at the consumer (internal/app) rather
// than here: this package resolves the operator's intent, and the probe
// turns an unusable environment into a boot refusal rather than a silent
// downgrade.
type LocalSandboxSetting int

const (
	// LocalSandboxUnset is an absent or empty TF_LOCAL_SANDBOX. Its own
	// value rather than a synonym for off, because unset and off resolve
	// differently on Linux and only an explicit value can fail boot.
	LocalSandboxUnset LocalSandboxSetting = iota

	// LocalSandboxOn is an explicit TF_LOCAL_SANDBOX=on.
	LocalSandboxOn

	// LocalSandboxOff is an explicit TF_LOCAL_SANDBOX=off.
	LocalSandboxOff
)

// envLocalSandbox is the knob's environment variable name.
const envLocalSandbox = "TF_LOCAL_SANDBOX"

// localSandboxEnabled is the resolved answer; localSandboxInitialized
// tracks whether boot resolved it. Defaults to false so a process that
// never initializes (a unit test, a CLI subcommand that short-circuits
// before boot) behaves exactly as it did before the knob existed.
var (
	localSandboxEnabled     bool
	localSandboxInitialized bool
	localSandboxMu          sync.RWMutex
)

// LocalSandboxEnabled reports whether local agent runs execute inside the
// bubblewrap mount namespace. Safe from any goroutine. False until
// InitLocalSandboxFromEnv resolves it, so every path that never boots the
// server keeps the unsandboxed behavior.
func LocalSandboxEnabled() bool {
	localSandboxMu.RLock()
	defer localSandboxMu.RUnlock()
	return localSandboxEnabled
}

// ParseLocalSandbox parses a TF_LOCAL_SANDBOX env-var string. Empty maps to
// LocalSandboxUnset; "on" and "off" map to their constants; anything else
// errors so a typo fails boot loudly rather than resolving to the wrong
// posture. Case-insensitive and whitespace-trimmed, mirroring ModeFromEnv.
//
// Deliberately narrower than the usual boolean spellings: this value is
// read back to the operator in the boot-refusal messages, and one spelling
// per answer keeps those messages exact.
func ParseLocalSandbox(s string) (LocalSandboxSetting, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return LocalSandboxUnset, nil
	case "on":
		return LocalSandboxOn, nil
	case "off":
		return LocalSandboxOff, nil
	default:
		return LocalSandboxUnset, fmt.Errorf("unknown %s=%q (want \"on\" or \"off\", or empty for the default)", envLocalSandbox, s)
	}
}

// ResolveLocalSandbox turns a parsed setting into the process-wide answer
// for a given mode and platform. Pure — no env, no runtime lookups — so
// every cell of the matrix is testable without env manipulation, the same
// discipline internal/agentprompt's Mode parameter follows.
//
// An explicit on that cannot be honored is an error, never a downgrade:
// asking for isolation and getting none is worse than being told no.
func ResolveLocalSandbox(setting LocalSandboxSetting, mode Mode, linux bool) (bool, error) {
	switch setting {
	case LocalSandboxOff:
		return false, nil
	case LocalSandboxOn:
		if mode != ModeLocal {
			return false, fmt.Errorf("%s=on is meaningless with TF_MODE=%s: multi-mode agent runs already execute inside a gVisor jail. Unset %s",
				envLocalSandbox, mode, envLocalSandbox)
		}
		if !linux {
			return false, fmt.Errorf("%s=on cannot be honored on %s: the local sandbox is bubblewrap, which is Linux-only. Unset %s or set %s=off",
				envLocalSandbox, runtime.GOOS, envLocalSandbox, envLocalSandbox)
		}
		return true, nil
	default:
		return mode == ModeLocal && linux, nil
	}
}

// InitLocalSandbox sets the process-wide answer. Same idempotency contract
// as Init: first call sets state and returns nil; a subsequent call with
// the same value is a no-op; a subsequent call with a different value
// returns an error without mutating state.
func InitLocalSandbox(enabled bool) error {
	localSandboxMu.Lock()
	defer localSandboxMu.Unlock()
	if localSandboxInitialized {
		if localSandboxEnabled == enabled {
			return nil
		}
		return fmt.Errorf("local sandbox already initialized as enabled=%t; cannot re-init as enabled=%t", localSandboxEnabled, enabled)
	}
	localSandboxEnabled = enabled
	localSandboxInitialized = true
	return nil
}

// InitLocalSandboxFromEnv reads TF_LOCAL_SANDBOX and initializes the
// toggle against this process's mode and platform. Call it right after
// InitFromEnv — the resolution is a function of the mode, so the mode has
// to be settled first.
func InitLocalSandboxFromEnv() error {
	setting, err := ParseLocalSandbox(os.Getenv(envLocalSandbox))
	if err != nil {
		return err
	}
	enabled, err := ResolveLocalSandbox(setting, Current(), runtime.GOOS == "linux")
	if err != nil {
		return err
	}
	return InitLocalSandbox(enabled)
}
