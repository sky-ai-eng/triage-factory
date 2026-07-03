package agentproc

import (
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

// DefaultRunMemoryLimitMB is the per-run memory ceiling handed to the
// sandbox when TF_RUN_MEMORY_LIMIT_MB is unset. Deliberately generous —
// ~16x the fleet-measured per-run budget — because in-sandbox builds
// legitimately spike (go build, dependency installs); the ceiling is
// host protection against a pathological run, not budget enforcement.
const DefaultRunMemoryLimitMB = 4096

// runMemoryLimitMB resolves the per-run sandbox memory ceiling from
// TF_RUN_MEMORY_LIMIT_MB. Empty → the default; 0 → disabled; invalid →
// the default with one warning per process (a bad value must not brick
// spawning). Read per spawn like the other agent runtime knobs.
func runMemoryLimitMB() int {
	raw := strings.TrimSpace(os.Getenv("TF_RUN_MEMORY_LIMIT_MB"))
	if raw == "" {
		return DefaultRunMemoryLimitMB
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		warnBadRunMemoryLimitOnce.Do(func() {
			agentprocLog.Warn("invalid TF_RUN_MEMORY_LIMIT_MB; using default",
				"raw", raw, "default_mb", DefaultRunMemoryLimitMB)
		})
		return DefaultRunMemoryLimitMB
	}
	return n
}

var warnBadRunMemoryLimitOnce sync.Once
