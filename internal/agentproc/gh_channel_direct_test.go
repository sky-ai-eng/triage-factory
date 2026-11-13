package agentproc

import (
	"context"
	"os"
	"path/filepath"
	"slices"
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
	// A half-built channel never points gh at its listener: with no trust file
	// that would fail every TLS handshake instead of degrading cleanly. It still
	// carries the refusing tokens, since the inherited ones are already gone.
	env := envMap(t, directGHChannelEnv(&GHChannelParams{Host: "127.0.0.1:1"}))
	if _, ok := env["GH_HOST"]; ok {
		t.Errorf("directGHChannelEnv(no cert) sets GH_HOST: %v", env)
	}
	if env["GH_TOKEN"] != ghNoCredentialToken || env["GH_ENTERPRISE_TOKEN"] != ghNoCredentialToken {
		t.Errorf("directGHChannelEnv(no cert) = %v, want the refusing tokens", env)
	}
}

// A run whose channel could not start gets a gh config dir of its own and a
// refusing token for both hosts gh knows, and nothing else: no listener to
// point gh at, no PATH entry for a binary TF does not have. Both token
// variables matter, because gh reads the OS keyring when they are unset, and
// the keyring is where `gh auth login` keeps the user's credential.
func TestDirectGHChannelEnv_IsolationOnly(t *testing.T) {
	want := map[string]string{
		"GH_CONFIG_DIR":         "/state/gh-channel/run1/config",
		"GH_TOKEN":              ghNoCredentialToken,
		"GH_ENTERPRISE_TOKEN":   ghNoCredentialToken,
		"GH_PROMPT_DISABLED":    "1",
		"GH_NO_UPDATE_NOTIFIER": "1",
	}
	for name, gc := range map[string]*GHChannelParams{
		"no listener":             {ConfigDir: "/state/gh-channel/run1/config"},
		"listener, no trust file": {Host: "127.0.0.1:1", ConfigDir: "/state/gh-channel/run1/config"},
	} {
		env := envMap(t, directGHChannelEnv(gc))
		if len(env) != len(want) {
			t.Errorf("%s: env = %v, want exactly %v", name, env, want)
			continue
		}
		for k, v := range want {
			if env[k] != v {
				t.Errorf("%s: %s = %q, want %q", name, k, env[k], v)
			}
		}
	}
}

func TestGHChannelParams_Live(t *testing.T) {
	var none *GHChannelParams
	for name, tc := range map[string]struct {
		gc   *GHChannelParams
		want bool
	}{
		"nil":            {none, false},
		"isolation-only": {&GHChannelParams{ConfigDir: "/state/gh-channel/run1/config"}, false},
		"live":           {&GHChannelParams{Host: "127.0.0.1:1", CertSourcePath: "/c", ConfigDir: "/d"}, true},
	} {
		if got := tc.gc.Live(); got != tc.want {
			t.Errorf("%s: Live() = %v, want %v", name, got, tc.want)
		}
	}
}

// Every GitHub credential the operator's shell exported stays out of a run that
// has a gh channel, isolation-only or full: gh reads a token from the
// environment before it reads its config dir, so an empty config dir alone
// would still hand the agent the operator's identity.
func TestNewDirectCommand_GHChannelDropsInheritedGitHubTokens(t *testing.T) {
	writeOperatorSettings(t, `{}`)
	const fromShell = "from-the-shell"
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_HOST", "GH_CONFIG_DIR"} {
		t.Setenv(k, fromShell)
	}
	// Mixed case: Windows reads `github_token` as GITHUB_TOKEN. Elsewhere the
	// name is distinct and gh ignores it, so dropping it costs nothing.
	t.Setenv("github_Token", fromShell)
	configDir := filepath.Join(t.TempDir(), "config")

	cmd, err := newDirectCommand(context.Background(), RunOptions{
		Cwd:       t.TempDir(),
		GHChannel: &GHChannelParams{ConfigDir: configDir},
	}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	for _, e := range cmd.Env {
		k, v, _ := strings.Cut(e, "=")
		if v == fromShell && slices.ContainsFunc(ghInheritedEnvKeys, func(g string) bool { return strings.EqualFold(g, k) }) {
			t.Errorf("%s=%q reached a run with a gh channel", k, v)
		}
	}
	env := envMap(t, cmd.Env)
	for k, want := range map[string]string{
		"GH_CONFIG_DIR":       configDir,
		"GH_TOKEN":            ghNoCredentialToken,
		"GH_ENTERPRISE_TOKEN": ghNoCredentialToken,
	} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
}

func TestDropEnvKeysFold(t *testing.T) {
	got := dropEnvKeysFold([]string{"GH_TOKEN=a", "gh_token=b", "PATH=/bin", "GH_TOKENX=c"}, []string{"GH_TOKEN"})
	want := []string{"PATH=/bin", "GH_TOKENX=c"}
	if !slices.Equal(got, want) {
		t.Errorf("dropEnvKeysFold = %v, want %v", got, want)
	}
}

func TestDirectGHChannelEnv_Shape(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", "/usr/bin"+sep+"/bin")
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
		"GH_TOKEN":              ghNoCredentialToken,
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
	if len(parts) == 0 || parts[0] != filepath.Clean("/state/bin") {
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
	// prependPath cleans the directory, which spells it with the platform's
	// separator; the expectations do too.
	tf := filepath.Clean("/tf/bin")
	if got := prependPath("/tf/bin", ""); got != tf {
		t.Errorf("empty PATH = %q, want %s", got, tf)
	}
	// Idempotent: applying it twice must not accumulate duplicates.
	once := prependPath("/tf/bin", "/usr/bin"+sep+"/bin")
	if got := prependPath("/tf/bin", once); got != once {
		t.Errorf("second prepend = %q, want %q", got, once)
	}
	// An existing copy anywhere in PATH moves to the front rather than
	// leaving a stale duplicate behind it.
	if got := prependPath("/tf/bin", "/usr/bin"+sep+"/tf/bin"); got != tf+sep+"/usr/bin" {
		t.Errorf("prepend over an existing entry = %q", got)
	}
}
