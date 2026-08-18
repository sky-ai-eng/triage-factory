package agentproc

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"sync"
)

// jscJITEnvKey is the engine-side env var agentRuntimeEnv sets to
// disable JSC's JIT. Pulled out as a const (rather than inlined at each
// call site) so the direct-path spawn can filter any inherited copy
// by the same name before appending ours — see newDirectCommand.
const jscJITEnvKey = "BUN_JSC_useJIT"

// agentRuntimeEnv returns runtime-tuning env entries for the agent
// engine process. The vendored engine is a Bun-compiled binary running
// on JavaScriptCore, and with the JIT disabled its resident set shrinks
// by roughly a fifth while startup time is unchanged — boot cost is
// bundle parsing, not compilation, and agent workloads are I/O-bound
// enough that interpreter-only execution is noise. Applied on both the
// sandbox and direct paths (same engine either way); the node
// supervisor passes its env through to the engine subprocess.
//
// TF_AGENT_JSC_JIT=1 restores the JIT for operators whose workloads
// regress under the interpreter.
func agentRuntimeEnv() []string {
	if os.Getenv("TF_AGENT_JSC_JIT") == "1" {
		return nil
	}
	return []string{jscJITEnvKey + "=0"}
}

// ErrClaimMemoryLimit marks an agent process killed by its per-run
// memory ceiling. It rides inside the returned error chain so callers
// classify with errors.Is — never by matching message text — when
// recording a machine-readable failure kind for the UI.
var ErrClaimMemoryLimit = errors.New("run exceeded its memory limit")

// DefaultClaimMemoryLimitMB is the per-run memory ceiling handed to the
// sandbox when TF_CLAIM_MEMORY_LIMIT_MB is unset. Deliberately generous —
// ~16x the fleet-measured per-run budget — because in-sandbox builds
// legitimately spike (go build, dependency installs); the ceiling is
// host protection against a pathological run, not budget enforcement.
const DefaultClaimMemoryLimitMB = 4096

// ClaimMemoryLimitMB resolves the per-run sandbox memory ceiling from
// TF_CLAIM_MEMORY_LIMIT_MB. Empty → the default; 0 → disabled; invalid →
// the default with one warning per process (a bad value must not brick
// spawning). Read per spawn like the other agent runtime knobs.
//
// Exported because the native runtime derives its per-command bash budget
// from the same number the launch caps the jail at: the two must be one
// resolution, not two reads of one env var that could disagree.
func ClaimMemoryLimitMB() int {
	raw := strings.TrimSpace(os.Getenv("TF_CLAIM_MEMORY_LIMIT_MB"))
	if raw == "" {
		return DefaultClaimMemoryLimitMB
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		warnBadClaimMemoryLimitOnce.Do(func() {
			agentprocLog.Warn("invalid TF_CLAIM_MEMORY_LIMIT_MB; using default",
				"raw", raw, "default_mb", DefaultClaimMemoryLimitMB)
		})
		return DefaultClaimMemoryLimitMB
	}
	return n
}

var warnBadClaimMemoryLimitOnce sync.Once
