package githooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestEnsure_WritesTheSignProgram(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	path := filepath.Join(HostDir(), SignProgram)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", SignProgram, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is %v; git cannot run a gpg.ssh.program that is not executable", SignProgram, info.Mode().Perm())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", SignProgram, err)
	}
	if string(got) != string(signProgram) {
		t.Errorf("%s on disk is not the embedded program", SignProgram)
	}
}

// Git calls its gpg.ssh.program to check signatures as well as to make them.
// The shim hands only the signing call to TF, with git's arguments as they
// came, and every other call to ssh-keygen, so git verify-commit and git log
// --show-signature work inside a run.
func TestSignProgram_RoutesSigningToTFAndTheRestToSSHKeygen(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	logDir := t.TempDir()
	fake := func(dir, name string) {
		t.Helper()
		body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + filepath.Join(logDir, name) + "\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}
	binDir := t.TempDir()
	fake(binDir, "triagefactory")
	fake(binDir, "ssh-keygen")

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(HostDir(), SignProgram), args...)
		cmd.Env = append(sanitizeEnv(os.Environ()), "PATH="+binDir+string(os.PathListSeparator)+"/usr/bin:/bin",
			BinEnvVar+"="+filepath.Join(binDir, "triagefactory"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %s: %v\n%s", SignProgram, strings.Join(args, " "), err, out)
		}
	}
	run("-Y", "sign", "-n", "git", "-f", "/tmp/key", "-U", "/tmp/buffer")
	run("-Y", "find-principals", "-f", "/run/allowed", "-s", "/tmp/sig", "-Overify-time=20261026000000")
	run("-Y", "verify", "-n", "git", "-f", "/run/allowed", "-I", "a@example.com", "-s", "/tmp/sig")
	run("-Y", "check-novalidate", "-n", "git", "-s", "/tmp/sig")

	read := func(name string) string {
		raw, _ := os.ReadFile(filepath.Join(logDir, name))
		return string(raw)
	}
	if got, want := read("triagefactory"), "exec git sign -Y sign -n git -f /tmp/key -U /tmp/buffer\n"; got != want {
		t.Errorf("TF was called with:\n%s\nwant\n%s", got, want)
	}
	want := "-Y find-principals -f /run/allowed -s /tmp/sig -Overify-time=20261026000000\n" +
		"-Y verify -n git -f /run/allowed -I a@example.com -s /tmp/sig\n" +
		"-Y check-novalidate -n git -s /tmp/sig\n"
	if got := read("ssh-keygen"); got != want {
		t.Errorf("ssh-keygen was called with:\n%s\nwant\n%s", got, want)
	}
}

func TestSigningConfigPairs(t *testing.T) {
	got := SigningConfigPairs("/run/tf-hooks", "ssh-ed25519 AAAAC3", "/run/tf-allowed-signers")
	want := [][2]string{
		{"gpg.format", "ssh"},
		{"commit.gpgsign", "true"},
		{"user.signingkey", "key::ssh-ed25519 AAAAC3"},
		{"gpg.ssh.program", "/run/tf-hooks/ssh-sign"},
		{"gpg.ssh.allowedSignersFile", "/run/tf-allowed-signers"},
	}
	if len(got) != len(want) {
		t.Fatalf("SigningConfigPairs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("pair %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestAllowedSigners(t *testing.T) {
	if got, want := string(AllowedSigners("9+tf@users.noreply.github.com", "ssh-ed25519 AAAA1", "")),
		"9+tf@users.noreply.github.com namespaces=\"git\" ssh-ed25519 AAAA1\n"; got != want {
		t.Errorf("one key:\n%s\nwant\n%s", got, want)
	}
	if got, want := string(AllowedSigners("a@example.com", "ssh-ed25519 AAAA1", "ssh-ed25519 AAAA2")),
		"a@example.com namespaces=\"git\" ssh-ed25519 AAAA1\na@example.com namespaces=\"git\" ssh-ed25519 AAAA2\n"; got != want {
		t.Errorf("a rotation:\n%s\nwant\n%s", got, want)
	}
}
