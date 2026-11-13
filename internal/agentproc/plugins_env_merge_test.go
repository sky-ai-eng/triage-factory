package agentproc

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/githooks"
)

// envEntries returns the values of every cmd.Env entry named key, in order.
func envEntries(env []string, key string) []string {
	var out []string
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			out = append(out, v)
		}
	}
	return out
}

func indexOfEnvKey(env []string, key string) int {
	return slices.IndexFunc(env, func(kv string) bool { return strings.HasPrefix(kv, key+"=") })
}

// A plugin key that collides with one TF sets for the run is dropped at the
// spawn and reported, and TF's value is the only one the child receives.
func TestNewDirectCommand_PluginEnvCollisionKeepsTFsValue(t *testing.T) {
	isolatePluginEnv(t)
	const extraKey = "MY_EXTRA_FLAG"
	dropped := map[string]string{}
	opts := RunOptions{
		Cwd:      t.TempDir(),
		ExtraEnv: []string{extraKey + "=from-tf"},
		Plugins: []Plugin{{
			Path: "/plugins/guard",
			Env: []string{
				"MY_PLUGIN_MODE=strict",
				extraKey + "=from-plugin",
				githooks.BinEnvVar + "=/evil/bin",
				githooks.LFSSkipSmudgeEnvVar + "=0",
				backgroundTasksEnvKey + "=0",
				"GIT_CONFIG_COUNT=99",
			},
		}},
		OnPluginEnvKeyDropped: func(key, reason string) { dropped[key] = reason },
	}
	cmd, err := newDirectCommand(context.Background(), opts, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}

	for _, key := range []string{extraKey, githooks.BinEnvVar, githooks.LFSSkipSmudgeEnvVar, backgroundTasksEnvKey, "GIT_CONFIG_COUNT"} {
		reason, ok := dropped[key]
		if !ok {
			t.Errorf("OnPluginEnvKeyDropped was not called for %s; dropped = %v", key, dropped)
			continue
		}
		if !strings.Contains(reason, "Triage Factory sets this key") {
			t.Errorf("reason for %s = %q", key, reason)
		}
	}
	if len(dropped) != 5 {
		t.Errorf("dropped = %v, want exactly the five colliding keys", dropped)
	}

	if got := envEntries(cmd.Env, extraKey); !slices.Equal(got, []string{"from-tf"}) {
		t.Errorf("%s entries = %v, want only TF's value", extraKey, got)
	}
	if got := envEntries(cmd.Env, githooks.LFSSkipSmudgeEnvVar); !slices.Equal(got, []string{"1"}) {
		t.Errorf("%s entries = %v, want [1]", githooks.LFSSkipSmudgeEnvVar, got)
	}
	if got := envEntries(cmd.Env, backgroundTasksEnvKey); !slices.Equal(got, []string{"1"}) {
		t.Errorf("%s entries = %v, want [1]", backgroundTasksEnvKey, got)
	}
	if got := envEntries(cmd.Env, githooks.BinEnvVar); len(got) != 1 || got[0] == "/evil/bin" {
		t.Errorf("%s entries = %v, want one entry, not the plugin's", githooks.BinEnvVar, got)
	}
	if got := envEntries(cmd.Env, "GIT_CONFIG_COUNT"); len(got) != 1 || got[0] == "99" {
		t.Errorf("GIT_CONFIG_COUNT entries = %v, want the one DirectAgentEnv computed", got)
	}
}

