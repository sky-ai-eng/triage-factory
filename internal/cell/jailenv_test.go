package cell

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/proxy/egressrelay"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// TestJailEnv_AssemblesEveryInputInOrder pins the whole environment for one
// launch. The order is what decides a repeated key under first-wins, and the
// value is what every backend must deliver, so both are asserted exactly.
func TestJailEnv_AssemblesEveryInputInOrder(t *testing.T) {
	got := JailEnv(LaunchSpec{
		RunTree: "/data/runs/task-1",
		Env: []string{
			"TRIAGE_FACTORY_CONVERSATION_ID=conv-1",
			"TRIAGE_FACTORY_CONVERSATION_ROOT=/data/runs/task-1",
		},
		ProxyEnv:  []string{"HTTPS_PROXY=http://10.42.1.1:9000", "GOPROXY=http://10.42.1.1:9001"},
		GHChannel: &GHChannel{Host: "10.42.1.1:443", Token: "placeholder"},
	})
	want := []string{
		"PATH=/opt/tf/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=/work",
		"TERM=xterm",
		"GOTOOLCHAIN=auto",
		"TRIAGE_FACTORY_SANDBOXED=1",
		"TRIAGE_FACTORY_CONVERSATION_ID=conv-1",
		"TRIAGE_FACTORY_CONVERSATION_ROOT=/work",
		"TRIAGE_FACTORY_BIN=/usr/local/bin/triagefactory",
		"HTTPS_PROXY=http://10.42.1.1:9000",
		"GOPROXY=http://10.42.1.1:9001",
		"SSL_CERT_FILE=/run/tf-ca-bundle.crt",
		"NODE_EXTRA_CA_CERTS=/run/tf-ca-bundle.crt",
		"PIP_CERT=/run/tf-ca-bundle.crt",
		"CARGO_HTTP_CAINFO=/run/tf-ca-bundle.crt",
		"GH_HOST=10.42.1.1:443",
		"GH_ENTERPRISE_TOKEN=placeholder",
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("JailEnv =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// TestJailEnv_NoGitConfig enforces the invariant documented on baseEnv: the
// jail's git config arrives in the proxy env as one indexed block that owns
// GIT_CONFIG_COUNT, so nothing else may emit a GIT_CONFIG_* entry. Asserted
// with an empty and a representative Env so a contributor who adds one here
// trips it at once.
func TestJailEnv_NoGitConfig(t *testing.T) {
	for _, extra := range [][]string{
		nil,
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1", "TRIAGE_FACTORY_WORKSPACE_KEY=r1"},
	} {
		for _, kv := range JailEnv(LaunchSpec{Env: extra}) {
			if strings.HasPrefix(kv, "GIT_CONFIG_") {
				t.Errorf("JailEnv emitted %q; only the proxy env's git block may carry GIT_CONFIG_*", kv)
			}
		}
	}
}

// TestJailEnv_CarriesSandboxMarker pins the "I am inside the jail" marker,
// unconditionally and whatever the caller passes. The jailed CLI reads it to
// tell a missing exec-verb socket (an outage) apart from a local-mode
// invocation, so a marker only sometimes set would make that read a coin
// flip. The last two cases are what make it trustworthy rather than merely
// present: a caller's own copy would duplicate the key or override the value.
func TestJailEnv_CarriesSandboxMarker(t *testing.T) {
	want := runmode.SandboxMarkerEnvVar + "=" + runmode.SandboxMarkerEnvValue
	for _, extra := range [][]string{
		nil,
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1"},
		{runmode.SandboxMarkerEnvVar + "=" + runmode.SandboxMarkerEnvValue},
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1", runmode.SandboxMarkerEnvVar + "=0"},
	} {
		if got := entriesFor(JailEnv(LaunchSpec{Env: extra}), runmode.SandboxMarkerEnvVar); len(got) != 1 || got[0] != want {
			t.Errorf("JailEnv(Env: %v) carries %v for %s; want exactly one entry, %q",
				extra, got, runmode.SandboxMarkerEnvVar, want)
		}
	}
}

// TestJailEnv_CarriesGoToolchainAuto pins the Go version floor's escape
// hatch. Under the rootfs's GOTOOLCHAIN=local a repo requiring a newer Go than
// the packaged one is unbuildable in the jail, and its absence would be a
// silent capability regression, hence a pin. The GOTOOLCHAIN=local cases are
// the load-bearing ones: that is the value an inherited env most likely
// carries, and exactly the one that re-breaks those builds.
func TestJailEnv_CarriesGoToolchainAuto(t *testing.T) {
	const want = "GOTOOLCHAIN=auto"
	for _, extra := range [][]string{
		nil,
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1"},
		{"GOTOOLCHAIN=local"},
		{"TRIAGE_FACTORY_CONVERSATION_ID=r1", "GOTOOLCHAIN=local"},
		{"GOTOOLCHAIN=auto"},
		{"GOTOOLCHAIN=go1.21.0"},
	} {
		if got := entriesFor(JailEnv(LaunchSpec{Env: extra}), "GOTOOLCHAIN"); len(got) != 1 || got[0] != want {
			t.Errorf("JailEnv(Env: %v) carries %v for GOTOOLCHAIN; want exactly one entry, %q", extra, got, want)
		}
	}
}

// TestJailEnv_DropsCatalogEnvKeys pins the relay-key filter. The proxy env's
// GOPROXY comes after Env, so under first-wins an inherited copy surviving in
// Env would shadow the relay's and point the jail's cmd/go at a host the
// allowlist does not carry. With no proxy env the right count is zero. Driven
// off the catalog so a future entry's key is covered the moment it exists.
func TestJailEnv_DropsCatalogEnvKeys(t *testing.T) {
	keys := egressrelay.CatalogEnvKeys()
	if len(keys) == 0 {
		t.Fatal("egressrelay.CatalogEnvKeys() is empty; expected at least GOPROXY")
	}
	for _, key := range keys {
		for _, extra := range [][]string{
			{key + "=direct"},
			{"TRIAGE_FACTORY_CONVERSATION_ID=r1", key + "=https://proxy.golang.org"},
		} {
			if got := entriesFor(JailEnv(LaunchSpec{Env: extra}), key); len(got) != 0 {
				t.Errorf("JailEnv(Env: %v) carries %v; catalog env keys must be dropped from Env so the relay's copy is the only one", extra, got)
			}
		}
	}
}

func TestTranslateEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     []string
		runTree string
		want    []string
	}{
		{
			name:    "nil_input",
			env:     nil,
			runTree: "/data/worktrees/abc",
			want:    nil,
		},
		{
			name: "non_path_values_passthrough",
			env: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_WORKSPACE_KEY=task-1",
			},
			runTree: "/data/worktrees/abc",
			want: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_WORKSPACE_KEY=task-1",
			},
		},
		{
			name:    "abs_path_under_run_tree_translates",
			env:     []string{"TRIAGE_FACTORY_CONVERSATION_ROOT=/data/worktrees/abc"},
			runTree: "/data/worktrees/abc",
			want:    []string{"TRIAGE_FACTORY_CONVERSATION_ROOT=/work"},
		},
		{
			name:    "abs_subpath_under_run_tree_translates",
			env:     []string{"TRIAGE_FACTORY_CONVERSATION_ROOT=/data/worktrees/abc/_tfac"},
			runTree: "/data/worktrees/abc",
			want:    []string{"TRIAGE_FACTORY_CONVERSATION_ROOT=/work/_tfac"},
		},
		{
			name:    "abs_path_outside_run_tree_dropped",
			env:     []string{"JAVA_HOME=/usr/lib/jvm/openjdk"},
			runTree: "/data/worktrees/abc",
			want:    []string{},
		},
		{
			name:    "dot_dot_escape_dropped",
			env:     []string{"X=/data/worktrees/abc/../other/sneaky"},
			runTree: "/data/worktrees/abc",
			want:    []string{},
		},
		{
			name: "mixed_keep_translate_drop",
			env: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_CONVERSATION_ROOT=/data/worktrees/abc",
				"JAVA_HOME=/usr/lib/jvm/openjdk",
			},
			runTree: "/data/worktrees/abc",
			want: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_CONVERSATION_ROOT=/work",
			},
		},
		{
			name: "empty_run_tree_drops_abs_paths_keeps_others",
			env: []string{
				"TRIAGE_FACTORY_CONVERSATION_ID=abc-123",
				"TRIAGE_FACTORY_CONVERSATION_ROOT=/data/worktrees/abc",
			},
			runTree: "",
			want:    []string{"TRIAGE_FACTORY_CONVERSATION_ID=abc-123"},
		},
		{
			name:    "malformed_no_equals_passthrough",
			env:     []string{"NOT_A_VALID_ENTRY"},
			runTree: "/data/worktrees/abc",
			want:    []string{"NOT_A_VALID_ENTRY"},
		},
		{
			name:    "empty_value_passthrough",
			env:     []string{"TRIAGE_FACTORY_CONVERSATION_ROOT="},
			runTree: "/data/worktrees/abc",
			want:    []string{"TRIAGE_FACTORY_CONVERSATION_ROOT="},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := translateEnv(c.env, c.runTree); !reflect.DeepEqual(got, c.want) {
				t.Errorf("translateEnv(%v, %q) = %v, want %v", c.env, c.runTree, got, c.want)
			}
		})
	}
}

// entriesFor is every entry of env for key.
func entriesFor(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}
