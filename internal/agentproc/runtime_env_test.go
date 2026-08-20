package agentproc

import "testing"

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
