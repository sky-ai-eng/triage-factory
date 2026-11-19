//go:build linux

package localsandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandbox_PluginDirIsReadOnlyThroughTheHomeMask runs the real bubblewrap
// with a plugin directory under the masked home and checks what a run gets:
// the plugin comes back through the mask, a write to it fails, and the
// denyplugin fixture's hook, which uses only the system shell, runs inside
// the namespace and denies the probe command. A hook that could not start
// under the mask would let every call through, so this is the part of the
// guard the sandbox can break.
func TestSandbox_PluginDirIsReadOnlyThroughTheHomeMask(t *testing.T) {
	if err := Probe(); err != nil {
		t.Skipf("bubblewrap unusable here: %v", err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell: %v", err)
	}
	fixture, err := filepath.Abs(filepath.Join("..", "testdata", "denyplugin"))
	if err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	home := filepath.Join(base, "home")
	tempDir := filepath.Join(base, "tmp")
	runRoot := filepath.Join(tempDir, "triagefactory-runs", "mine")
	plugin := filepath.Join(home, "plugins", "denyplugin")
	for _, d := range []string{runRoot, filepath.Join(plugin, ".claude-plugin"), filepath.Join(plugin, "hooks")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/deny.sh"} {
		body, err := os.ReadFile(filepath.Join(fixture, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plugin, rel), body, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".netrc"), "operator secret")

	claudeDir := filepath.Join(home, ".claude")
	host := Host{Home: home, ClaudeDir: claudeDir, StateRoot: filepath.Join(home, ".triagefactory"), TempDir: tempDir, Cwd: runRoot}
	spec := Spec{RunRoot: runRoot, ClaudeProjectDir: filepath.Join(claudeDir, "projects", "-run"), Plugins: []string{plugin}}
	if err := Preflight(spec, host); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	args, err := Args(spec, host)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	run := func(script string) (string, error) {
		out, err := exec.Command(args[0], append(append([]string(nil), args[1:]...), sh, "-c", script)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	t.Run("the plugin comes back through the home mask", func(t *testing.T) {
		out, err := run("cat " + filepath.Join(plugin, ".claude-plugin", "plugin.json"))
		if err != nil || !strings.Contains(out, "denyplugin") {
			t.Fatalf("plugin manifest unreadable inside: %v (%s)", err, out)
		}
		if out, _ := run("cat " + filepath.Join(home, ".netrc")); strings.Contains(out, "operator secret") {
			t.Errorf("the rest of the home is readable: %q", out)
		}
	})

	t.Run("the plugin is read-only", func(t *testing.T) {
		hooks := filepath.Join(plugin, "hooks", "hooks.json")
		if out, err := run("echo '{}' > " + hooks); err == nil {
			t.Fatalf("a write to the plugin's hooks.json succeeded inside the namespace (%s)", out)
		}
		body, err := os.ReadFile(hooks)
		if err != nil || strings.TrimSpace(string(body)) == "{}" {
			t.Errorf("the host's hooks.json changed: %q, %v", body, err)
		}
	})

	t.Run("the fixture's hook runs and denies", func(t *testing.T) {
		out, err := run(`printf '%s' '{"tool_input":{"command":"echo tf-plugin-probe-deny"}}' | sh ` + filepath.Join(plugin, "hooks", "deny.sh"))
		if err != nil || !strings.Contains(out, `"permissionDecision":"deny"`) {
			t.Fatalf("hook did not deny inside the namespace: %v (%s)", err, out)
		}
		out, err = run(`printf '%s' '{"tool_input":{"command":"echo fine"}}' | sh ` + filepath.Join(plugin, "hooks", "deny.sh"))
		if err != nil || out != "" {
			t.Errorf("hook answered a harmless command: %v (%q)", err, out)
		}
	})

	t.Run("a missing plugin fails preflight", func(t *testing.T) {
		missing := spec
		missing.Plugins = []string{filepath.Join(home, "plugins", "gone")}
		if err := Preflight(missing, host); err == nil || !strings.Contains(err.Error(), "local plugin") {
			t.Fatalf("Preflight = %v, want a refusal naming the local plugin", err)
		}
	})
}
