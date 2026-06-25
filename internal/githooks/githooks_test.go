package githooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
)

func TestHostDir_UnderStateRoot(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, "/s")
	if got, want := HostDir(), filepath.Join("/s", "hooks"); got != want {
		t.Errorf("HostDir() = %q, want %q", got, want)
	}
}

func TestEnsure_CreatesDirReadmeKeep(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())

	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	// Idempotent: a second call must not error.
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() second call = %v", err)
	}

	for _, name := range []string{"README.md", ".keep"} {
		if _, err := os.Stat(filepath.Join(HostDir(), name)); err != nil {
			t.Errorf("expected %s in hooks dir: %v", name, err)
		}
	}
}

// TestDirectAgentEnv_IndexBookkeeping pins that the local hooks entry
// lands at the next free GIT_CONFIG index — index 0 over a clean env, and
// bumped past a pre-existing operator GIT_CONFIG_* set so it composes
// rather than clobbering it. The inherited COUNT line is dropped and
// re-emitted bumped, so the result carries exactly one GIT_CONFIG_COUNT
// (no reliance on first-vs-last-wins duplicate-env resolution).
func TestDirectAgentEnv_IndexBookkeeping(t *testing.T) {
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, "/s")
	wantPath := filepath.Join("/s", "hooks")

	// Clean env → index 0, count 1. The non-git entry passes through.
	clean := DirectAgentEnv([]string{"PATH=/usr/bin"})
	assertEnv(t, clean, "PATH", "/usr/bin")
	assertEnv(t, clean, "GIT_CONFIG_KEY_0", ConfigKey)
	assertEnv(t, clean, "GIT_CONFIG_VALUE_0", wantPath)
	assertEnv(t, clean, "GIT_CONFIG_COUNT", "1")
	assertSingleCount(t, clean)

	// Inherited count 2 → our entry at index 2, count bumped to 3, the
	// operator's existing indices left intact, and exactly one COUNT line.
	inherited := []string{
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=http.https://example.com/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic xxx",
		"GIT_CONFIG_KEY_1=core.sshCommand",
		"GIT_CONFIG_VALUE_1=ssh -i /k",
	}
	layered := DirectAgentEnv(inherited)
	assertEnv(t, layered, "GIT_CONFIG_KEY_0", "http.https://example.com/.extraHeader")
	assertEnv(t, layered, "GIT_CONFIG_KEY_1", "core.sshCommand")
	assertEnv(t, layered, "GIT_CONFIG_KEY_2", ConfigKey)
	assertEnv(t, layered, "GIT_CONFIG_VALUE_2", wantPath)
	assertEnv(t, layered, "GIT_CONFIG_COUNT", "3")
	assertSingleCount(t, layered)
}

// TestLocalHookFires is the local-mode F2 acceptance test: a placeholder
// pre-push hook installed only through the process-scoped core.hooksPath
// env (DirectAgentEnv) FIRES on an agent git op, with NO per-worktree git
// config and NO ~/.gitconfig write.
func TestLocalHookFires(t *testing.T) {
	requireGit(t)
	env, marker := setupHooks(t, "#!/bin/sh\necho fired >> \"$TF_HOOK_MARKER\"\nexit 0\n")

	work := newRepoWithRemote(t, env)
	mustGit(t, env, work, "push", "-u", "origin", "main")

	if got := readMarker(t, marker); !strings.Contains(got, "fired") {
		t.Errorf("pre-push hook did not fire: marker = %q", got)
	}
}

