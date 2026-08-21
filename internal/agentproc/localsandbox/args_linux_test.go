//go:build linux

package localsandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// testHost is the layout every case below is computed against — fixed
// literals rather than the developer's real home, so the golden argv is the
// same on every machine.
func testHost() Host {
	return Host{
		Home:      "/home/dev",
		ClaudeDir: "/home/dev/.claude",
		StateRoot: "/home/dev/.triagefactory",
		HooksDir:  "/home/dev/.triagefactory/hooks",
		GHBinDir:  "/home/dev/.triagefactory/bin",
		TempDir:   "/tmp",
		Cwd:       "/tmp/triagefactory-runs/rk1",
	}
}

// TestArgs_MountPlan is the golden for the whole plan. It is written as one
// exact slice rather than a set of contains-assertions because ORDER is the
// policy here: bwrap applies operations in argv order, so a mask that drifts
// below the bind-back it was supposed to hide, or a grant that drifts above
// it, silently changes what the agent can reach.
func TestArgs_MountPlan(t *testing.T) {
	spec := Spec{
		RunRoot:         "/tmp/triagefactory-runs/rk1",
		GHChannelDir:    "/home/dev/.triagefactory/gh-channel/conv-1",
		AgentHostSocket: "/home/dev/.triagefactory/agenthost/conv-1.sock",
		AddDirs:         []string{"/home/dev/notes", "/srv/shared"},
		ReadOnly:        []string{"/home/dev/.nvm/versions/node/v22/bin/node", "/home/dev/src/tf/triagefactory"},
	}

	got, err := Args(spec, testHost())
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	want := []string{
		"bwrap",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		// Every mask first, sorted so a parent precedes its children.
		"--tmpfs", "/home/dev",
		"--tmpfs", "/home/dev/.triagefactory",
		"--tmpfs", "/run",
		"--tmpfs", "/tmp",
		// Then everything that comes back through them.
		"--bind-try", "/home/dev/.claude", "/home/dev/.claude",
		"--ro-bind-try", "/home/dev/.triagefactory/hooks", "/home/dev/.triagefactory/hooks",
		"--ro-bind-try", "/home/dev/.triagefactory/bin", "/home/dev/.triagefactory/bin",
		"--bind-try", "/home/dev/.triagefactory/gh-channel/conv-1", "/home/dev/.triagefactory/gh-channel/conv-1",
		"--bind", "/tmp/triagefactory-runs/rk1", "/tmp/triagefactory-runs/rk1",
		"--bind", "/home/dev/.triagefactory/agenthost/conv-1.sock", "/run/tf.sock",
		"--ro-bind-try", "/home/dev/.nvm/versions/node/v22/bin/node", "/home/dev/.nvm/versions/node/v22/bin/node",
		"--ro-bind-try", "/home/dev/src/tf/triagefactory", "/home/dev/src/tf/triagefactory",
		// Grants last.
		"--bind", "/home/dev/notes", "/home/dev/notes",
		"--bind", "/srv/shared", "/srv/shared",
		"--die-with-parent",
		"--chdir", "/tmp/triagefactory-runs/rk1",
		"--",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mount plan drifted.\n got: %v\nwant: %v", got, want)
	}
}

// TestArgs_AddDirsWinOverTheMasks is D9's load-bearing property stated on its
// own: a grant under a masked tree has to be emitted AFTER the tmpfs that
// would hide it, or the operator's explicitly granted directory shows up
// empty and the agent reads that emptiness as an answer.
func TestArgs_AddDirsWinOverTheMasks(t *testing.T) {
	got, err := Args(Spec{
		RunRoot: "/tmp/triagefactory-runs/rk1",
		AddDirs: []string{"/home/dev/notes"},
	}, testHost())
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	maskAt := indexOfOp(got, "--tmpfs", "/home/dev")
	grantAt := indexOfOp(got, "--bind", "/home/dev/notes")
	if maskAt < 0 || grantAt < 0 {
		t.Fatalf("expected both the home mask and the grant in %v", got)
	}
	if grantAt < maskAt {
		t.Errorf("--add-dir grant at %d precedes the home tmpfs at %d; the tmpfs would hide it", grantAt, maskAt)
	}
}

