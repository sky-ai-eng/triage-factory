package agentproc

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// stubNode puts a `node` on PATH. directArgv resolves node to an absolute path
// before it builds the plan, and what these tests assert is the plan, not the
// interpreter, so they do not need a real one installed.
func stubNode(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node"), []byte("#!/bin/sh"+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// sandboxFixture is a run tree and a state root, with bwrap and node stubbed.
func sandboxFixture(t *testing.T) (runRoot string, spec *localsandbox.Spec) {
	t.Helper()
	root := t.TempDir()
	paths.SetForTest(t, root)
	stubBwrap(t)
	stubNode(t)
	runRoot = filepath.Join(root, "run")
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return runRoot, sandboxSpec(t, runRoot)
}

// bindAt is the index of the bind of target by flag (source and destination
// both target), or -1.
func bindAt(argv []string, flag, target string) int {
	for i := 0; i+2 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == target && argv[i+2] == target {
			return i
		}
	}
	return -1
}

// The plugins Claude Code is given and the directories the namespace binds are
// one list: a plugin bound only in the grants would be a flag pointing at a
// tmpfs.
func TestDirectArgv_FoldsInPlugins(t *testing.T) {
	runRoot, spec := sandboxFixture(t)
	pluginA, pluginB := newPluginDir(t), newPluginDir(t)
	grant := filepath.Join(filepath.Dir(runRoot), "granted")
	if err := os.MkdirAll(grant, 0o700); err != nil {
		t.Fatal(err)
	}
	spec.Plugins = []string{pluginA}

	argv, err := directArgv(RunOptions{
		Cwd:          runRoot,
		AddDirs:      []string{grant},
		Plugins:      []Plugin{{Path: pluginB}},
		LocalSandbox: spec,
	}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("directArgv: %v", err)
	}

	for _, dir := range []string{pluginA, pluginB} {
		at := bindAt(argv, "--ro-bind", dir)
		if at < 0 {
			t.Errorf("argv does not bind plugin %q strictly and read-only: %v", dir, argv)
			continue
		}
		if bindAt(argv, "--ro-bind-try", dir) >= 0 {
			t.Errorf("plugin %q is bound with -try, which would mount an empty directory if it vanished", dir)
		}
		if bindAt(argv, "--bind", dir) >= 0 {
			t.Errorf("plugin %q is bound writable", dir)
		}
		// After every bind-back of what the run resolved, before the grants.
		if last := lastIndexOf(argv, "--ro-bind-try"); at < last {
			t.Errorf("plugin bind at %d precedes a read-only bind-back at %d: %v", at, last, argv)
		}
		if grantAt := bindAt(argv, "--bind", grant); grantAt < at {
			t.Errorf("--add-dir grant at %d precedes the plugin bind at %d", grantAt, at)
		}
	}

	// The caller's Spec is a template reused across a retry or a resume.
	if !slices.Equal(spec.Plugins, []string{pluginA}) {
		t.Errorf("directArgv grew the caller's Spec.Plugins to %v", spec.Plugins)
	}
}

func lastIndexOf(args []string, flag string) int {
	for i := len(args) - 1; i >= 0; i-- {
		if args[i] == flag {
			return i
		}
	}
	return -1
}

func TestDirectArgv_PluginThatVanishedFailsTheSpawn(t *testing.T) {
	runRoot, spec := sandboxFixture(t)
	gone := filepath.Join(t.TempDir(), "gone")

	argv, err := directArgv(RunOptions{
		Cwd:          runRoot,
		Plugins:      []Plugin{{Path: gone, Required: true}},
		LocalSandbox: spec,
	}, []string{"wrapper.mjs"})
	if err == nil {
		t.Fatalf("directArgv = %v, want an error for a plugin directory that is not on disk", argv)
	}
	if !strings.Contains(err.Error(), "local plugin") || !strings.Contains(err.Error(), gone) {
		t.Errorf("directArgv error = %v, want it to name the local plugin %q", err, gone)
	}
}

func TestDirectArgv_NoPluginsAddsNoStrictBind(t *testing.T) {
	runRoot, spec := sandboxFixture(t)
	argv, err := directArgv(RunOptions{Cwd: runRoot, LocalSandbox: spec}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("directArgv: %v", err)
	}
	// The one strict read-only bind in a run with nothing to sign and no plugin
	// is the host root.
	var strict []string
	for i, a := range argv {
		if a == "--ro-bind" && i+1 < len(argv) {
			strict = append(strict, argv[i+1])
		}
	}
	if !slices.Equal(strict, []string{"/"}) {
		t.Errorf("strict read-only binds = %v, want only /", strict)
	}
}
