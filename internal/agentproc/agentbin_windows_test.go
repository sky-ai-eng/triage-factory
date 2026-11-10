//go:build windows

package agentproc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeBinary(t *testing.T, path, content string, mtime time.Time) os.FileInfo {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func TestEnsureAgentBinary_LinksAndRefreshesOnUpgrade(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(root, "install", "triagefactory.exe")
	if err := os.MkdirAll(filepath.Dir(self), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	selfInfo := writeBinary(t, self, "version one", now)
	dir := filepath.Join(root, "bin")
	dst := filepath.Join(dir, "triagefactory.exe")

	if err := ensureAgentBinary(self, dir); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("agent binary not created: %v", err)
	}
	if !isCurrentAgentBinary(dstInfo, selfInfo) {
		t.Fatalf("agent binary is neither a link to nor a current copy of the running binary")
	}

	// An unchanged binary is left alone.
	if err := ensureAgentBinary(self, dir); err != nil {
		t.Fatalf("second ensure: %v", err)
	}

	// An upgrade replaces the install with a different file; the agent-visible
	// copy follows it, and the previous one is not left behind.
	upgraded := filepath.Join(root, "install", "triagefactory-new.exe")
	writeBinary(t, upgraded, "version two, longer", now.Add(time.Minute))
	if err := os.Remove(self); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(upgraded, self); err != nil {
		t.Fatal(err)
	}
	if err := ensureAgentBinary(self, dir); err != nil {
		t.Fatalf("ensure after upgrade: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version two, longer" {
		t.Errorf("agent binary holds %q after the upgrade, want the new version", got)
	}
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Errorf("replaced agent binary was not cleaned up: %v", err)
	}
}

// A copy (the fallback where a hardlink cannot be made) is recognised as
// current by size and modification time, and not when either differs.
func TestEnsureAgentBinary_CopyFallbackIsRecognisedAsCurrent(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(root, "self.exe")
	now := time.Now().Truncate(time.Second)
	selfInfo := writeBinary(t, self, "payload", now)
	dir := filepath.Join(root, "bin")
	dst := filepath.Join(dir, "triagefactory.exe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := copyAgentBinary(self, dst, selfInfo); err != nil {
		t.Fatalf("copyAgentBinary: %v", err)
	}
	copied, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(copied, selfInfo) {
		t.Fatal("copy is a link; the test needs a distinct file")
	}
	if !isCurrentAgentBinary(copied, selfInfo) {
		t.Error("a fresh copy is not recognised as current")
	}

	other := writeBinary(t, filepath.Join(root, "other.exe"), "payload", now.Add(-time.Hour))
	if isCurrentAgentBinary(copied, other) {
		t.Error("a copy with a different modification time was taken as current")
	}
	longer := writeBinary(t, filepath.Join(root, "longer.exe"), "payload plus", now)
	if isCurrentAgentBinary(copied, longer) {
		t.Error("a copy with a different size was taken as current")
	}
}

func TestSetPathLeading(t *testing.T) {
	dir := `C:\state\bin`
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"mixed-case key keeps its spelling", []string{"A=1", `Path=C:\Windows;C:\state\bin;C:\Tools`}, `Path=C:\state\bin;C:\Windows;C:\Tools`},
		{"upper-case key", []string{`PATH=C:\Windows`}, `PATH=C:\state\bin;C:\Windows`},
		{"no PATH at all", []string{"A=1"}, `PATH=C:\state\bin`},
		{"entry already present in other case", []string{`Path=C:\STATE\BIN;C:\Windows`}, `Path=C:\state\bin;C:\Windows`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := setPathLeading(tc.env, dir)
			if last := got[len(got)-1]; last != tc.want {
				t.Errorf("PATH entry = %q, want %q (env %v)", last, tc.want, got)
			}
			n := 0
			for _, e := range got {
				if k, _, _ := strings.Cut(e, "="); strings.EqualFold(k, "PATH") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("env carries %d PATH entries, want exactly 1: %v", n, got)
			}
		})
	}
}

// TestNewDirectCommand_PutsTheAgentBinaryOnPath is the end-to-end view of the
// Windows agent binary: the spawn's PATH leads with the state root's bin
// directory, and the bare name resolves there to the running binary.
func TestNewDirectCommand_PutsTheAgentBinaryOnPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cmd, err := newDirectCommand(context.Background(), RunOptions{Cwd: t.TempDir()}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	t.Cleanup(func() { removeArgsFile(cmd) })

	var path string
	for _, e := range cmd.Env {
		if k, v, ok := strings.Cut(e, "="); ok && strings.EqualFold(k, "PATH") {
			path = v
		}
	}
	first, _, _ := strings.Cut(path, string(os.PathListSeparator))
	if !strings.EqualFold(first, agentBinDir()) {
		t.Fatalf("PATH leads with %q, want the TF-owned %q", first, agentBinDir())
	}
	exe, err := os.Stat(filepath.Join(agentBinDir(), "triagefactory.exe"))
	if err != nil {
		t.Fatalf("the bare name does not resolve in the TF-owned bin dir: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	if exe.Size() != selfInfo.Size() {
		t.Errorf("agent binary is %d bytes, the running binary %d", exe.Size(), selfInfo.Size())
	}
}