// TestArgs_MasksTMPDIRAlongsideTmp covers a TMPDIR that isn't /tmp: both are
// masked, because the run tree lives under the former and the machine's
// shared temp is the latter, and an agent must be kept out of each.
func TestArgs_MasksTMPDIRAlongsideTmp(t *testing.T) {
	host := testHost()
	host.TempDir = "/var/tmp/dev"
	got, err := Args(Spec{RunRoot: "/var/tmp/dev/triagefactory-runs/rk1"}, host)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	for _, dir := range []string{"/tmp", "/var/tmp/dev"} {
		if indexOfOp(got, "--tmpfs", dir) < 0 {
			t.Errorf("plan does not mask %s: %v", dir, got)
		}
	}
	if runAt, maskAt := indexOfOp(got, "--bind", "/var/tmp/dev/triagefactory-runs/rk1"), indexOfOp(got, "--tmpfs", "/var/tmp/dev"); runAt < maskAt {
		t.Errorf("run root bound at %d before its own tmpfs at %d", runAt, maskAt)
	}
	// The grouping is the property, not the particular nesting: no bind may
	// precede any mask, or a plan is correct only for the way one host's
	// directories happen to nest.
	if lastMask, firstBind := lastIndexOfFlag(got, "--tmpfs"), firstIndexOfBind(got); firstBind < lastMask {
		t.Errorf("a bind at %d precedes the last mask at %d: %v", firstBind, lastMask, got)
	}
}

// TestArgs_OmitsAbsentOptionalPaths pins that a run with no gh channel and no
// agenthost socket produces a plan with no dangling binds — bwrap fails a
// bind whose source is an empty string, so the omission is what keeps an
// unprovisioned channel a degraded run rather than a failed one.
func TestArgs_OmitsAbsentOptionalPaths(t *testing.T) {
	got, err := Args(Spec{RunRoot: "/tmp/triagefactory-runs/rk1"}, testHost())
	if err != nil {
		t.Fatalf("Args: %v", err)
	}
	joined := strings.Join(got, " ")
	if strings.Contains(joined, AgentHostSocketDest) {
		t.Errorf("plan binds %s with no socket in the Spec: %v", AgentHostSocketDest, got)
	}
	if strings.Contains(joined, "gh-channel") {
		t.Errorf("plan binds a gh-channel dir the Spec never named: %v", got)
	}
	for i, arg := range got {
		if arg == "" {
			t.Fatalf("empty argv entry at %d: %v", i, got)
		}
	}
}

// TestArgs_RejectsIncompleteSpecs pins that the builder refuses a plan it
// cannot make safe, rather than emitting one bwrap will interpret loosely.
func TestArgs_RejectsIncompleteSpecs(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		host Host
	}{
		{name: "no run root", spec: Spec{}, host: testHost()},
		{name: "no home", spec: Spec{RunRoot: "/tmp/r"}, host: Host{Cwd: "/tmp/r"}},
		{name: "no cwd", spec: Spec{RunRoot: "/tmp/r"}, host: Host{Home: "/home/dev"}},
		{name: "relative grant", spec: Spec{RunRoot: "/tmp/r", AddDirs: []string{"notes"}}, host: testHost()},
		{name: "relative bind-back", spec: Spec{RunRoot: "/tmp/r", ReadOnly: []string{"node"}}, host: testHost()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got, err := Args(c.spec, c.host); err == nil {
				t.Errorf("Args = %v, want an error", got)
			}
		})
	}
}

// TestPreflight_NamesAMissingGrant is the other half of D9: a grant whose
// directory has gone away fails the run with its own path in the message,
// never a silent skip that leaves the agent reading an empty directory.
func TestPreflight_NamesAMissingGrant(t *testing.T) {
	root := t.TempDir()
	host := testHost()
	host.ClaudeDir = filepath.Join(root, "claude")
	missing := filepath.Join(root, "gone")

	err := Preflight(Spec{RunRoot: root, AddDirs: []string{missing}}, host)
	if err == nil {
		t.Fatal("Preflight accepted a grant that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("Preflight error %q does not name the missing path %q", err, missing)
	}
}

// TestPreflight_CreatesClaudeDir pins the one directory Preflight makes
// rather than demands: without it the bind has no source, and the run's
// transcript lands in a tmpfs that vanishes with the namespace — taking the
// park-and-resume path's warm cache with it.
func TestPreflight_CreatesClaudeDir(t *testing.T) {
	root := t.TempDir()
	host := testHost()
	host.ClaudeDir = filepath.Join(root, "claude")

	if err := Preflight(Spec{RunRoot: root}, host); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if info, err := os.Stat(host.ClaudeDir); err != nil || !info.IsDir() {
		t.Fatalf("Preflight did not create %s (err=%v)", host.ClaudeDir, err)
	}
}

// lastIndexOfFlag returns the index of the last occurrence of flag, or -1.
func lastIndexOfFlag(args []string, flag string) int {
	last := -1
	for i, a := range args {
		if a == flag {
			last = i
		}
	}
	return last
}

// firstIndexOfBind returns the index of the first bind-family operation, or
// len(args) when there is none.
func firstIndexOfBind(args []string) int {
	for i, a := range args {
		switch a {
		case "--bind", "--bind-try", "--ro-bind-try":
			return i
		}
	}
	return len(args)
}

// indexOfOp returns the index of the flag in args whose first operand is
// target, or -1.
func indexOfOp(args []string, flag, target string) int {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == target {
			return i
		}
	}
	return -1
}

