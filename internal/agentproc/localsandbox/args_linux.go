//go:build linux

package localsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Args returns the bubblewrap argv the agent command is prefixed with,
// ending in the "--" that closes bwrap's own options. The caller appends the
// command it wanted to run — Args never sees or names it.
//
// The plan is ordered, because bwrap applies operations in argv order and the
// order IS the policy: mask first, bind back second, grants last. Reading it
// top to bottom is reading what the agent can reach.
//
// Pure: no filesystem access, no environment. Whether the paths it names
// exist is Preflight's question, asked separately so a missing run-scoped
// grant fails with its own name rather than as a bwrap mount error.
func Args(spec Spec, host Host) ([]string, error) {
	if spec.RunRoot == "" {
		return nil, errors.New("localsandbox: Spec.RunRoot is required")
	}
	if host.Home == "" {
		return nil, errors.New("localsandbox: Host.Home is required")
	}
	if host.Cwd == "" {
		return nil, errors.New("localsandbox: Host.Cwd is required")
	}
	for _, p := range append(append([]string{spec.RunRoot, host.Home, host.Cwd}, spec.AddDirs...), spec.ReadOnly...) {
		if !filepath.IsAbs(p) {
			return nil, fmt.Errorf("localsandbox: %q is not an absolute path (the mount plan binds every path at its real location)", p)
		}
	}

	// The whole host, read-only, plus the two pseudo-filesystems that cannot
	// simply be carried over: a minimal /dev (no host disks, no /dev/kmsg)
	// and a fresh /proc. No --unshare-net (the run's git proxy, gh injector
	// and LLM endpoint are loopback) and no --unshare-pid (the caller's
	// Setpgid + kill-process-group teardown reaches the agent's children
	// only while they share this pid namespace; --die-with-parent below is
	// what covers the case --unshare-pid would have).
	bwrap := host.Bwrap
	if bwrap == "" {
		bwrap = Binary
	}
	args := []string{
		bwrap,
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}

	// Phase 1 — every mask, before any bind-back. Four trees are replaced
	// with empty tmpfs:
	//
	//
	//   the operator's home    dotfiles, ssh keys, every other project on the
	//                          machine — none of which a run needs, since its
	//                          identity, credentials and remotes all arrive
	//                          through env-scoped config and a loopback proxy
	//   the TF state root      the SQLite database, the bare clone cache, the
	//                          curator projects, every OTHER conversation's
	//                          gh-channel dir
	//   the temp dir           every concurrent run's worktree — the thing
	//                          this package exists for, since before it two
	//                          runs' trees were flat siblings under one uid
	//                          and `ls` plus `cat` read either. /tmp is masked
	//                          alongside it whenever TMPDIR points elsewhere,
	//                          so the machine's shared temp goes too
	//   /run                   which takes /run/user/<uid> with it — the
	//                          session D-Bus and Secret Service sockets — so
	//                          the OS keychain is simply unreachable
	//
	// Sorted, which puts a parent ahead of any child of it (a prefix sorts
	// first), and emitted as a group so no later mask can shadow an earlier
	// bind-back. Without that grouping the plan would depend on how the four
	// happen to nest on a given host.
	masks := dedupe([]string{host.Home, host.StateRoot, "/tmp", host.TempDir, "/run"})
	slices.Sort(masks)
	for _, m := range masks {
		args = append(args, "--tmpfs", m)
	}

	// Phase 2 — everything that comes back through them. -try on the
	// host-owned directories, which may legitimately not exist yet on a fresh
	// install (no hooks written, no gh fetched, no ~/.claude); strict on the
	// run's own, which Preflight has already established are there.
	args = appendBindTry(args, "--bind-try", host.ClaudeDir)
	args = appendBindTry(args, "--ro-bind-try", host.HooksDir, host.GHBinDir)
	args = appendBindTry(args, "--bind-try", spec.GHChannelDir)
	args = append(args, "--bind", spec.RunRoot, spec.RunRoot)
	if spec.AgentHostSocket != "" {
		args = append(args, "--bind", spec.AgentHostSocket, AgentHostSocketDest)
	}
	// Host paths the run resolved rather than configured — the node
	// interpreter, this binary, the SDK install. -try because a race that
	// removes one between resolution and spawn should surface as the command
	// failing to exec, not as an unrelated mount error.
	args = appendBindTry(args, "--ro-bind-try", spec.ReadOnly...)

	// Phase 3 — grants last, so a directory the operator explicitly handed
	// this run wins over anything above it. rw because that is what the grant
	// means.
	for _, dir := range dedupe(spec.AddDirs) {
		args = append(args, "--bind", dir, dir)
	}

	args = append(args,
		"--die-with-parent",
		"--chdir", host.Cwd,
		"--",
	)
	return args, nil
}

