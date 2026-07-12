package llmcred

import (
	"testing"
	"time"
)

// TestTTLFromEnv_Clamps pins the floor (900s) and the ≤1h ceiling: the
// feature's blast-radius claim depends on the minted duration never exceeding
// one hour regardless of the operator's TF_LLM_CRED_TTL_SEC value.
func TestTTLFromEnv_Clamps(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", DefaultTTL},           // unset → default 1h
		{"1800", 30 * time.Minute}, // in-range → verbatim
		{"60", MinTTL},             // below floor → 15m
		{"7200", MaxTTL},           // above ceiling → clamped to 1h
		{"86400", MaxTTL},          // a full day → still 1h
	}
	for _, c := range cases {
		t.Setenv("TF_LLM_CRED_TTL_SEC", c.env)
		got, err := TTLFromEnv()
		if err != nil {
			t.Fatalf("TTLFromEnv(%q): %v", c.env, err)
		}
		if got != c.want {
			t.Errorf("TTLFromEnv(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

// TestNewResolver_ClampsTTLCeiling: even a resolver constructed directly with
// a >1h TTL mints for at most one hour.
func TestNewResolver_ClampsTTLCeiling(t *testing.T) {
	r := NewResolver(fakeSecrets{}, nil, 6*time.Hour, NetworkBinding{})
	if r.TTL() != MaxTTL {
		t.Errorf("TTL() = %v, want the %v ceiling", r.TTL(), MaxTTL)
	}
}
