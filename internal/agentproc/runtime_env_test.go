package agentproc

import (
	"reflect"
	"testing"
)

func TestRunMemoryLimitMB(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int
	}{
		{"empty uses default", "", DefaultClaimMemoryLimitMB},
		{"zero disables", "0", 0},
		{"plain value", "2048", 2048},
		{"trims whitespace", " 8192 ", 8192},
		{"negative falls back", "-5", DefaultClaimMemoryLimitMB},
		{"garbage falls back", "big", DefaultClaimMemoryLimitMB},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TF_CLAIM_MEMORY_LIMIT_MB", tt.raw)
			if got := ClaimMemoryLimitMB(); got != tt.want {
				t.Errorf("ClaimMemoryLimitMB() with %q = %d, want %d", tt.raw, got, tt.want)
			}
		})
	}
}

// TestAgentRuntimeEnvFor pins the engine tuning per OS: the JIT is off
// by default on every host but Darwin and restored by the opt-in, while
// Darwin never carries the flag at all — JavaScriptCore drops SharedArrayBuffer there
// whenever the JIT is disabled, and the engine needs it to boot.
func TestAgentRuntimeEnvFor(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		optIn string
		want  []string
	}{
		{"linux default off", "linux", "", []string{"BUN_JSC_useJIT=0"}},
		{"linux opt-in restores JIT", "linux", "1", nil},
		{"darwin never disables", "darwin", "", nil},
		{"darwin opt-in still nothing", "darwin", "1", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("TF_AGENT_JSC_JIT", tt.optIn)
			if got := agentRuntimeEnvFor(tt.goos); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("agentRuntimeEnvFor(%q) with TF_AGENT_JSC_JIT=%q = %v, want %v", tt.goos, tt.optIn, got, tt.want)
			}
		})
	}
}
