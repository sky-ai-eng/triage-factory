package secretenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve_TrimsOnlyTrailingNewline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	// Leading/inner space is preserved; only the trailing \r\n is stripped.
	if err := os.WriteFile(path, []byte("  lead and space \r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_TEST_SECRET_FILE", path)
	dst := map[string]string{}
	if err := resolveInto(dst, []string{"TF_TEST_SECRET"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got, want := dst["TF_TEST_SECRET"], "  lead and space "; got != want {
		t.Fatalf("value = %q, want %q (only trailing CR/LF trimmed)", got, want)
	}
}

func TestResolve_FileMaterializesAndUnsets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	// Trailing newline is the common secret-file shape; it must be trimmed.
	if err := os.WriteFile(path, []byte("s3cr3t-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_TEST_SECRET_FILE", path)

	dst := map[string]string{}
	if err := resolveInto(dst, []string{"TF_TEST_SECRET"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dst["TF_TEST_SECRET"] != "s3cr3t-value" {
		t.Fatalf("captured value = %q, want %q (trailing newline trimmed)", dst["TF_TEST_SECRET"], "s3cr3t-value")
	}
	// The file-indirection var is unset from the environment.
	if v := os.Getenv("TF_TEST_SECRET_FILE"); v != "" {
		t.Fatalf("TF_TEST_SECRET_FILE still in env = %q; must be unset", v)
	}
}

func TestResolve_PlainEnvIsCapturedAndUnset(t *testing.T) {
	t.Setenv("TF_TEST_SECRET", "plain-env-value")
	dst := map[string]string{}
	if err := resolveInto(dst, []string{"TF_TEST_SECRET"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dst["TF_TEST_SECRET"] != "plain-env-value" {
		t.Fatalf("captured = %q, want the plain env value", dst["TF_TEST_SECRET"])
	}
	// The whole point: even a plain env secret leaves the environment, so a
	// child spawned with os.Environ() never inherits it.
	if v := os.Getenv("TF_TEST_SECRET"); v != "" {
		t.Fatalf("TF_TEST_SECRET still in env = %q; resolve must unset it", v)
	}
}

// The load-bearing property: a child process spawned after Resolve does not
// inherit the secret through the environment.
func TestResolve_ChildProcessDoesNotInherit(t *testing.T) {
	printenv, err := exec.LookPath("printenv")
	if err != nil {
		t.Skip("printenv not available")
	}
	t.Setenv("TF_TEST_SECRET", "leaky-value")

	// Sanity: before resolve, a child DOES see it (proves the test is real).
	if out, _ := exec.Command(printenv, "TF_TEST_SECRET").Output(); strings.TrimSpace(string(out)) != "leaky-value" {
		t.Fatalf("precondition: child should inherit the plain env var, got %q", out)
	}

	if err := resolveInto(map[string]string{}, []string{"TF_TEST_SECRET"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	// After resolve: printenv exits non-zero and prints nothing (var unset), so
	// a child inheriting os.Environ() can't see the secret.
	out, _ := exec.Command(printenv, "TF_TEST_SECRET").Output()
	if strings.Contains(string(out), "leaky-value") {
		t.Fatalf("child inherited the secret after resolve: %q", out)
	}
}

func TestResolve_UnreadableFileFailsFast(t *testing.T) {
	t.Setenv("TF_TEST_SECRET_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	if err := resolveInto(map[string]string{}, []string{"TF_TEST_SECRET"}); err == nil {
		t.Fatal("expected an error for a set-but-unreadable _FILE, got nil")
	}
}

func TestResolve_FileWinsOverEnvWithWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TF_TEST_SECRET_FILE", path)
	t.Setenv("TF_TEST_SECRET", "from-env")

	warned := false
	SetWarnFunc(func(string, ...any) { warned = true })
	t.Cleanup(func() { SetWarnFunc(func(string, ...any) {}) })

	dst := map[string]string{}
	if err := resolveInto(dst, []string{"TF_TEST_SECRET"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if dst["TF_TEST_SECRET"] != "from-file" {
		t.Fatalf("value = %q, want the file to win over env", dst["TF_TEST_SECRET"])
	}
	if !warned {
		t.Fatal("expected a warning when both NAME and NAME_FILE are set")
	}
}

func TestGet_FallsBackToEnvForUncapturedNames(t *testing.T) {
	// A name Resolve never captured is served straight from the environment,
	// so Get is a safe drop-in for os.Getenv on non-secret / pre-Resolve reads.
	t.Setenv("TF_TEST_UNCAPTURED", "env-value")
	if got := Get("TF_TEST_UNCAPTURED"); got != "env-value" {
		t.Fatalf("Get fallback = %q, want the env value", got)
	}
}
