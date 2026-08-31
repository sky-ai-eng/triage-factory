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

// TestResolve_DeploymentGitHubAppSecrets: the deployment GitHub App's three
// undisclosable halves are registered here, and each resolves through the
// NAME_FILE convention. The file form is the one that matters for these: a
// multi-line PEM is awkward in a .env and natural as a mounted file, and only a
// NAME_FILE value stays out of this process's own /proc/environ.
//
// It resolves through the real Secrets slice rather than a local list, so
// dropping a name from that slice fails here rather than silently reverting the
// secret to plain-env handling.
func TestResolve_DeploymentGitHubAppSecrets(t *testing.T) {
	names := []string{
		"TF_GITHUB_APP_PRIVATE_KEY",
		"TF_GITHUB_APP_WEBHOOK_SECRET",
		"TF_GITHUB_APP_CLIENT_SECRET",
	}
	// The App ID is not a secret and must not be captured here — it is ordinary
	// config, and a name in this list is a name Resolve unsets.
	for _, name := range append(names, "TF_GITHUB_APP_ID") {
		registered := false
		for _, s := range Secrets {
			if s == name {
				registered = true
				break
			}
		}
		if want := name != "TF_GITHUB_APP_ID"; registered != want {
			t.Errorf("Secrets contains %s = %v, want %v", name, registered, want)
		}
	}

	dir := t.TempDir()
	for i, name := range names {
		path := filepath.Join(dir, name)
		// Trailing newline is the shape a mounted secret file has; a PEM's
		// internal newlines must survive it.
		if err := os.WriteFile(path, []byte(secretFileValue(i)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(name+"_FILE", path)
	}

	dst := map[string]string{}
	if err := resolveInto(dst, Secrets); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	for i, name := range names {
		if got, want := dst[name], secretFileValue(i); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
		if v := os.Getenv(name + "_FILE"); v != "" {
			t.Errorf("%s_FILE still in env = %q; resolve must unset it", name, v)
		}
	}
}

// secretFileValue makes each secret's file content distinguishable, and gives
// the first one internal newlines so the PEM case is actually exercised.
func secretFileValue(i int) string {
	if i == 0 {
		return "-----BEGIN RSA PRIVATE KEY-----\nMIIEow\nAAAA\n-----END RSA PRIVATE KEY-----"
	}
	return "value-" + strings.Repeat("x", i)
}