// appendBindTry appends one -try bind per non-empty path, source and
// destination identical, skipping duplicates. Identical source and
// destination is the invariant the whole plan rests on: nothing is relocated,
// so every path the agent is told about — its cwd, its run root, an --add-dir
// grant — means the same thing inside the namespace as outside it.
func appendBindTry(args []string, flag string, paths ...string) []string {
	for _, p := range dedupe(paths) {
		args = append(args, flag, p, p)
	}
	return args
}

// dedupe returns the cleaned, non-empty paths in order, without repeats.
func dedupe(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.TrimSpace(p) == "" {
			continue
		}
		c := filepath.Clean(p)
		if _, dup := seen[c]; dup {
			continue
		}
		seen[c] = struct{}{}
		out = append(out, c)
	}
	return out
}

// Preflight checks the parts of the plan that must exist on disk before bwrap
// reads them, and creates the one directory the plan would rather not find
// missing.
//
// Strictness is split by who owns the path. A run-scoped path — the run root,
// an --add-dir grant, the agenthost socket — is named by this run and must be
// there: a grant that silently vanishes leaves the agent reading an empty
// directory as if it were the answer, which is worse than a failed run. A
// host-owned path — the hooks dir, the pinned gh dir, ~/.claude — may
// legitimately not exist yet on a fresh install, so Args binds those with
// -try and this function only ensures ~/.claude, whose absence would send the
// run's transcript to a tmpfs that disappears with the namespace.
func Preflight(spec Spec, host Host) error {
	if host.ClaudeDir != "" {
		if err := os.MkdirAll(host.ClaudeDir, 0o700); err != nil {
			return fmt.Errorf("localsandbox: create %s: %w", host.ClaudeDir, err)
		}
	}
	if err := mustExist(spec.RunRoot, "run root"); err != nil {
		return err
	}
	if spec.AgentHostSocket != "" {
		if err := mustExist(spec.AgentHostSocket, "agenthost socket"); err != nil {
			return err
		}
	}
	for _, dir := range dedupe(spec.AddDirs) {
		if err := mustExist(dir, "--add-dir grant"); err != nil {
			return err
		}
	}
	return nil
}

func mustExist(path, what string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("localsandbox: %s %s is not on disk: %w", what, path, err)
	}
	return nil
}

// probeTimeout bounds the boot probe. Both probe steps are a fork+exec of a
// program that does nothing; a host where that takes seconds has a problem
// the operator needs told about either way.
const probeTimeout = 10 * time.Second

// distroBinary is where a distribution's bubblewrap package installs it, and
// the reason this package looks past PATH at all. On Ubuntu 23.10+
// unprivileged user namespaces are gated behind AppArmor, and the profile
// that permits them is attached to THIS path — so a snap, nix, Homebrew or
// hand-built bwrap sitting earlier on PATH is unconfined and gets denied a
// namespace, while the distro one two directories over works fine.
//
// A var rather than a const so a test can point it somewhere else and
// exercise the no-bubblewrap-anywhere path on a machine that has one — the
// same seam hostGHBinaryPath uses in internal/agentproc.
var distroBinary = "/usr/bin/bwrap"

