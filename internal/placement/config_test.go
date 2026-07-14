package placement

import (
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestConfigFromEnv_DefaultsEnabledInMulti(t *testing.T) {
	cfg, err := ConfigFromEnv(env(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Enabled || cfg.Aging != DefaultAging || cfg.Liveness != DefaultLiveness {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestConfigFromEnv_LocalForcesOff(t *testing.T) {
	cfg, err := ConfigFromEnv(env(map[string]string{"TF_PLACEMENT": "on"}), true)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Enabled {
		t.Fatalf("local mode must force placement off even with TF_PLACEMENT=on: %+v", cfg)
	}
}

func TestConfigFromEnv_ExplicitOff(t *testing.T) {
	for _, v := range []string{"off", "0", "false", "no", "disabled"} {
		cfg, err := ConfigFromEnv(env(map[string]string{"TF_PLACEMENT": v}), false)
		if err != nil {
			t.Fatalf("%q: %v", v, err)
		}
		if cfg.Enabled {
			t.Fatalf("TF_PLACEMENT=%q should disable, got enabled", v)
		}
	}
}

func TestConfigFromEnv_CustomWindows(t *testing.T) {
	cfg, err := ConfigFromEnv(env(map[string]string{
		"TF_PLACEMENT_AGING_SEC":    "30",
		"TF_PLACEMENT_LIVENESS_SEC": "8",
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Aging != 30*time.Second || cfg.Liveness != 8*time.Second {
		t.Fatalf("custom windows wrong: %+v", cfg)
	}
}

func TestConfigFromEnv_RejectsBadValues(t *testing.T) {
	for _, m := range []map[string]string{
		{"TF_PLACEMENT": "maybe"},
		{"TF_PLACEMENT_AGING_SEC": "-1"},
		{"TF_PLACEMENT_LIVENESS_SEC": "abc"},
	} {
		if _, err := ConfigFromEnv(env(m), false); err == nil {
			t.Fatalf("expected error for %v", m)
		}
	}
}