// TestLocalHookFires_SubdirClone proves process-scoped coverage: the hook
// fires for a repo the agent clones into a SUBDIR itself — a repo whose
// .git/config never saw core.hooksPath. Only the inherited process env
// carries it, which is exactly the dynamic-workspace (Jira) case F2 must
// cover.
func TestLocalHookFires_SubdirClone(t *testing.T) {
	requireGit(t)
	env, marker := setupHooks(t, "#!/bin/sh\necho fired-subdir >> \"$TF_HOOK_MARKER\"\nexit 0\n")

	// A bare remote with one commit, so a subdir clone has an upstream to
	// push back to.
	work := newRepoWithRemote(t, env)
	mustGit(t, env, work, "push", "-u", "origin", "main")
	remote := mustGit(t, env, work, "remote", "get-url", "origin")
	_ = os.Truncate(marker, 0) // ignore the primary push's marker line

	// Clone into a subdir of the work tree and push a fresh commit from
	// there — same process env, different (never-configured) repo.
	sub := filepath.Join(work, "vendored", "clone")
	if err := os.MkdirAll(filepath.Dir(sub), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, env, work, "clone", strings.TrimSpace(remote), sub)
	configIdentity(t, env, sub)
	writeFile(t, filepath.Join(sub, "b.txt"), "b")
	mustGit(t, env, sub, "add", ".")
	mustGit(t, env, sub, "commit", "-m", "second")
	mustGit(t, env, sub, "push", "origin", "main")

	if got := readMarker(t, marker); !strings.Contains(got, "fired-subdir") {
		t.Errorf("pre-push hook did not fire for subdir clone: marker = %q", got)
	}
}

// TestLocalHooksPath_DoesNotTouchGlobalConfig pins the env-scoped
// guarantee: core.hooksPath resolves to the TF hooks dir for the agent's
// git, yet the operator's global config is never written — there is no
// ~/.gitconfig entry for it.
func TestLocalHooksPath_DoesNotTouchGlobalConfig(t *testing.T) {
	requireGit(t)
	env, _ := setupHooks(t, "#!/bin/sh\nexit 0\n")
	work := newRepoWithRemote(t, env)

	// Effective config (env-injected) resolves to our hooks dir...
	if got := strings.TrimSpace(mustGit(t, env, work, "config", "core.hooksPath")); got != HostDir() {
		t.Errorf("effective core.hooksPath = %q, want %q", got, HostDir())
	}
	// ...but the global config has nothing — `--global core.hooksPath`
	// exits non-zero (unset), and no ~/.gitconfig was created.
	if out, err := runGit(env, work, "config", "--global", "core.hooksPath"); err == nil {
		t.Errorf("global core.hooksPath is set (%q); F2 must be env-scoped only", strings.TrimSpace(out))
	}
	home := envValue(env, "HOME")
	if _, err := os.Stat(filepath.Join(home, ".gitconfig")); err == nil {
		t.Errorf("~/.gitconfig was created at %s; the operator config must be untouched", home)
	}
}

// TestFailingHookDoesNotFailGitOp pins the best-effort contract: a hook
// whose recording side-channel fails but which still exits 0 (the pattern
// the README mandates) must not abort the agent's git op.
func TestFailingHookDoesNotFailGitOp(t *testing.T) {
	requireGit(t)
	// The side-channel (a bogus command) fails, but `|| true` + `exit 0`
	// keeps the hook green — so the push must succeed.
	env, marker := setupHooks(t, "#!/bin/sh\necho fired >> \"$TF_HOOK_MARKER\"\nthis-command-does-not-exist || true\nexit 0\n")
	work := newRepoWithRemote(t, env)

	if out, err := runGit(env, work, "push", "-u", "origin", "main"); err != nil {
		t.Fatalf("push failed despite best-effort hook: %v\n%s", err, out)
	}
	if got := readMarker(t, marker); !strings.Contains(got, "fired") {
		t.Errorf("hook did not run: marker = %q", got)
	}
}

// --- A·3 (TFAC-460): the real embedded pre-push hook --------------------

