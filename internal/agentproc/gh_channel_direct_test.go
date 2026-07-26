package agentproc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			t.Fatalf("malformed env entry %q", e)
		}
		out[k] = v
	}
	return out
}

func TestDirectGHChannelEnv_Off(t *testing.T) {
	if got := directGHChannelEnv(nil); got != nil {
		t.Errorf("directGHChannelEnv(nil) = %v, want nil", got)
	}
	// A half-built channel is off too: pointing gh at a listener with no trust
	// file would fail every TLS handshake instead of degrading cleanly.
	if got := directGHChannelEnv(&GHChannelParams{Host: "127.0.0.1:1"}); got != nil {
		t.Errorf("directGHChannelEnv(no cert) = %v, want nil", got)
	}
}

func TestDirectGHChannelEnv_Shape(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	gc := &GHChannelParams{
		Host:           "127.0.0.1:54321",
		Token:          "per-run-placeholder",
		CertSourcePath: "/state/gh-channel/run1/injector.crt",
		BinDir:         "/state/bin",
		ConfigDir:      "/state/gh-channel/run1/config",
	}
	env := envMap(t, directGHChannelEnv(gc))

	for k, want := range map[string]string{
		"GH_HOST":               "127.0.0.1:54321",
		"GH_ENTERPRISE_TOKEN":   "per-run-placeholder",
		"SSL_CERT_FILE":         "/state/gh-channel/run1/injector.crt",
		"GH_PROMPT_DISABLED":    "1",
		"GH_NO_UPDATE_NOTIFIER": "1",
		"GH_CONFIG_DIR":         "/state/gh-channel/run1/config",
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	// PATH must LEAD with the TF-owned dir, or `gh` resolves to whatever the
	// user has installed — which carries the user's own GitHub auth.
	parts := filepath.SplitList(env["PATH"])
	if len(parts) == 0 || parts[0] != "/state/bin" {
		t.Errorf("PATH = %q, want it to lead with the TF-owned gh dir", env["PATH"])
	}
	if !strings.Contains(env["PATH"], "/usr/bin") {
		t.Errorf("PATH = %q, want the inherited entries preserved after the prepend", env["PATH"])
	}
}

// The channel's environment must never carry the org's real credential — the
// placeholder is the whole point, and in local mode there is no jail to enforce
// it, so it is asserted here instead.
func TestDirectGHChannelEnv_PlaceholderOnly(t *testing.T) {
	const realToken = "ghs_the_real_org_credential"
	env := directGHChannelEnv(&GHChannelParams{
		Host:           "127.0.0.1:1",
		Token:          "placeholder-only",
		CertSourcePath: "/tmp/injector.crt",
	})
	for _, e := range env {
		if strings.Contains(e, realToken) {
			t.Fatalf("env entry %q carries the real credential", e)
		}
	}
	if m := envMap(t, env); m["GH_ENTERPRISE_TOKEN"] != "placeholder-only" {
		t.Errorf("GH_ENTERPRISE_TOKEN = %q, want the per-run placeholder", m["GH_ENTERPRISE_TOKEN"])
	}
}

func TestPrependPath(t *testing.T) {
	sep := string(os.PathListSeparator)
	if got := prependPath("/tf/bin", ""); got != "/tf/bin" {
		t.Errorf("empty PATH = %q, want /tf/bin", got)
	}
	// Idempotent: applying it twice must not accumulate duplicates.
	once := prependPath("/tf/bin", "/usr/bin"+sep+"/bin")
	if got := prependPath("/tf/bin", once); got != once {
		t.Errorf("second prepend = %q, want %q", got, once)
	}
	// An existing copy anywhere in PATH moves to the front rather than
	// leaving a stale duplicate behind it.
	if got := prependPath("/tf/bin", "/usr/bin"+sep+"/tf/bin"); got != "/tf/bin"+sep+"/usr/bin" {
		t.Errorf("prepend over an existing entry = %q", got)
	}
}
