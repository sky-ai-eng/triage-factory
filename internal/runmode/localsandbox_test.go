package runmode

import (
	"strings"
	"testing"
)

func TestParseLocalSandbox(t *testing.T) {
	cases := []struct {
		raw     string
		want    LocalSandboxSetting
		wantErr bool
	}{
		{raw: "", want: LocalSandboxUnset},
		{raw: "on", want: LocalSandboxOn},
		{raw: "ON", want: LocalSandboxOn},
		{raw: "  off  ", want: LocalSandboxOff},
		// Boolean spellings are rejected on purpose: one spelling per answer
		// keeps the boot-refusal messages exact, and a value TF does not
		// understand must never resolve to a posture the operator did not ask
		// for.
		{raw: "true", wantErr: true},
		{raw: "1", wantErr: true},
		{raw: "yes", wantErr: true},
		{raw: "onn", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseLocalSandbox(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseLocalSandbox(%q) = %v, want an error", c.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseLocalSandbox(%q): %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseLocalSandbox(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// TestResolveLocalSandbox walks the whole matrix, because the difference
// between "off" and "refuse to boot" is the entire point of the knob: an
// explicit request that cannot be honored must fail loudly, while a default
// TF picked for the operator quietly resolves to whatever the platform can do.
func TestResolveLocalSandbox(t *testing.T) {
	cases := []struct {
		name    string
		setting LocalSandboxSetting
		mode    Mode
		linux   bool
		want    bool
		wantErr bool
	}{
		{name: "unset local linux defaults on", setting: LocalSandboxUnset, mode: ModeLocal, linux: true, want: true},
		{name: "unset local non-linux resolves off", setting: LocalSandboxUnset, mode: ModeLocal, linux: false, want: false},
		{name: "unset multi resolves off", setting: LocalSandboxUnset, mode: ModeMulti, linux: true, want: false},
		{name: "off local linux", setting: LocalSandboxOff, mode: ModeLocal, linux: true, want: false},
		{name: "off multi", setting: LocalSandboxOff, mode: ModeMulti, linux: true, want: false},
		{name: "on local linux", setting: LocalSandboxOn, mode: ModeLocal, linux: true, want: true},
		{name: "on multi refuses", setting: LocalSandboxOn, mode: ModeMulti, linux: true, wantErr: true},
		{name: "on non-linux refuses", setting: LocalSandboxOn, mode: ModeLocal, linux: false, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveLocalSandbox(c.setting, c.mode, c.linux)
			if c.wantErr {
				if err == nil {
					t.Fatalf("ResolveLocalSandbox = %t, want an error", got)
				}
				// The refusal has to name the variable, or the operator has no
				// way to act on it.
				if !strings.Contains(err.Error(), envLocalSandbox) {
					t.Errorf("refusal %q does not name %s", err, envLocalSandbox)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveLocalSandbox: %v", err)
			}
			if got != c.want {
				t.Errorf("ResolveLocalSandbox = %t, want %t", got, c.want)
			}
		})
	}
}

// TestInitLocalSandbox_Idempotency mirrors Init's contract: a repeat with the
// same answer is a no-op, a repeat with a different one is an error that
// leaves state alone.
func TestInitLocalSandbox_Idempotency(t *testing.T) {
	SetLocalSandboxForTest(t, false)

	localSandboxMu.Lock()
	localSandboxInitialized = false
	localSandboxEnabled = false
	localSandboxMu.Unlock()

	if err := InitLocalSandbox(true); err != nil {
		t.Fatalf("first InitLocalSandbox: %v", err)
	}
	if !LocalSandboxEnabled() {
		t.Fatal("LocalSandboxEnabled() = false after InitLocalSandbox(true)")
	}
	if err := InitLocalSandbox(true); err != nil {
		t.Fatalf("idempotent re-init: %v", err)
	}
	if err := InitLocalSandbox(false); err == nil {
		t.Fatal("conflicting re-init returned nil, want an error")
	}
	if !LocalSandboxEnabled() {
		t.Error("a refused re-init mutated state")
	}
}

// TestLocalSandboxEnabled_DefaultsOffBeforeInit pins the pre-boot default.
// Every path that never resolves the posture — a CLI subcommand, a unit test
// — must behave exactly as it did before the knob existed.
func TestLocalSandboxEnabled_DefaultsOffBeforeInit(t *testing.T) {
	localSandboxMu.RLock()
	enabled, inited := localSandboxEnabled, localSandboxInitialized
	localSandboxMu.RUnlock()
	t.Cleanup(func() {
		localSandboxMu.Lock()
		localSandboxEnabled, localSandboxInitialized = enabled, inited
		localSandboxMu.Unlock()
	})

	localSandboxMu.Lock()
	localSandboxEnabled, localSandboxInitialized = false, false
	localSandboxMu.Unlock()

	if LocalSandboxEnabled() {
		t.Error("LocalSandboxEnabled() = true before initialization; the un-inited default must be the historical behavior")
	}
}