// TestPrePushHook_RecordsNewAndUpdate drives the actual embedded pre-push
// hook (written by Ensure) over a real git push and asserts it invokes the
// record-push verb with the correct ref/sha and the new-vs-update flag. The
// verb itself is faked by a recorder script wired in as TRIAGE_FACTORY_BIN,
// so this stays a hermetic git-level test with no DB.
func TestPrePushHook_RecordsNewAndUpdate(t *testing.T) {
	requireGit(t)
	env, recLog := setupEmbeddedHook(t, 0)
	work := newRepoWithRemote(t, env)

	// First push creates main on the remote → --new=true.
	mustGit(t, env, work, "push", "-u", "origin", "main")
	got := readMarker(t, recLog)
	for _, want := range []string{"exec git record-push", "--ref refs/heads/main", "--new=true"} {
		if !strings.Contains(got, want) {
			t.Errorf("new-branch push: recorder log %q missing %q", got, want)
		}
	}

	// Second push fast-forwards main → --new=false.
	_ = os.Truncate(recLog, 0)
	writeFile(t, filepath.Join(work, "b.txt"), "b")
	mustGit(t, env, work, "add", ".")
	mustGit(t, env, work, "commit", "-m", "second")
	mustGit(t, env, work, "push", "origin", "main")
	got = readMarker(t, recLog)
	if !strings.Contains(got, "--new=false") {
		t.Errorf("update push: recorder log %q missing --new=false", got)
	}
}

// TestPrePushHook_SkipsDeletes pins that a branch deletion (all-zero local
// sha) records nothing — there's no branch to capture.
func TestPrePushHook_SkipsDeletes(t *testing.T) {
	requireGit(t)
	env, recLog := setupEmbeddedHook(t, 0)
	work := newRepoWithRemote(t, env)
	mustGit(t, env, work, "push", "-u", "origin", "main")

	// Push then delete a throwaway branch; only the delete is observed.
	mustGit(t, env, work, "checkout", "-b", "tmp")
	mustGit(t, env, work, "push", "-u", "origin", "tmp")
	_ = os.Truncate(recLog, 0)
	mustGit(t, env, work, "push", "origin", "--delete", "tmp")

	if got := readMarker(t, recLog); strings.Contains(got, "record-push") {
		t.Errorf("delete push recorded something: %q", got)
	}
}

// TestPrePushHook_RecordFailureDoesNotFailPush pins the best-effort
// contract end-to-end through the real hook: the record-push verb exiting
// non-zero must not abort the push.
func TestPrePushHook_RecordFailureDoesNotFailPush(t *testing.T) {
	requireGit(t)
	env, _ := setupEmbeddedHook(t, 1) // recorder always fails
	work := newRepoWithRemote(t, env)

	if out, err := runGit(env, work, "push", "-u", "origin", "main"); err != nil {
		t.Fatalf("push failed despite best-effort hook: %v\n%s", err, out)
	}
}

// setupEmbeddedHook pins the state root, calls Ensure (which writes the real
// embedded pre-push hook), and wires a recorder script as TRIAGE_FACTORY_BIN
// that appends its argv to recLog and exits recorderExit. Returns the agent
// git env and the recorder log path.
func setupEmbeddedHook(t *testing.T, recorderExit int) (env []string, recLog string) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}

	recLog = filepath.Join(t.TempDir(), "rec.log")
	recorder := filepath.Join(t.TempDir(), "triagefactory")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$REC_LOG\"\nexit " +
		strconv.Itoa(recorderExit) + "\n"
	if err := os.WriteFile(recorder, []byte(body), 0o755); err != nil {
		t.Fatalf("write recorder: %v", err)
	}

	home := t.TempDir()
	base := append(sanitizeEnv(os.Environ()),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		BinEnvVar+"="+recorder,
		"REC_LOG="+recLog,
	)
	return DirectAgentEnv(base), recLog
}

