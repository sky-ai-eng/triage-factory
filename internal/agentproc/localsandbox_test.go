package agentproc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/githooks"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
)

// stubBwrap puts an always-succeeding `bwrap` on PATH and returns its path.
//
// The tests below assert what the spawn seam BUILDS — the argv shape, the jail
// marker, the folded-in grants, the absolutized node — none of which needs a
// bubblewrap that can really unshare anything. Requiring a working one would
// make the whole group skip on CI, which is the one machine that runs them on
// every commit, so the coverage would exist only where it is least needed.
//
// It wins Resolve deterministically on both kinds of host: it is the PATH hit,
// so it is the sole candidate where no bubblewrap is installed (returned
// unprobed) and the first candidate where one is (its smoke test passes,
// because it exits 0 for the --version probe and the mount argv alike). That
// real bubblewrap actually works is asserted where it belongs — the
// integration test in internal/agentproc/localsandbox, which drives the real
// binary and skips when there is none.
func stubBwrap(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bwrap")
	if err := os.WriteFile(bin, []byte("#!/bin/sh"+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return bin
}

// TestNewDirectCommand_NilSandboxIsUnchanged is the TF_LOCAL_SANDBOX=off
// acceptance in test form: with no Spec, the spawn must be the one that
// shipped before the sandbox existed — same argv, same env, byte for byte.
// Asserted by building the command twice against the same options, once
// through the seam and once against the literal expectation, so a future
// prefix that leaks onto the unsandboxed path fails here rather than in
// somebody's local run.
func TestNewDirectCommand_NilSandboxIsUnchanged(t *testing.T) {
	opts := RunOptions{Cwd: t.TempDir(), ExtraEnv: []string{"TF_TEST_MARKER=1"}}

	cmd, err := newDirectCommand(context.Background(), opts, []string{"wrapper.mjs", "--model", "x"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}

	// argv: node with the wrapper args, and nothing in front of them.
	if want := []string{"node", "wrapper.mjs", "--model", "x"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
	// cmd.Path is exec's PATH resolution of argv[0]; a bwrap prefix would
	// show up here even if Args somehow matched.
	if base := filepath.Base(cmd.Path); base != "node" && base != "node.exe" {
		t.Errorf("cmd.Path = %q, want a resolved `node`", cmd.Path)
	}
	// env: no marker (the child is not in a jail) and no fabricated identity
	// (an unset org identity still means "inherit ambient" out here).
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, SandboxMarkerEnvVar+"=") {
			t.Errorf("unsandboxed env carries the jail marker: %q", kv)
		}
	}
	if cfg := gitConfigMap(t, cmd.Env); cfg["user.name"] != "" || cfg["user.email"] != "" {
		t.Errorf("unsandboxed spawn fabricated a git identity: %v", cfg)
	}
}

// TestNewDirectCommand_SandboxPrefixesBwrapAndMarks pins the three things a
// Spec changes about the spawn: the argv gains the bwrap prefix, node is
// named absolutely (a PATH lookup inside the namespace would miss an
// nvm-installed one under the masked home), and the child carries the marker
// that flips its `tfac exec` children onto the socket.
func TestNewDirectCommand_SandboxPrefixesBwrapAndMarks(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not installed: %v", err)
	}
	root := t.TempDir()
	paths.SetForTest(t, root)
	wantBwrap := stubBwrap(t)
	runRoot := filepath.Join(root, "run")
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd, err := newDirectCommand(context.Background(), RunOptions{
		Cwd:          runRoot,
		LocalSandbox: &localsandbox.Spec{RunRoot: runRoot},
	}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}

	// argv[0] is the RESOLVED bubblewrap, not the bare name: the probe and the
	// spawn route through one resolution so a host with several installed
	// cannot have one blessed at boot and a different one executed per run.
	if cmd.Args[0] != wantBwrap {
		t.Fatalf("argv[0] = %q, want the resolved %q", cmd.Args[0], wantBwrap)
	}
	if !filepath.IsAbs(cmd.Args[0]) {
		t.Errorf("argv[0] = %q, want an absolute path", cmd.Args[0])
	}
	sep := slices.Index(cmd.Args, "--")
	if sep < 0 || sep+1 >= len(cmd.Args) {
		t.Fatalf("argv has no command after the bwrap terminator: %v", cmd.Args)
	}
	if got, want := cmd.Args[sep+1:], []string{nodePath, "wrapper.mjs"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sandboxed command = %v, want %v", got, want)
	}
	if !slices.Contains(cmd.Env, SandboxMarkerEnvVar+"="+SandboxMarkerEnvValue) {
		t.Error("sandboxed child env is missing the jail marker; its exec verbs would open a database instead of the socket")
	}
}

// TestNewDirectCommand_SandboxStripsInheritedMarkerFirst pins that the marker
// on the sandboxed child is OURS. The parent-env strip still has to run — an
// operator's stray export must not survive — so a sandboxed spawn must end up
// with exactly one marker entry rather than the inherited one plus ours.
func TestNewDirectCommand_SandboxStripsInheritedMarkerFirst(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not installed: %v", err)
	}
	t.Setenv(SandboxMarkerEnvVar, "inherited-junk")
	root := t.TempDir()
	paths.SetForTest(t, root)
	stubBwrap(t)
	runRoot := filepath.Join(root, "run")
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	cmd, err := newDirectCommand(context.Background(), RunOptions{
		Cwd:          runRoot,
		LocalSandbox: &localsandbox.Spec{RunRoot: runRoot},
	}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	var markers []string
	for _, kv := range cmd.Env {
		if strings.HasPrefix(kv, SandboxMarkerEnvVar+"=") {
			markers = append(markers, kv)
		}
	}
	if want := []string{SandboxMarkerEnvVar + "=" + SandboxMarkerEnvValue}; !reflect.DeepEqual(markers, want) {
		t.Errorf("marker entries = %v, want %v", markers, want)
	}
}

// TestNewDirectCommand_SandboxSynthesizesFallbackIdentity is D8: the plan
// masks the operator's ~/.gitconfig on purpose, so an org with no configured
// identity would otherwise hit "please tell me who you are" on its first
// commit. A configured identity still wins.
func TestNewDirectCommand_SandboxSynthesizesFallbackIdentity(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not installed: %v", err)
	}
	root := t.TempDir()
	paths.SetForTest(t, root)
	stubBwrap(t)
	runRoot := filepath.Join(root, "run")
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	spec := &localsandbox.Spec{RunRoot: runRoot}

	cmd, err := newDirectCommand(context.Background(), RunOptions{Cwd: runRoot, LocalSandbox: spec}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	cfg := gitConfigMap(t, cmd.Env)
	if cfg["user.name"] != githooks.FallbackIdentityName || cfg["user.email"] != githooks.FallbackIdentityEmail {
		t.Errorf("fallback identity = (%q, %q), want (%q, %q)",
			cfg["user.name"], cfg["user.email"], githooks.FallbackIdentityName, githooks.FallbackIdentityEmail)
	}

	withOrg, err := newDirectCommand(context.Background(), RunOptions{
		Cwd: runRoot, LocalSandbox: spec,
		GitUserName: "acme[bot]", GitUserEmail: "bot@acme.example",
	}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("newDirectCommand: %v", err)
	}
	if cfg := gitConfigMap(t, withOrg.Env); cfg["user.name"] != "acme[bot]" {
		t.Errorf("configured org identity was overridden by the fallback: %v", cfg)
	}
}

// TestHostToolPaths_CoversTheResolvedHostBinaries pins the bind-back
// computation: node, this binary and the SDK install are all resolved rather
// than configured, any of them may sit under the masked home (an nvm node, a
// dev build, an SDK under the state root), and a plan that forgot one would
// fail to exec — or, worse for the TF binary, fail Claude Code's per-tool
// path check before it ever tried.
func TestHostToolPaths_CoversTheResolvedHostBinaries(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	nodePath := "/home/dev/.nvm/versions/node/v22/bin/node"

	got := hostToolPaths(nodePath)

	if !slices.Contains(got, nodePath) {
		t.Errorf("bind-backs %v are missing the resolved node path %q", got, nodePath)
	}
	if !slices.Contains(got, paths.SDKDir()) {
		t.Errorf("bind-backs %v are missing the SDK dir %q", got, paths.SDKDir())
	}
	selfBin, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	if !slices.Contains(got, selfBin) {
		t.Errorf("bind-backs %v are missing this binary %q", got, selfBin)
	}
}

// TestDirectArgv_FoldsInAddDirGrants pins that the --add-dir flags the agent
// is granted and the directories the namespace binds are one list. A caller
// that adds a grant without a bind would hand the agent a flag pointing at a
// tmpfs, which reads as an empty directory rather than as an error.
func TestDirectArgv_FoldsInAddDirGrants(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not installed: %v", err)
	}
	root := t.TempDir()
	paths.SetForTest(t, root)
	stubBwrap(t)
	runRoot := filepath.Join(root, "run")
	grant := filepath.Join(root, "granted")
	for _, d := range []string{runRoot, grant} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	argv, err := directArgv(RunOptions{
		Cwd:          runRoot,
		AddDirs:      []string{grant},
		LocalSandbox: &localsandbox.Spec{RunRoot: runRoot},
	}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("directArgv: %v", err)
	}
	if !hasBind(argv, "--bind", grant) {
		t.Errorf("argv does not bind the --add-dir grant %q: %v", grant, argv)
	}
}

