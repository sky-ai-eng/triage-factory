package gitssh

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dispatchEnv makes the re-executed test binary act as `triagefactory
// git-ssh`; a copy named ssh acts as the real ssh instead. Both are the test
// binary itself, so no second program has to be built.
const dispatchEnv = "GITSSH_TEST_DISPATCH"

const fakeSSHExitCode = 7

func TestMain(m *testing.M) {
	if strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe") == "ssh" {
		in, _ := io.ReadAll(os.Stdin)
		fmt.Fprintf(os.Stdout, "args=%s\n", strings.Join(os.Args[1:], "|"))
		if _, err := os.Stdout.Write(in); err != nil {
			fmt.Fprintf(os.Stderr, "fake ssh: %v\n", err)
			os.Exit(1)
		}
		os.Exit(fakeSSHExitCode)
	}
	if os.Getenv(dispatchEnv) == "1" {
		Handle(os.Args[1:])
	}
	os.Exit(m.Run())
}

// TestExecSSH_PropagatesExitStatusAndStdio pins the passthrough arm's contract
// with git: the real ssh's stdin, stdout and exit status are the dispatcher's.
func TestExecSSH_PropagatesExitStatusAndStdio(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	binDir := t.TempDir()
	fakeSSH := filepath.Join(binDir, "ssh")
	if os.PathSeparator == '\\' {
		fakeSSH += ".exe"
	}
	src, err := os.ReadFile(self)
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	if err := os.WriteFile(fakeSSH, src, 0o755); err != nil {
		t.Fatalf("install fake ssh: %v", err)
	}

	// An unconfigured env means no host is bridged, so every argv passes through.
	cmd := exec.Command(self, "-p", "2222", "other.example", "git-upload-pack 'a/b'")
	cmd.Env = append(os.Environ(),
		dispatchEnv+"=1",
		UpstreamHostEnvVar+"=",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	cmd.Stdin = strings.NewReader("pkt-in")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected a non-zero exit from the fake ssh, got err=%v stderr=%q", err, stderr.String())
	}
	if got := exitErr.ExitCode(); got != fakeSSHExitCode {
		t.Fatalf("exit code = %d, want %d (stderr=%q)", got, fakeSSHExitCode, stderr.String())
	}
	want := "args=-p|2222|other.example|git-upload-pack 'a/b'\npkt-in"
	if stdout.String() != want {
		t.Fatalf("stdout = %q, want %q", stdout.String(), want)
	}
}

// TestExecSSH_MissingSSHExits127 pins the start-failure status git sees when
// no ssh is installed.
func TestExecSSH_MissingSSHExits127(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locate test binary: %v", err)
	}
	cmd := exec.Command(self, "other.example", "git-upload-pack 'a/b'")
	cmd.Env = append(os.Environ(), dispatchEnv+"=1", "PATH="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err = cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 127 {
		t.Fatalf("expected exit 127, got err=%v stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "ssh not found") {
		t.Fatalf("stderr = %q, want the ssh-not-found message", stderr.String())
	}
}