// Resolve returns the path of a bubblewrap that can actually build a namespace
// on this host.
//
// It exists because "the first bwrap on PATH" is the wrong answer often enough
// to matter, and the right one is knowable without asking the operator: when
// more than one candidate is installed, the one that WORKS is discovered by
// trying them rather than by ranking them. That turns a boot refusal telling
// someone to go audit their PATH into a boot that just succeeds.
//
// One candidate is returned unprobed. A host with a single bwrap has no choice
// to make, and paying two fork+execs per agent launch to re-confirm what the
// boot probe already established would be waste; Probe still validates it at
// boot, which is where the fail-closed guarantee lives.
//
// Every caller routes through here — the boot probe and the spawn seam alike —
// so the binary the probe validated is the binary the run executes. Resolving
// separately in each is how a probe comes to bless one bwrap while runs use
// another.
func Resolve() (string, error) {
	candidates := binaryCandidates()
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("%w: no %s on PATH or at %s", ErrNotInstalled, Binary, distroBinary)
	case 1:
		return candidates[0], nil
	}
	var (
		firstErr error
		firstBin string
	)
	for _, bin := range candidates {
		if err := smokeTest(bin); err != nil {
			if firstErr == nil || errors.Is(err, ErrNamespaceRefused) {
				// A namespace refusal is the more informative failure to
				// report: it means a real bwrap ran and the host said no.
				firstErr = err
				firstBin = bin
			}
			continue
		}
		return bin, nil
	}
	return "", classifyNamespaceRefusal(firstBin, firstErr)
}

// binaryCandidates lists the bubblewrap binaries worth trying, PATH first and
// the distro path second, deduplicated. Order is preference only for the
// single-candidate shortcut — when both exist, Resolve picks by what works.
func binaryCandidates() []string {
	var out []string
	if p, err := exec.LookPath(Binary); err == nil {
		if abs, aerr := filepath.Abs(p); aerr == nil {
			out = append(out, abs)
		}
	}
	if len(out) == 0 || out[0] != distroBinary {
		if info, err := os.Stat(distroBinary); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			out = append(out, distroBinary)
		}
	}
	return out
}

// Probe reports whether the local sandbox can run on this host. Boot calls it
// when the sandbox is on and refuses to start if it fails — never a silent
// downgrade to an unsandboxed run.
//
// Neither failure is fixed by running TF as root, and the refusal must never
// suggest it. bubblewrap creating an UNPRIVILEGED user namespace is the
// design; a host that needs privilege to do it has a policy saying no, and the
// answers are to change that policy or to opt out.
func Probe() error {
	bin, err := Resolve()
	if err != nil {
		return err
	}
	return classifyNamespaceRefusal(bin, smokeTest(bin))
}

// smokeTest answers the two questions that decide whether one bubblewrap is
// usable, in the order whose failures have different fixes: can it run at all,
// and will this host let it create a namespace.
func smokeTest(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, bin, "--version").CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s --version failed: %v (%s)", ErrNotInstalled, bin, err, firstLine(out))
	}
	// The same argv SHAPE a real spawn uses — including the "--" terminator —
	// so a bubblewrap too old to understand one of them is caught here rather
	// than on the first delegated run.
	smoke := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--die-with-parent", "--", trueBinary()}
	if out, err := exec.CommandContext(ctx, bin, smoke...).CombinedOutput(); err != nil {
		// bwrap's own first line is the most diagnostic thing available
		// ("setting up uid map: Permission denied" vs "No permissions to
		// create new namespace"), and the toggles say which host policy did
		// it — so the operator reads the cause rather than going to find it.
		return fmt.Errorf("%w: %s: %v (%s); %s",
			ErrNamespaceRefused, bin, err, firstLine(out), strings.Join(userNSDiagnostics(), ", "))
	}
	return nil
}