// TestDirectArgv_SandboxFailsRatherThanDegrades pins the posture: a Spec whose
// run tree is gone must fail the spawn. Falling back to an unsandboxed run
// would hand the agent the operator's whole filesystem after isolation had
// been asked for and reported as active.
func TestDirectArgv_SandboxFailsRatherThanDegrades(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node not installed: %v", err)
	}
	paths.SetForTest(t, t.TempDir())
	stubBwrap(t)
	missing := filepath.Join(t.TempDir(), "never-created")

	argv, err := directArgv(RunOptions{
		Cwd:          missing,
		LocalSandbox: &localsandbox.Spec{RunRoot: missing},
	}, []string{"wrapper.mjs"})
	if err == nil {
		t.Fatalf("directArgv = %v, want an error for a run tree that is not on disk", argv)
	}
	// Named, because an error for some unrelated reason — no bubblewrap on the
	// host, say — would satisfy a bare non-nil check while proving nothing
	// about the missing tree this test is here for.
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("directArgv error = %v, want it to name the absent run tree %q", err, missing)
	}
}

// TestDirectArgv_ResolvesNodeAbsolutely pins the fix for a relative PATH
// entry. exec.LookPath joins the matching entry with the name, so PATH=bin:…
// yields a relative path; the plan has no real location to bind for one, and
// the namespace chdirs to the run root where it would name something else.
// Go normally reports ErrDot for that shape, but GODEBUG=execerrdot=0
// suppresses it — so the absolutization, not the error check, is what holds
// here.
func TestDirectArgv_ResolvesNodeAbsolutely(t *testing.T) {
	root := t.TempDir()
	paths.SetForTest(t, root)
	runRoot := filepath.Join(root, "run")
	relBin := filepath.Join(root, "relbin")
	for _, d := range []string{runRoot, relBin} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A stub `node` reachable only through a RELATIVE PATH entry, with the
	// process cwd set so the entry resolves.
	if err := os.WriteFile(filepath.Join(relBin, "node"), []byte("#!/bin/sh"+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	stub := stubBwrap(t)
	t.Chdir(root)
	// The relative entry first, so node resolves through it; the stub's own
	// directory after, so bwrap still resolves without giving node an absolute
	// path to be found by.
	t.Setenv("PATH", "relbin"+string(os.PathListSeparator)+filepath.Dir(stub))
	t.Setenv("GODEBUG", "execerrdot=0")

	argv, err := directArgv(RunOptions{
		Cwd:          runRoot,
		LocalSandbox: &localsandbox.Spec{RunRoot: runRoot},
	}, []string{"wrapper.mjs"})
	if err != nil {
		// A Go build that ignores the GODEBUG still refuses the relative
		// lookup outright, which is the same fail-closed answer the
		// unsandboxed spawn gives — nothing to assert beyond "not a
		// relative path in the argv".
		if strings.Contains(err.Error(), "relative to current directory") {
			t.Skip("this toolchain refuses the relative lookup before absolutization can apply")
		}
		t.Fatalf("directArgv: %v", err)
	}
	sep := slices.Index(argv, "--")
	if sep < 0 || sep+1 >= len(argv) {
		t.Fatalf("argv has no command after the bwrap terminator: %v", argv)
	}
	if got := argv[sep+1]; !filepath.IsAbs(got) {
		t.Errorf("sandboxed command is %q, want an absolute node path", got)
	}
	if !hasBind(argv, "--ro-bind-try", filepath.Join(root, "relbin", "node")) {
		t.Errorf("argv does not bind the resolved node path back through the masks: %v", argv)
	}
}

func hasBind(args []string, flag, target string) bool {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == flag && args[i+1] == target && args[i+2] == target {
			return true
		}
	}
	return false
}

// TestDirectArgv_BindsResolverPathsBack pins that the resolver survives the
// plan the spawn seam actually builds. The pure layer is asserted separately
// against injected paths; this is the wiring — a run whose plan masks /run
// without these binds resolves no hostnames, which reaches the operator as an
// agent that produces nothing and dies at its first completion.
func TestDirectArgv_BindsResolverPathsBack(t *testing.T) {
	paths.SetForTest(t, t.TempDir())
	stubBwrap(t)
	runRoot := t.TempDir()

	argv, err := directArgv(RunOptions{
		Cwd:          runRoot,
		LocalSandbox: &localsandbox.Spec{RunRoot: runRoot},
	}, []string{"wrapper.mjs"})
	if err != nil {
		t.Fatalf("directArgv: %v", err)
	}

	dns := localsandbox.DNSPaths()
	if len(dns) == 0 {
		t.Fatal("DNSPaths returned nothing on a Linux host; the resolver dir is unconditional")
	}
	for _, p := range dns {
		if indexOfOperand(argv, "--ro-bind-try", p) < 0 {
			t.Errorf("plan is missing the resolver bind-back %q:\n%v", p, argv)
		}
	}
	// And the mask it has to survive is still there — a plan that stopped
	// masking /run would pass the loop above for the wrong reason.
	if indexOfOperand(argv, "--tmpfs", "/run") < 0 {
		t.Errorf("plan no longer masks /run:\n%v", argv)
	}
}

// indexOfOperand returns the index of the flag in argv whose first operand is
// target, or -1.
func indexOfOperand(argv []string, flag, target string) int {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag && argv[i+1] == target {
			return i
		}
	}
	return -1
}
