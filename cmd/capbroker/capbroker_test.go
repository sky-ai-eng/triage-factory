package capbroker

import "testing"

func TestEnabled(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		// Default-on: unset, and anything not a recognized falsy spelling,
		// is enabled.
		{"", true},
		{"bogus", true},
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"on", true},
		{"yes", true},
		{" true ", true},
		// The rollback escape hatch: only these spellings turn it off.
		{"0", false},
		{"false", false},
		{"off", false},
		{"no", false},
		{" FALSE ", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			t.Setenv("TF_PRIVSEP", tt.val)
			if got := Enabled(); got != tt.want {
				t.Errorf("Enabled() with TF_PRIVSEP=%q = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}
