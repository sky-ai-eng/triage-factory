//go:build windows

package agentproc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitInstall lays out a fake Git for Windows: <root>\bin\bash.exe plus git.exe
// at each of the places the real installer puts it.
func gitInstall(t *testing.T) (cmdGit, mingwGit, bash string) {
	t.Helper()
	root := t.TempDir()
	cmdGit = filepath.Join(root, "cmd", "git.exe")
	mingwGit = filepath.Join(root, "mingw64", "bin", "git.exe")
	bash = filepath.Join(root, "bin", "bash.exe")
	for _, p := range []string{cmdGit, mingwGit, bash} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return cmdGit, mingwGit, bash
}

func lookGit(path string) func(string) (string, error) {
	return func(name string) (string, error) {
		if name != "git" {
			return "", errors.New("unexpected lookup of " + name)
		}
		return path, nil
	}
}

func TestFindGitBash_DerivesFromGit(t *testing.T) {
	cmdGit, mingwGit, bash := gitInstall(t)
	for name, git := range map[string]string{"cmd dir": cmdGit, "mingw64 bin dir": mingwGit} {
		got, err := findGitBash("", lookGit(git))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != bash {
			t.Errorf("%s: bash = %q, want %q", name, got, bash)
		}
	}
}

func TestFindGitBash_ExplicitSettingWins(t *testing.T) {
	cmdGit, _, _ := gitInstall(t)
	other := filepath.Join(t.TempDir(), "bash.exe")
	if err := os.WriteFile(other, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := findGitBash(other, lookGit(cmdGit))
	if err != nil || got != other {
		t.Fatalf("findGitBash(setting) = (%q, %v), want %q", got, err, other)
	}
}

func TestFindGitBash_RefusalsNameTheFix(t *testing.T) {
	cmdGit, _, _ := gitInstall(t)
	noBash := filepath.Join(t.TempDir(), "cmd", "git.exe")
	missing := filepath.Join(t.TempDir(), "nope", "bash.exe")
	gitAbsent := func(string) (string, error) { return "", errors.New("not found") }

	for name, run := range map[string]func() (string, error){
		"setting names no file": func() (string, error) { return findGitBash(missing, lookGit(cmdGit)) },
		"git not on PATH":       func() (string, error) { return findGitBash("", gitAbsent) },
		"git without a bash":    func() (string, error) { return findGitBash("", lookGit(noBash)) },
	} {
		got, err := run()
		if err == nil {
			t.Errorf("%s: resolved %q, want an error", name, got)
			continue
		}
		if !strings.Contains(err.Error(), envGitBashPath) {
			t.Errorf("%s: error does not name %s: %v", name, envGitBashPath, err)
		}
	}
	if _, err := findGitBash("", gitAbsent); err == nil || !strings.Contains(err.Error(), "Git for Windows") {
		t.Errorf("a missing git does not point at Git for Windows: %v", err)
	}
}

// The preflight is what refuses a host with no shell for the agent, and it
// runs even when the SDK is already installed.
func TestCheckGitBash_RefusesWithoutAShell(t *testing.T) {
	t.Setenv(envGitBashPath, filepath.Join(t.TempDir(), "absent", "bash.exe"))
	if err := checkGitBash(); err == nil {
		t.Error("checkGitBash accepted a CLAUDE_CODE_GIT_BASH_PATH that names no file")
	}
}

func TestGitBashEnv_ExportsTheResolvedPath(t *testing.T) {
	_, _, bash := gitInstall(t)
	t.Setenv(envGitBashPath, bash)
	env := gitBashEnv()
	if len(env) != 1 || env[0] != envGitBashPath+"="+bash {
		t.Errorf("gitBashEnv = %v, want [%s=%s]", env, envGitBashPath, bash)
	}
}
