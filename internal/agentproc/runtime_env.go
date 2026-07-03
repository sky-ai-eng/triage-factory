package agentproc

import "os"

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
	return []string{"BUN_JSC_useJIT=0"}
}
