//go:build linux

package localsandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
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

// TestClassifyNamespaceRefusal pins the sub-diagnosis the boot refusal's
// wording turns on: the AppArmor case fires exactly when the knob says
// AppArmor is the mechanism, and the profile scan answers "is an exemption
// even on disk" the way AppArmor itself would — by attachment path in a
// file's content, not by filename, and only for loadable files directly
// under the profile dir.
func TestClassifyNamespaceRefusal(t *testing.T) {
	const bin = "/usr/local/bin/bwrap"
	refusal := fmt.Errorf("%w: %s: denied", ErrNamespaceRefused, bin)

	// setHost pins the two host inputs the classification reads: the knob
	// ("" = absent) and the contents of the profile dir, with subdirFiles
	// landing one directory down where a profile must not be found.
	setHost := func(t *testing.T, knob string, files, subdirFiles map[string]string) {
		t.Helper()
		dir := t.TempDir()
		restoreKnob, restoreDir := apparmorRestrictKnob, apparmorProfileDir
		t.Cleanup(func() { apparmorRestrictKnob, apparmorProfileDir = restoreKnob, restoreDir })
		apparmorRestrictKnob = filepath.Join(dir, "knob")
		if knob != "" {
			if err := os.WriteFile(apparmorRestrictKnob, []byte(knob), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		apparmorProfileDir = filepath.Join(dir, "apparmor.d")
		if err := os.MkdirAll(apparmorProfileDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(apparmorProfileDir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for name, content := range subdirFiles {
			sub := filepath.Join(apparmorProfileDir, name)
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sub, "inner"), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	profileText := "abi <abi/4.0>,\nprofile bwrap " + bin + " flags=(unconfined) {\n  userns,\n}\n"
	otherProfile := "profile firefox /usr/bin/firefox {\n}\n"

	t.Run("knob set with a profile on disk", func(t *testing.T) {
		setHost(t, "1\n", map[string]string{
			"aa-something-else": otherProfile,
			"zz-bwrap-userns":   profileText,
		}, nil)
		got := classifyNamespaceRefusal(bin, refusal)
		var aa *AppArmorDenial
		if !errors.As(got, &aa) {
			t.Fatalf("classify = %v, want an AppArmorDenial", got)
		}
		if aa.Bin != bin {
			t.Errorf("Bin = %q, want %q", aa.Bin, bin)
		}
		if want := filepath.Join(apparmorProfileDir, "zz-bwrap-userns"); aa.ProfileFile != want {
			t.Errorf("ProfileFile = %q, want %q — the file whose content names the binary", aa.ProfileFile, want)
		}
		if !errors.Is(got, ErrNamespaceRefused) {
			t.Error("sharpening the refusal lost errors.Is(err, ErrNamespaceRefused)")
		}
	})

	t.Run("knob set with no profile anywhere", func(t *testing.T) {
		setHost(t, "1\n", map[string]string{"aa-something-else": otherProfile}, nil)
		var aa *AppArmorDenial
		if got := classifyNamespaceRefusal(bin, refusal); !errors.As(got, &aa) {
			t.Fatalf("classify = %v, want an AppArmorDenial", got)
		}
		if aa.ProfileFile != "" {
			t.Errorf("ProfileFile = %q, want empty when nothing names the binary", aa.ProfileFile)
		}
	})

	t.Run("a file inside a subdirectory is not a loadable profile", func(t *testing.T) {
		setHost(t, "1\n", nil, map[string]string{"disable": profileText})
		var aa *AppArmorDenial
		if got := classifyNamespaceRefusal(bin, refusal); !errors.As(got, &aa) {
			t.Fatalf("classify = %v, want an AppArmorDenial", got)
		} else if aa.ProfileFile != "" {
			t.Errorf("ProfileFile = %q, want empty — subdirectories hold includes and disables, not profiles", aa.ProfileFile)
		}
	})

	t.Run("knob zero passes the refusal through", func(t *testing.T) {
		setHost(t, "0\n", map[string]string{"zz-bwrap-userns": profileText}, nil)
		if got := classifyNamespaceRefusal(bin, refusal); got != refusal {
			t.Errorf("classify = %v, want the refusal untouched when AppArmor is not the mechanism", got)
		}
	})

	t.Run("absent knob passes the refusal through", func(t *testing.T) {
		setHost(t, "", nil, nil)
		if got := classifyNamespaceRefusal(bin, refusal); got != refusal {
			t.Errorf("classify = %v, want the refusal untouched on a kernel without the gate", got)
		}
	})

	t.Run("only namespace refusals are classified", func(t *testing.T) {
		setHost(t, "1\n", nil, nil)
		other := errors.New("some other failure")
		if got := classifyNamespaceRefusal(bin, other); got != other {
			t.Errorf("classify = %v, want a non-refusal untouched even with the knob set", got)
		}
		if got := classifyNamespaceRefusal(bin, nil); got != nil {
			t.Errorf("classify(nil) = %v, want nil", got)
		}
	})
}

// TestProbe_AppArmorDenialCarriesTheResolvedBinary pins the thread from probe
// to remedy: the profile the refusal tells the operator to write attaches to
// a path, so the path must be the binary the probe actually ran, not a guess.
func TestProbe_AppArmorDenialCarriesTheResolvedBinary(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, Binary)
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo bubblewrap 0.0.0; exit 0; fi\n" +
		"echo 'bwrap: setting up uid map: Permission denied' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	restore := distroBinary
	distroBinary = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { distroBinary = restore })

	seam := t.TempDir()
	restoreKnob, restoreDir := apparmorRestrictKnob, apparmorProfileDir
	t.Cleanup(func() { apparmorRestrictKnob, apparmorProfileDir = restoreKnob, restoreDir })
	apparmorRestrictKnob = filepath.Join(seam, "knob")
	if err := os.WriteFile(apparmorRestrictKnob, []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	apparmorProfileDir = filepath.Join(seam, "apparmor.d")
	if err := os.MkdirAll(apparmorProfileDir, 0o755); err != nil {
		t.Fatal(err)
	}

	err := Probe()
	var aa *AppArmorDenial
	if !errors.As(err, &aa) {
		t.Fatalf("Probe = %v, want an AppArmorDenial", err)
	}
	if aa.Bin != fake {
		t.Errorf("Bin = %q, want the probed binary %q", aa.Bin, fake)
	}
	if aa.ProfileFile != "" {
		t.Errorf("ProfileFile = %q, want empty for an empty profile dir", aa.ProfileFile)
	}
	if !errors.Is(err, ErrNamespaceRefused) {
		t.Error("the sharpened refusal lost errors.Is(err, ErrNamespaceRefused)")
	}
}

// TestArgs_BindsResolverPathsBackThroughTheRunMask is the regression for a
// silent, total failure: the /run mask left /etc/resolv.conf dangling on
// systemd-resolved hosts, so every hostname lookup inside the namespace
// failed. Nothing else noticed — the git and gh proxies are loopback by IP and
// the agenthost socket is bound at its own path — so runs booted green and
// died at their first completion with what looked like a hang.
//
// The pure layer takes the resolved paths as data, so both host shapes are
// exercised by injecting what DNSPaths would have returned on them.
func TestArgs_BindsResolverPathsBackThroughTheRunMask(t *testing.T) {
	cases := []struct {
		name     string
		resolver string
		// masked says whether the resolver path sits under a tmpfs, which is
		// what makes the bind load-bearing rather than a no-op.
		masked bool
	}{
		{
			name:     "systemd-resolved stub under the masked /run",
			resolver: "/run/systemd/resolve/stub-resolv.conf",
			masked:   true,
		},
		{
			name:     "plain file on a host that keeps one",
			resolver: "/etc/resolv.conf",
			masked:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spec := Spec{
				RunRoot:  "/tmp/triagefactory-runs/rk1",
				ReadOnly: []string{c.resolver, "/run/systemd/resolve"},
			}
			got, err := Args(spec, testHost())
			if err != nil {
				t.Fatalf("Args: %v", err)
			}
			bind := indexOfOp(got, "--ro-bind-try", c.resolver)
			if bind < 0 {
				t.Fatalf("plan never binds the resolver path %q back:\n%v", c.resolver, got)
			}
			// Phase order is the policy: a bind-back emitted before the mask
			// that shadows it would restore nothing, which is precisely the
			// bug being regressed.
			if mask := indexOfOp(got, "--tmpfs", "/run"); mask < 0 {
				t.Fatal("plan no longer masks /run at all")
			} else if c.masked && bind < mask {
				t.Errorf("resolver bind at %d precedes the /run mask at %d, so the mask shadows it", bind, mask)
			}
			// The directory too, for nss-resolve's varlink socket: binding the
			// stub file alone fixes glibc's fallback and leaves the primary
			// resolution path broken.
			if indexOfOp(got, "--ro-bind-try", systemdResolveDir) < 0 {
				t.Errorf("plan never binds %s, so nss-resolve's socket stays masked:\n%v", systemdResolveDir, got)
			}
		})
	}
}

// TestDNSPaths pins what the resolver resolution hands the plan, against a
// fixture tree rather than whatever the machine running the suite happens to
// be configured with.
func TestDNSPaths(t *testing.T) {
	// setResolv points the package at a fixture and returns its root.
	setResolv := func(t *testing.T, link, target string) string {
		t.Helper()
		dir := t.TempDir()
		restoreConf, restoreDir := resolvConfPath, systemdResolveDir
		t.Cleanup(func() { resolvConfPath, systemdResolveDir = restoreConf, restoreDir })
		resolvConfPath = filepath.Join(dir, link)
		systemdResolveDir = filepath.Join(dir, "run", "systemd", "resolve")
		if target != "" {
			full := filepath.Join(dir, target)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte("nameserver 127.0.0.53\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	t.Run("follows the symlink to the real file", func(t *testing.T) {
		dir := setResolv(t, "resolv.conf", "run/systemd/resolve/stub-resolv.conf")
		if err := os.Symlink(filepath.Join(dir, "run/systemd/resolve/stub-resolv.conf"), resolvConfPath); err != nil {
			t.Fatal(err)
		}
		got := DNSPaths()
		want := filepath.Join(dir, "run/systemd/resolve/stub-resolv.conf")
		if !slices.Contains(got, want) {
			t.Errorf("DNSPaths = %v, want the symlink TARGET %q — binding the link itself restores a dangling path", got, want)
		}
		if !slices.Contains(got, systemdResolveDir) {
			t.Errorf("DNSPaths = %v, want the resolver directory %q for nss-resolve's socket", got, systemdResolveDir)
		}
	})

	t.Run("a plain file is returned as itself", func(t *testing.T) {
		dir := setResolv(t, "resolv.conf", "resolv.conf")
		got := DNSPaths()
		if want := filepath.Join(dir, "resolv.conf"); !slices.Contains(got, want) {
			t.Errorf("DNSPaths = %v, want the plain file %q", got, want)
		}
	})

	t.Run("no resolv.conf skips it rather than failing", func(t *testing.T) {
		setResolv(t, "resolv.conf", "")
		got := DNSPaths()
		if slices.Contains(got, resolvConfPath) {
			t.Errorf("DNSPaths = %v, want no entry for an unresolvable resolv.conf", got)
		}
		// The directory is still offered; -try is what handles its absence,
		// and deciding here would give the probe and the plan two answers.
		if !slices.Contains(got, systemdResolveDir) {
			t.Errorf("DNSPaths = %v, want the resolver directory offered unconditionally", got)
		}
	})
}

// TestProbeCommand pins that the probe asserts the resolver is reachable
// where that assertion is meaningful, and stays a do-nothing exec where it is
// not. A host with no resolv.conf must not be refused a boot for a breakage
// the sandbox did not cause.
func TestProbeCommand(t *testing.T) {
	dir := t.TempDir()
	restore := resolvConfPath
	t.Cleanup(func() { resolvConfPath = restore })

	resolvConfPath = filepath.Join(dir, "absent")
	if got := probeCommand(); len(got) != 1 || !strings.HasSuffix(got[0], "true") {
		t.Errorf("probeCommand with no resolv.conf = %v, want the do-nothing program", got)
	}

	resolvConfPath = filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(resolvConfPath, []byte("nameserver 1.1.1.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := probeCommand()
	if len(got) != 3 || got[1] != "-c" || !strings.Contains(got[2], resolvConfPath) {
		t.Fatalf("probeCommand with a resolv.conf = %v, want a readability test on %q", got, resolvConfPath)
	}
}

// TestSmokeTest_ValidatesThePlanNotJustTheNamespace pins the probe gap that
// let this ship: the smoke argv masked only /tmp and exec'd a program that
// does nothing, so it proved a namespace could be created and proved nothing
// about whether the plan it gets created with is habitable.
func TestSmokeTest_ValidatesThePlanNotJustTheNamespace(t *testing.T) {
	dir := t.TempDir()
	recorder := filepath.Join(dir, Binary)
	argvLog := filepath.Join(dir, "argv")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = --version ]; then echo bubblewrap 0.0.0; exit 0; fi\n" +
		"printf '%s\\n' \"$@\" >" + argvLog + "\n" +
		"exit 0\n"
	if err := os.WriteFile(recorder, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := smokeTest(recorder); err != nil {
		t.Fatalf("smokeTest against a recorder that always succeeds: %v", err)
	}
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("the smoke run never executed: %v", err)
	}
	argv := strings.Split(strings.TrimSpace(string(b)), "\n")

	if indexOfOp(argv, "--tmpfs", "/run") < 0 {
		t.Errorf("smoke argv never masks /run, so it cannot catch a plan that breaks under that mask:\n%v", argv)
	}
	for _, p := range DNSPaths() {
		if indexOfOp(argv, "--ro-bind-try", p) < 0 {
			t.Errorf("smoke argv is missing the resolver bind-back %q, so it validates a plan a run never gets:\n%v", p, argv)
		}
	}
}