func TestNewDirectCommand_PluginEnvReachesTheChild(t *testing.T) {
	isolatePluginEnv(t)
	t.Setenv("MY_INHERITED", "from-parent")
	opts := RunOptions{
		Cwd: t.TempDir(),
		Plugins: []Plugin{
			{Path: "/plugins/a", Env: []string{"MY_PLUGIN_MODE=strict", "MY_STATE=/run/a/", "MY_INHERITED=from-plugin"}},
			{Path: "/plugins/b", Env: []string{"MY_OTHER=2"}},
		},
	}
	cmd, err := newDirectCommand(context.Background(), opts, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	for key, want := range map[string]string{
		"MY_PLUGIN_MODE": "strict",
		"MY_STATE":       "/run/a/",
		"MY_OTHER":       "2",
	} {
		if got := envEntries(cmd.Env, key); !slices.Equal(got, []string{want}) {
			t.Errorf("%s entries = %v, want [%s]", key, got, want)
		}
	}

	// A key the operator's shell exported is not one TF sets, so the plugin's
	// value follows it and wins under last-entry-wins.
	inherited := envEntries(cmd.Env, "MY_INHERITED")
	if len(inherited) == 0 || inherited[len(inherited)-1] != "from-plugin" {
		t.Errorf("MY_INHERITED entries = %v, want the plugin's value last", inherited)
	}

	// The plugin's entries precede TF's own run entries, so TF's win any
	// duplicate a later change introduces.
	if pluginAt, tfAt := indexOfEnvKey(cmd.Env, "MY_PLUGIN_MODE"), indexOfEnvKey(cmd.Env, githooks.BinEnvVar); tfAt >= 0 && pluginAt > tfAt {
		t.Errorf("plugin entry at %d follows TF's %s at %d", pluginAt, githooks.BinEnvVar, tfAt)
	}
}

func TestNewDirectCommand_PluginEnvCollisionWithoutACallback(t *testing.T) {
	isolatePluginEnv(t)
	cmd, err := newDirectCommand(context.Background(), RunOptions{
		Cwd:      t.TempDir(),
		ExtraEnv: []string{"MY_EXTRA_FLAG=from-tf"},
		Plugins:  []Plugin{{Path: "/plugins/guard", Env: []string{"MY_EXTRA_FLAG=from-plugin"}}},
	}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	if got := envEntries(cmd.Env, "MY_EXTRA_FLAG"); !slices.Equal(got, []string{"from-tf"}) {
		t.Errorf("MY_EXTRA_FLAG entries = %v, want [from-tf]", got)
	}
}

// A run with no plugins gets the environment it got before plugins existed.
func TestNewDirectCommand_NoPluginsLeavesTheEnvUnchanged(t *testing.T) {
	isolatePluginEnv(t)
	cwd := t.TempDir()
	base := RunOptions{Cwd: cwd, ExtraEnv: []string{"MY_EXTRA_FLAG=from-tf"}}
	called := false
	withEmpty := base
	withEmpty.Plugins = []Plugin{}
	withEmpty.OnPluginEnvKeyDropped = func(string, string) { called = true }

	want, err := newDirectCommand(context.Background(), base, []string{"wrapper.mjs", "--model", "x"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	got, err := newDirectCommand(context.Background(), withEmpty, []string{"wrapper.mjs", "--model", "x"})
	if err != nil {
		t.Fatalf("newDirectCommand with an empty list: %v", err)
	}
	if !slices.Equal(got.Env, want.Env) {
		t.Errorf("env differs with an empty plugin list\n got: %v\nwant: %v", got.Env, want.Env)
	}
	if !slices.Equal(got.Args, want.Args) {
		t.Errorf("args differ with an empty plugin list\n got: %v\nwant: %v", got.Args, want.Args)
	}
	if called {
		t.Error("OnPluginEnvKeyDropped was called for a run with no plugins")
	}
}

// A plugin with no env of its own adds nothing, and so is the same run as one
// with none.
func TestNewDirectCommand_PluginWithoutEnvAddsNoEntries(t *testing.T) {
	isolatePluginEnv(t)
	cwd := t.TempDir()
	without, err := newDirectCommand(context.Background(), RunOptions{Cwd: cwd}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	with, err := newDirectCommand(context.Background(), RunOptions{Cwd: cwd, Plugins: []Plugin{{Path: "/plugins/a"}}}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand with a plugin: %v", err)
	}
	if !slices.Equal(with.Env, without.Env) {
		t.Errorf("env differs\n got: %v\nwant: %v", with.Env, without.Env)
	}
}
