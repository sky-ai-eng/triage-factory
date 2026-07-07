package tracker

import "testing"

func TestParseRepoConcurrency(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		want        int
		wantClamped bool
		wantErr     bool
	}{
		{"empty uses default", "", DefaultRepoConcurrency, false, false},
		{"whitespace uses default", "  ", DefaultRepoConcurrency, false, false},
		{"plain value", "8", 8, false, false},
		{"trims whitespace", " 3 ", 3, false, false},
		{"one is legal", "1", 1, false, false},
		{"ceiling exactly", "16", MaxRepoConcurrency, false, false},
		{"above ceiling clamps", "1000", MaxRepoConcurrency, true, false},
		{"zero is invalid", "0", DefaultRepoConcurrency, false, true},
		{"negative is invalid", "-3", DefaultRepoConcurrency, false, true},
		{"garbage is invalid", "lots", DefaultRepoConcurrency, false, true},
		{"float is invalid", "4.5", DefaultRepoConcurrency, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, clamped, err := ParseRepoConcurrency(tt.raw)
			if got != tt.want {
				t.Errorf("ParseRepoConcurrency(%q) = %d, want %d", tt.raw, got, tt.want)
			}
			if clamped != tt.wantClamped {
				t.Errorf("ParseRepoConcurrency(%q) clamped = %v, want %v", tt.raw, clamped, tt.wantClamped)
			}
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseRepoConcurrency(%q) err = %v, wantErr %v", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestRepoConcurrency_ReadsEnvVar(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "7")
	if got := repoConcurrency(); got != 7 {
		t.Errorf("repoConcurrency() = %d, want 7", got)
	}
}

func TestRepoConcurrency_UnsetUsesDefault(t *testing.T) {
	t.Setenv("TF_POLL_REPO_CONCURRENCY", "")
	if got := repoConcurrency(); got != DefaultRepoConcurrency {
		t.Errorf("repoConcurrency() = %d, want default %d", got, DefaultRepoConcurrency)
	}
}