// classifyNamespaceRefusal sharpens a namespace refusal with the sub-diagnosis
// the remedy turns on. bwrap's failure looks the same whether the host is a
// container that blocks nested namespaces or an Ubuntu 23.10+ whose AppArmor
// restricts user namespaces to exempted binaries, and the fixes are disjoint —
// so the knob that separates them is read here, on the failure path, and the
// AppArmor case goes out as an *AppArmorDenial carrying what the boot
// refusal's wording branches on: the binary the exemption must attach to, and
// whether any /etc/apparmor.d file even names it. Everything else passes
// through as the generic refusal.
func classifyNamespaceRefusal(bin string, err error) error {
	if err == nil || !errors.Is(err, ErrNamespaceRefused) {
		return err
	}
	if !apparmorRestrictsUserNS() {
		return err
	}
	return &AppArmorDenial{Bin: bin, ProfileFile: apparmorProfileFor(bin), Refusal: err}
}

// apparmorRestrictKnob is the sysctl that gates unprivileged user namespaces
// behind AppArmor on Ubuntu 23.10+. A var so the classification tests can pin
// every reading on a host whose real value is fixed.
var apparmorRestrictKnob = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"

// apparmorRestrictsUserNS reads the knob; absent and 0 both mean AppArmor is
// not the mechanism (absent = not a kernel that has the gate at all).
func apparmorRestrictsUserNS() bool {
	b, err := os.ReadFile(apparmorRestrictKnob)
	if err != nil {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && n != 0
}

// apparmorProfileDir is where AppArmor profiles load from, a var for the same
// testing reason as the knob.
var apparmorProfileDir = "/etc/apparmor.d"

// apparmorProfileFor returns the profile file whose content names bin, empty
// when none does. Content match, not filename — AppArmor attaches a profile by
// the path in its header, and the file carrying it can be called anything
// (bwrap, bwrap-userns-restrict, ...). Only regular files directly under the
// dir are scanned: the subdirectories (abstractions, tunables, local, disable)
// hold includes and disables, not loadable profiles. This is a scan of ~120
// small files on a stock system, and it runs only on a path that ends in a
// boot refusal, where its cost is irrelevant and its answer decides the one
// line the operator acts on.
func apparmorProfileFor(bin string) string {
	entries, err := os.ReadDir(apparmorProfileDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(apparmorProfileDir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if strings.Contains(string(b), bin) {
			return p
		}
	}
	return ""
}

// userNSKnobs are the host toggles that actually gate an unprivileged user
// namespace, in the order an operator should read them. Each is absent on
// kernels that don't implement it, which is itself informative — an absent
// apparmor_restrict_unprivileged_userns means this is not an Ubuntu 23.10+
// host and that is not the cause.
var userNSKnobs = []string{
	apparmorRestrictKnob,
	"/proc/sys/user/max_user_namespaces",
	"/proc/sys/kernel/unprivileged_userns_clone",
}

// userNSDiagnostics reads the toggles above so the refusal carries the values
// that decide the answer rather than making the operator go find them. Only
// consulted on the failure path, so three file reads cost nothing in the
// normal case.
func userNSDiagnostics() []string {
	out := make([]string, 0, len(userNSKnobs))
	for _, knob := range userNSKnobs {
		name := filepath.Base(knob)
		b, err := os.ReadFile(knob)
		if err != nil {
			out = append(out, name+"=<absent>")
			continue
		}
		out = append(out, name+"="+strings.TrimSpace(string(b)))
	}
	return out
}

// trueBinary resolves the do-nothing program the smoke run executes inside
// the namespace. /bin/true is the fallback for a PATH that cannot find it;
// a host with neither fails the smoke run, which is the correct answer for
// an environment that cannot run a trivial command.
func trueBinary() string {
	if p, err := exec.LookPath("true"); err == nil {
		return p
	}
	return "/bin/true"
}

func firstLine(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "no output"
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