// --- helpers -------------------------------------------------------------

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// setupHooks pins the state root to a temp dir, ensures the hooks dir,
// writes an executable pre-push hook with the given body, and returns the
// process env an agent's git would run under (os.Environ() + a hermetic
// HOME + the DirectAgentEnv core.hooksPath layering) plus the marker path
// the hook appends to.
func setupHooks(t *testing.T, prePushBody string) (env []string, marker string) {
	t.Helper()
	runmode.SetForTest(t, runmode.ModeLocal)
	paths.SetForTest(t, t.TempDir())
	if err := Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	hookPath := filepath.Join(HostDir(), "pre-push")
	if err := os.WriteFile(hookPath, []byte(prePushBody), 0o755); err != nil {
		t.Fatalf("write pre-push hook: %v", err)
	}

	home := t.TempDir()
	marker = filepath.Join(t.TempDir(), "marker")
	// Hermetic base: drop every inherited GIT_* var AND the keys we override
	// below, so the agent git runs from a known-clean environment with no
	// duplicate keys. Stripping the inherited HOME matters specifically: the
	// runner exports its own HOME (e.g. /root), and leaving it in place would
	// give two HOME entries — glibc getenv is first-wins, so git would read
	// the operator's real ~/.gitconfig instead of the temp one, silently
	// breaking hermeticity. Then add a clean HOME, no system config, no
	// credential prompts, and the marker path for the hook.
	base := append(sanitizeEnv(os.Environ()),
		"HOME="+home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"TF_HOOK_MARKER="+marker,
	)
	return DirectAgentEnv(base), marker
}

// sanitizeEnv returns env with every GIT_*-prefixed entry and the keys the
// setup helpers override (HOME, TF_HOOK_MARKER, TRIAGE_FACTORY_BIN, REC_LOG)
// removed, so the test agent env has no duplicate keys and doesn't inherit
// the runner's git/home config or a stray TRIAGE_FACTORY_BIN from the
// developer/CI shell — which, being first-wins under glibc getenv, would
// otherwise shadow the test recorder.
func sanitizeEnv(env []string) []string {
	drop := map[string]struct{}{"HOME": {}, "TF_HOOK_MARKER": {}, BinEnvVar: {}, "REC_LOG": {}}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			if _, ok := drop[kv[:eq]]; ok {
				continue
			}
		}
		out = append(out, kv)
	}
	return out
}

// newRepoWithRemote builds a work repo with a single commit and a bare
// remote wired as origin, returning the work dir.
func newRepoWithRemote(t *testing.T, env []string) string {
	t.Helper()
	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	remote := filepath.Join(dir, "remote.git")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, env, dir, "init", "--bare", remote)
	// Pin the bare remote's default branch to main so a fresh clone checks
	// out main (not the init-default master), making subdir-clone pushes
	// resolve the main refspec.
	mustGit(t, env, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	mustGit(t, env, work, "init", "-b", "main")
	configIdentity(t, env, work)
	writeFile(t, filepath.Join(work, "a.txt"), "a")
	mustGit(t, env, work, "add", ".")
	mustGit(t, env, work, "commit", "-m", "first")
	mustGit(t, env, work, "remote", "add", "origin", remote)
	return work
}

func configIdentity(t *testing.T, env []string, repo string) {
	t.Helper()
	mustGit(t, env, repo, "config", "user.email", "agent@triagefactory.test")
	mustGit(t, env, repo, "config", "user.name", "TF Agent")
}

func runGit(env []string, dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGit(t *testing.T, env []string, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(env, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readMarker(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read marker %s: %v", path, err)
	}
	return string(b)
}

func assertEnv(t *testing.T, env []string, key, want string) {
	t.Helper()
	if got := envValue(env, key); got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

// assertSingleCount fails if env carries more than one GIT_CONFIG_COUNT
// line — the property that frees us from depending on first-vs-last-wins
// duplicate-env resolution.
func assertSingleCount(t *testing.T, env []string) {
	t.Helper()
	n := 0
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_CONFIG_COUNT=") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("GIT_CONFIG_COUNT appears %d times, want exactly 1: %v", n, env)
	}
}

// envValue returns the first value for key in a KEY=VALUE slice. The test
// envs never contain duplicate keys (DirectAgentEnv drops the inherited
// GIT_CONFIG_COUNT and re-emits one; assertSingleCount guards that), so a
// first-match scan is unambiguous — matching the same helper in
// proxies_test.go.
func envValue(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}
