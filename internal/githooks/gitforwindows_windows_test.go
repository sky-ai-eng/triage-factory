//go:build windows

package githooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

// fakeTF stands in for the triagefactory binary the hooks call through
// TRIAGE_FACTORY_BIN. It logs its argv and, for `exec git sign`, writes the
// stand-in signature git reads back from <buffer file>.sig. Git does not check
// the signature at commit time, so the commit's gpgsig header carries it.
const fakeTF = `#!/bin/sh
printf '%s\n' "$*" >> "$REC_LOG"
if [ "$1 $2 $3" = "exec git sign" ]; then
	prev=
	for a in "$@"; do
		if [ "$prev" = "-U" ]; then
			printf -- '-----BEGIN SSH SIGNATURE-----\nfake\n-----END SSH SIGNATURE-----\n' > "$a.sig"
		fi
		prev=$a
	done
fi
exit 0
`

// TestGitForWindows_HooksSigningAndPush drives the installed hooks directory
// through a real Git for Windows: an agent env built by DirectAgentEnv (hooks
// path, org identity, signing config, the Windows git pairs), a real commit
// signed through the ssh-sign shim, a commit trailer from prepare-commit-msg,
// a path past MAX_PATH, and a push to a local bare remote that runs the
// pre-push hook. The hooks are shell scripts, so every side effect asserted
// here proves git found sh and ran them with a TRIAGE_FACTORY_BIN that is a
// Windows path.
func TestGitForWindows_HooksSigningAndPush(t *testing.T) {
	requireGit(t)
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}

	recLog := filepath.Join(t.TempDir(), "rec.log")
	recorder := filepath.Join(t.TempDir(), "triagefactory")
	if err := os.WriteFile(recorder, []byte(fakeTF), 0o755); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(allowed, AllowedSigners("tf@example.com", "ssh-ed25519 AAAAfake"), 0o600); err != nil {
		t.Fatal(err)
	}
	const trailer = "Co-authored-by: alice <alice@users.noreply.github.com>"

	base := append(sanitizeEnv(os.Environ()),
		"HOME="+t.TempDir(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		BinEnvVar+"="+recorder,
		"REC_LOG="+recLog,
		"TRIAGE_FACTORY_GIT_COAUTHOR_TRAILER="+trailer,
	)
	pairs := append(IdentityConfigPairs("tf-bot[bot]", "tf@example.com"),
		SigningConfigPairs(HostDir(), "ssh-ed25519 AAAAfake", allowed)...)
	env := DirectAgentEnv(base, pairs...)

	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, env, dir, "init", "--bare", remote)
	mustGit(t, env, work, "init", "-b", "main")
	mustGit(t, env, work, "remote", "add", "origin", remote)

	// The Windows pairs reach git: longpaths on, the helper list reset to empty.
	if got := strings.TrimSpace(mustGit(t, env, work, "config", "core.longpaths")); got != "true" {
		t.Errorf("core.longpaths = %q, want true", got)
	}
	if out, err := runGit(env, work, "config", "--get-all", "credential.helper"); err != nil || strings.TrimSpace(out) != "" {
		t.Errorf("credential.helper = %q (err %v), want present and empty", out, err)
	}
	if got := envValue(env, "GCM_INTERACTIVE"); got != "never" {
		t.Errorf("GCM_INTERACTIVE = %q, want never", got)
	}

	// A file whose full path is well past the 260-character classic limit.
	deep := work
	for range 6 {
		deep = filepath.Join(deep, strings.Repeat("d", 50))
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(deep, "f.txt"), "deep")
	writeFile(t, filepath.Join(work, "a.txt"), "a")
	mustGit(t, env, work, "add", "-A")

	// commit.gpgsign=true signs this commit; -S names the same thing.
	mustGit(t, env, work, "commit", "-S", "-m", "first")
	commit := mustGit(t, env, work, "cat-file", "commit", "HEAD")
	if !strings.Contains(commit, "gpgsig -----BEGIN SSH SIGNATURE-----") {
		t.Errorf("commit carries no ssh signature:\n%s", commit)
	}
	if !strings.Contains(commit, "tf-bot[bot] <tf@example.com>") {
		t.Errorf("commit is not authored by the injected identity:\n%s", commit)
	}
	if got := strings.Count(commit, trailer); got != 1 {
		t.Errorf("prepare-commit-msg trailer count = %d, want 1:\n%s", got, commit)
	}

	mustGit(t, env, work, "push", "-u", "origin", "main")
	if got := strings.TrimSpace(mustGit(t, env, remote, "rev-parse", "refs/heads/main")); got != strings.TrimSpace(mustGit(t, env, work, "rev-parse", "HEAD")) {
		t.Errorf("remote main = %s, want the pushed HEAD", got)
	}

	log := readMarker(t, recLog)
	for _, want := range []string{"exec git sign -Y sign -n git", "hook check-push", "hook record-push", "--ref refs/heads/main", "--new=true"} {
		if !strings.Contains(log, want) {
			t.Errorf("hook recorder log missing %q:\n%s", want, log)
		}
	}
}

// The Windows pairs layer after an inherited GIT_CONFIG_* set and the http
// extraHeader worktree adds, replacing an inherited GCM_INTERACTIVE.
func TestDirectAgentEnv_WindowsPairs(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, "/s")
	env := DirectAgentEnv([]string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://example.com/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic xxx",
		"GCM_INTERACTIVE=always",
	})
	assertEnv(t, env, "GIT_CONFIG_KEY_0", "http.https://example.com/.extraHeader")
	assertEnv(t, env, "GIT_CONFIG_KEY_1", ConfigKey)
	assertEnv(t, env, "GIT_CONFIG_KEY_2", "core.longpaths")
	assertEnv(t, env, "GIT_CONFIG_VALUE_2", "true")
	assertEnv(t, env, "GIT_CONFIG_KEY_3", "credential.helper")
	assertEnv(t, env, "GIT_CONFIG_VALUE_3", "")
	assertEnv(t, env, "GIT_CONFIG_COUNT", "4")
	assertSingleCount(t, env)
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "GCM_INTERACTIVE=") {
			n++
		}
	}
	if n != 1 || envValue(env, "GCM_INTERACTIVE") != "never" {
		t.Errorf("GCM_INTERACTIVE entries = %d (value %q), want exactly one, never", n, envValue(env, "GCM_INTERACTIVE"))
	}
}
