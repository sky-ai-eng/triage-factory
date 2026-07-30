package sandbox

import (
	"os"
	"strings"
	"testing"
)

// captureEnvMap parses CaptureChildEnv() into key→value, failing on a malformed
// or duplicated entry: which of two entries for one key wins is the exec layer's
// business, and this env's guarantees must not rest on that.
func captureEnvMap(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range CaptureChildEnv() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("env entry %q is not KEY=VALUE", kv)
		}
		if _, dup := out[k]; dup {
			t.Errorf("env sets %s twice", k)
		}
		out[k] = v
	}
	return out
}

// TestCaptureChildEnv_CarriesOnlyGitLocals pins the two properties the capture
// child's environment exists for: it inherits nothing from this process (the
// orchestrator's env holds DB passwords and service tokens the child, running
// attacker-influenceable git, must never see), and it pins git away from every
// user/global/system config a shared writable HOME could plant.
func TestCaptureChildEnv_CarriesOnlyGitLocals(t *testing.T) {
	t.Setenv("TF_SECRET_ENCRYPTION_KEY", "super-secret-should-not-travel")

	env := captureEnvMap(t)
	for k, want := range map[string]string{
		"HOME":              "/nonexistent",
		"GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_CONFIG_SYSTEM": os.DevNull,
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if env["PATH"] == "" {
		t.Error("PATH is empty; the child could not locate the git binary")
	}
	if _, leaked := env["TF_SECRET_ENCRYPTION_KEY"]; leaked {
		t.Error("the child env inherited a process secret; it must carry only what git needs")
	}
}

// TestCaptureChildEnv_KeepsDiffExternalTripwire holds the empty `diff.external`
// override in place. It is not protection — git reads an empty value as the
// command "" and tries to exec it — which is exactly its job: a `git diff` that
// reaches this key dies loudly instead of quietly running whatever program the
// agent named in the run root's own (agent-writable, and read) config. The
// capture's diffs pass --no-ext-diff --no-textconv, so they never reach it;
// dropping this entry would leave a diff added later without those flags
// silently trusting repo config. There is no value meaning "disabled" to swap in
// — every non-empty one is a command git runs.
func TestCaptureChildEnv_KeepsDiffExternalTripwire(t *testing.T) {
	env := captureEnvMap(t)
	if got := env["GIT_CONFIG_COUNT"]; got != "2" {
		t.Errorf("GIT_CONFIG_COUNT = %q, want 2; git reads only the first COUNT overrides", got)
	}
	for _, want := range []struct{ key, value string }{
		{"GIT_CONFIG_KEY_0", "core.fsmonitor"},
		{"GIT_CONFIG_KEY_1", "diff.external"},
	} {
		if got := env[want.key]; got != want.value {
			t.Errorf("%s = %q, want %q", want.key, got, want.value)
		}
	}
	for _, key := range []string{"GIT_CONFIG_VALUE_0", "GIT_CONFIG_VALUE_1"} {
		v, ok := env[key]
		if !ok {
			t.Errorf("%s is absent; a KEY without its VALUE makes git reject the whole override set", key)
			continue
		}
		if v != "" {
			t.Errorf("%s = %q, want empty: a non-empty value is a command git would run", key, v)
		}
	}
}