// TestProbe_ClassifiesTheFailure pins that Probe reports WHICH step failed,
// which is what lets the boot refusal name a fix that could actually work. A
// probe that returned one undifferentiated error is how an operator ends up
// installing a package they already have.
func TestProbe_ClassifiesTheFailure(t *testing.T) {
	// No bwrap on PATH and none at the distro path — the not-installed shape.
	// Both have to be cut: falling back to the distro path when PATH misses is
	// exactly the behavior Resolve adds, so an empty PATH alone no longer
	// means "not installed" on a machine that has the package.
	t.Setenv("PATH", t.TempDir())
	restore := distroBinary
	distroBinary = filepath.Join(t.TempDir(), "no-bwrap-here")
	t.Cleanup(func() { distroBinary = restore })

	err := Probe()
	if err == nil {
		t.Fatal("Probe succeeded with no bwrap on PATH")
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("Probe error = %v, want ErrNotInstalled", err)
	}
	if errors.Is(err, ErrNamespaceRefused) {
		t.Errorf("a missing binary was classified as a refused namespace: %v", err)
	}
}

// TestUserNSDiagnostics_ReportsEveryKnob pins that the refusal carries a
// reading for each toggle that gates an unprivileged user namespace, absent
// ones included — an absent apparmor_restrict_unprivileged_userns is itself
// the answer to "is this the Ubuntu 23.10+ AppArmor block?", so reporting
// nothing for it would drop the most useful line.
func TestUserNSDiagnostics_ReportsEveryKnob(t *testing.T) {
	got := userNSDiagnostics()
	if len(got) != len(userNSKnobs) {
		t.Fatalf("diagnostics = %v, want one entry per knob (%d)", got, len(userNSKnobs))
	}
	for i, knob := range userNSKnobs {
		name := filepath.Base(knob)
		if !strings.HasPrefix(got[i], name+"=") {
			t.Errorf("entry %d = %q, want a %s= reading", i, got[i], name)
		}
		if strings.TrimPrefix(got[i], name+"=") == "" {
			t.Errorf("entry %d = %q has no value; an unreadable knob must report <absent>", i, got[i])
		}
	}
}

// TestResolve_PrefersABwrapThatWorks is the point of Resolve. A snap, nix or
// hand-built bwrap earlier on PATH is unconfined by the distro AppArmor
// profile and gets denied a namespace on Ubuntu 23.10+, while the distro
// binary two directories over works. Ranking the candidates would be a guess;
// trying them is not, and it turns a boot refusal that asks the operator to go
// audit their PATH into a boot that simply succeeds.
func TestResolve_PrefersABwrapThatWorks(t *testing.T) {
	real, err := exec.LookPath(Binary)
	if err != nil {
		t.Skipf("no bubblewrap installed: %v", err)
	}
	if err := smokeTest(real); err != nil {
		t.Skipf("bubblewrap unusable here: %v", err)
	}

	// A broken bwrap that shadows the working one on PATH, standing in for the
	// unconfined-binary case (it fails for a different reason; what Resolve
	// sees either way is a candidate whose smoke test does not pass).
	shadowDir := t.TempDir()
	shadow := filepath.Join(shadowDir, Binary)
	if err := os.WriteFile(shadow, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shadowDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	restore := distroBinary
	distroBinary = real
	t.Cleanup(func() { distroBinary = restore })

	got, err := Resolve()
	if err != nil {
		t.Fatalf("Resolve rejected the host even though a working bubblewrap is installed: %v", err)
	}
	if got == shadow {
		t.Errorf("Resolve picked the broken shadowing binary %q", got)
	}
	if got != real {
		t.Errorf("Resolve = %q, want the working %q", got, real)
	}
	// And the whole point: the boot gate passes rather than refusing.
	if err := Probe(); err != nil {
		t.Errorf("Probe failed with a working bubblewrap present: %v", err)
	}
}

// TestResolve_SingleCandidateIsNotProbed pins the cheap path. One installed
// bwrap is no choice at all, and paying two fork+execs per agent launch to
// re-confirm what the boot probe already established would be waste — so a
// lone candidate comes back even when it is plainly broken, and Probe is what
// rejects it.
func TestResolve_SingleCandidateIsNotProbed(t *testing.T) {
	dir := t.TempDir()
	broken := filepath.Join(dir, Binary)
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	restore := distroBinary
	distroBinary = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { distroBinary = restore })

	got, err := Resolve()
	if err != nil || got != broken {
		t.Fatalf("Resolve = (%q, %v), want the sole candidate %q unprobed", got, err, broken)
	}
	if err := Probe(); !errors.Is(err, ErrNotInstalled) && !errors.Is(err, ErrNamespaceRefused) {
		t.Errorf("Probe = %v, want a classified failure for a broken binary", err)
	}
}
