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

func writeBinary(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestEnsureAgentBinary_CopiesAndRefreshesOnUpgrade(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(root, "install", "triagefactory.exe")
	if err := os.MkdirAll(filepath.Dir(self), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	writeBinary(t, self, "version one", now)
	dir := filepath.Join(root, "bin")
	dst := filepath.Join(dir, "triagefactory.exe")

	if err := ensureAgentBinary(self, dir); err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if got := readFile(t, dst); got != "version one" {
		t.Fatalf("agent binary holds %q, want a copy of the running binary", got)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(dstInfo, selfInfo) {
		t.Fatal("agent binary is a link to the installed binary; a write through it would modify the install")
	}

	// An unchanged binary is left alone: the file is not replaced.
	before := dstInfo.ModTime()
	time.Sleep(20 * time.Millisecond)
	if err := ensureAgentBinary(self, dir); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if after, err := os.Stat(dst); err != nil || !after.ModTime().Equal(before) {
		t.Fatalf("an unchanged binary was rewritten (err %v)", err)
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
	if got := readFile(t, dst); got != "version two, longer" {
		t.Errorf("agent binary holds %q after the upgrade, want the new version", got)
	}
	if _, err := os.Stat(dst + ".old"); !os.IsNotExist(err) {
		t.Errorf("replaced agent binary was not cleaned up: %v", err)
	}
}

// A copy the agent rewrote with same-sized content and a reset modification
// time is not current: the content decides, not the stamp.
func TestEnsureAgentBinary_ReplacesATamperedCopy(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(root, "self.exe")
	now := time.Now().Truncate(time.Second)
	writeBinary(t, self, "payload", now)
	dir := filepath.Join(root, "bin")
	dst := filepath.Join(dir, "triagefactory.exe")
	if err := ensureAgentBinary(self, dir); err != nil {
		t.Fatal(err)
	}

	writeBinary(t, dst, "PAYLOAD", now)
	if err := ensureAgentBinary(self, dir); err != nil {
		t.Fatalf("ensure over a tampered copy: %v", err)
	}
	if got := readFile(t, dst); got != "payload" {
		t.Errorf("tampered agent binary holds %q, want it replaced with the running binary", got)
	}
	if got := readFile(t, self); got != "payload" {
		t.Errorf("running binary holds %q, want it untouched", got)
	}
}

// A hardlink to the running binary, such as an earlier version left behind,
// aliases the install, so it is replaced by a copy and the install survives a
// later write through the agent-visible path.
func TestEnsureAgentBinary_ReplacesALinkToTheInstall(t *testing.T) {
	root := t.TempDir()
	self := filepath.Join(root, "self.exe")
	writeBinary(t, self, "payload", time.Now().Truncate(time.Second))
	dir := filepath.Join(root, "bin")
	dst := filepath.Join(dir, "triagefactory.exe")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(self, dst); err != nil {
		t.Skipf("hardlinks unavailable here: %v", err)
	}

	if err := ensureAgentBinary(self, dir); err != nil {
		t.Fatalf("ensure over a link: %v", err)
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	dstInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(dstInfo, selfInfo) {
		t.Fatal("the agent binary is still a link to the installed binary")
	}
	if err := os.WriteFile(dst, []byte("agent wrote this"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, self); got != "payload" {
		t.Errorf("a write through the agent binary changed the install to %q", got)
	}
}

// The running binary is hashed once and the digest reused while the file is the
// same; a file replaced at the same path is hashed again.
func TestOwnBinaryDigest_IsCachedAndFollowsTheFile(t *testing.T) {
	self := filepath.Join(t.TempDir(), "self.exe")
	now := time.Now().Truncate(time.Second)
	writeBinary(t, self, "one", now)
	info, err := os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	agentBinaryMu.Lock()
	defer agentBinaryMu.Unlock()
	first, err := ownBinaryDigest(self, info)
	if err != nil {
		t.Fatal(err)
	}

	// Same identity: the cached digest is returned without reading the file,
	// which is gone from disk here.
	if err := os.Remove(self); err != nil {
		t.Fatal(err)
	}
	again, err := ownBinaryDigest(self, info)
	if err != nil || again != first {
		t.Fatalf("cached digest = %q (err %v), want %q", again, err, first)
	}

	writeBinary(t, self, "two!", now.Add(time.Minute))
	info, err = os.Stat(self)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := ownBinaryDigest(self, info)
	if err != nil || changed == first {
		t.Fatalf("digest after the file changed = %q (err %v), want a new one", changed, err)
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

func TestSetEnvVar_ReplacesEverySpelling(t *testing.T) {
	got := setEnvVar([]string{"A=1", "nodefaultcurrentdirectoryinexepath=0", "B=2"}, noDefaultCwdInExePathEnv, "1")
	n := 0
	for _, e := range got {
		if k, v, _ := strings.Cut(e, "="); strings.EqualFold(k, noDefaultCwdInExePathEnv) {
			n++
			if v != "1" {
				t.Errorf("value = %q, want 1", v)
			}
		}
	}
	if n != 1 || len(got) != 3 {
		t.Errorf("env = %v, want the inherited spelling replaced by one entry", got)
	}
}

// TestNewDirectCommand_PutsTheAgentBinaryOnPath is the end-to-end view of the
// Windows agent binary: the spawn's PATH leads with the state root's bin
// directory, the bare name resolves there to a copy of the running binary, and
// the executable search skips the working directory.
func TestNewDirectCommand_PutsTheAgentBinaryOnPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cmd, err := newDirectCommand(context.Background(), RunOptions{Cwd: t.TempDir()}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	t.Cleanup(func() { removeArgsFile(cmd) })

	var path, noCwd string
	for _, e := range cmd.Env {
		k, v, ok := strings.Cut(e, "=")
		switch {
		case ok && strings.EqualFold(k, "PATH"):
			path = v
		case ok && strings.EqualFold(k, noDefaultCwdInExePathEnv):
			noCwd = v
		}
	}
	first, _, _ := strings.Cut(path, string(os.PathListSeparator))
	if !strings.EqualFold(first, agentBinDir()) {
		t.Fatalf("PATH leads with %q, want the TF-owned %q", first, agentBinDir())
	}
	if noCwd != "1" {
		t.Errorf("%s = %q, want 1", noDefaultCwdInExePathEnv, noCwd)
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
	if os.SameFile(exe, selfInfo) {
		t.Errorf("agent binary is the running binary itself, not a copy")
	}
}
