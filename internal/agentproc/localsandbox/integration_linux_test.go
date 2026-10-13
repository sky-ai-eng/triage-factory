//go:build linux

package localsandbox

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSandbox_IsolationProperties runs the real bubblewrap with a real plan
// and checks what the agent can actually see. The golden argv test above pins
// the plan's SHAPE; this one pins what the shape buys, which is the part that
// would still be wrong if a flag meant something other than what we think.
//
// Skipped where bubblewrap is absent or cannot create a namespace — the same
// two conditions the boot probe refuses on, which on a developer's machine is
// a reason to skip rather than to fail.
func TestSandbox_IsolationProperties(t *testing.T) {
	if err := Probe(); err != nil {
		t.Skipf("bubblewrap unusable here: %v", err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell: %v", err)
	}

	// A miniature of the real layout: a home with a secret in it, a state
	// root with the database in it, a temp dir holding this run's tree
	// alongside a sibling run's, and a granted directory outside all of them.
	base := t.TempDir()
	home := filepath.Join(base, "home")
	stateRoot := filepath.Join(home, ".triagefactory")
	tempDir := filepath.Join(base, "tmp")
	runRoot := filepath.Join(tempDir, "triagefactory-runs", "mine")
	sibling := filepath.Join(tempDir, "triagefactory-runs", "theirs")
	grant := filepath.Join(base, "granted")
	for _, d := range []string{home, stateRoot, tempDir, runRoot, sibling, grant} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".netrc"), "operator secret")
	write(filepath.Join(stateRoot, "triagefactory.db"), "the database")
	write(filepath.Join(sibling, "notes.md"), "another run's work")
	write(filepath.Join(runRoot, "mine.txt"), "my work")
	write(filepath.Join(grant, "granted.txt"), "granted work")

	host := Host{Home: home, ClaudeDir: filepath.Join(home, ".claude"), StateRoot: stateRoot, TempDir: tempDir, Cwd: runRoot}
	spec := Spec{RunRoot: runRoot, AddDirs: []string{grant}}
	if err := Preflight(spec, host); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	args, err := Args(spec, host)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}

	run := func(script string) (string, error) {
		out, err := exec.Command(args[0], append(append([]string(nil), args[1:]...), sh, "-c", script)...).CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// The resolver, read through the real plan rather than a hand-built argv.
	// On a systemd-resolved host /etc/resolv.conf is a symlink into the /run
	// this plan masks, so without the bind-backs this read fails and every
	// hostname lookup in a real run fails with it — silently, because the git
	// and gh proxies are loopback by IP and only the LLM hop resolves a name.
	// Where the host keeps a plain /etc/resolv.conf the read succeeds either
	// way; the assertion costs nothing there and is the only one that fires on
	// the hosts that break.
	t.Run("the resolver survives the /run mask", func(t *testing.T) {
		if _, err := os.ReadFile("/etc/resolv.conf"); err != nil {
			t.Skipf("no readable /etc/resolv.conf on this host: %v", err)
		}
		dnsSpec := spec
		dnsSpec.ReadOnly = DNSPaths()
		dnsArgs, err := Args(dnsSpec, host)
		if err != nil {
			t.Fatalf("Args: %v", err)
		}
		out, err := exec.Command(dnsArgs[0], append(append([]string(nil), dnsArgs[1:]...),
			sh, "-c", "cat /etc/resolv.conf")...).CombinedOutput()
		if err != nil {
			t.Fatalf("resolv.conf unreadable inside the namespace: %v (%s)", err, strings.TrimSpace(string(out)))
		}
		if !strings.Contains(string(out), "nameserver") {
			t.Errorf("resolv.conf read back without a nameserver line: %q", strings.TrimSpace(string(out)))
		}
	})

	t.Run("its own tree is readable and writable", func(t *testing.T) {
		out, err := run("cat mine.txt && echo appended > new.txt && cat new.txt")
		if err != nil {
			t.Fatalf("run: %v (%s)", err, out)
		}
		if !strings.Contains(out, "my work") || !strings.Contains(out, "appended") {
			t.Errorf("run tree not usable: %q", out)
		}
		if _, err := os.Stat(filepath.Join(runRoot, "new.txt")); err != nil {
			t.Errorf("a write inside the namespace did not land on the host tree: %v", err)
		}
	})

	t.Run("a sibling run is invisible", func(t *testing.T) {
		out, _ := run("cat " + filepath.Join(sibling, "notes.md"))
		if strings.Contains(out, "another run's work") {
			t.Errorf("sibling run's work is readable from inside: %q", out)
		}
		listing, err := run("ls " + filepath.Dir(runRoot))
		if err != nil {
			t.Fatalf("ls: %v (%s)", err, listing)
		}
		if fields := strings.Fields(listing); len(fields) != 1 || fields[0] != "mine" {
			t.Errorf("run-tree parent lists %v, want only this run's own root", fields)
		}
	})

	t.Run("TF state and the operator's home are masked", func(t *testing.T) {
		out, _ := run("cat " + filepath.Join(stateRoot, "triagefactory.db"))
		if strings.Contains(out, "the database") {
			t.Errorf("the SQLite database is readable from inside: %q", out)
		}
		out, _ = run("cat " + filepath.Join(home, ".netrc"))
		if strings.Contains(out, "operator secret") {
			t.Errorf("the operator's dotfiles are readable from inside: %q", out)
		}
	})

	t.Run("an add-dir grant is readable and writable", func(t *testing.T) {
		out, err := run("cat " + filepath.Join(grant, "granted.txt") + " && echo more > " + filepath.Join(grant, "added.txt"))
		if err != nil {
			t.Fatalf("run: %v (%s)", err, out)
		}
		if !strings.Contains(out, "granted work") {
			t.Errorf("granted dir not readable: %q", out)
		}
		if _, err := os.Stat(filepath.Join(grant, "added.txt")); err != nil {
			t.Errorf("a write to the granted dir did not land on the host: %v", err)
		}
	})

	t.Run("the exec-verb socket is reachable and the rest of /run is gone", func(t *testing.T) {
		sockPath := filepath.Join(base, "agenthost.sock")
		ln, err := net.Listen("unix", sockPath)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = ln.Close() }()

		withSock := spec
		withSock.AgentHostSocket = sockPath
		// The resolver bind-backs too, so this asserts the /run a real run
		// gets rather than a shape production never builds.
		withSock.ReadOnly = DNSPaths()
		sockArgs, err := Args(withSock, host)
		if err != nil {
			t.Fatalf("Args: %v", err)
		}
		out, err := exec.Command(sockArgs[0], append(append([]string(nil), sockArgs[1:]...), sh, "-c",
			"test -S "+AgentHostSocketDest+" && echo socket-ok; ls /run")...).CombinedOutput()
		got := strings.TrimSpace(string(out))
		if err != nil {
			t.Fatalf("run: %v (%s)", err, got)
		}
		if !strings.Contains(got, "socket-ok") {
			t.Errorf("%s is not a socket inside the namespace: %q", AgentHostSocketDest, got)
		}
		// /run holds the socket and, on a systemd host, the resolver state
		// bound back so DNS works. Everything else the host keeps there is
		// gone with the tmpfs — above all /run/user, where the session D-Bus
		// and Secret Service sockets live, so those socket paths are absent
		// from here. That absence is the property the mask exists for; the
		// resolver is root-owned world-readable state that grants nothing.
		listing := strings.Fields(strings.TrimPrefix(got, "socket-ok"))
		allowed := map[string]bool{"tf.sock": true}
		for _, p := range DNSPaths() {
			if rel, err := filepath.Rel("/run", p); err == nil && !strings.HasPrefix(rel, "..") {
				allowed[strings.Split(rel, string(filepath.Separator))[0]] = true
			}
		}
		for _, entry := range listing {
			if !allowed[entry] {
				t.Errorf("/run lists unexpected %q inside the namespace (all: %v)", entry, listing)
			}
		}
		if !slices.Contains(listing, "tf.sock") {
			t.Errorf("/run lists %v inside the namespace, want tf.sock among them", listing)
		}
		if slices.Contains(listing, "user") {
			t.Error("/run/user survived the mask; the session bus and Secret Service socket paths are visible inside the namespace")
		}
	})

	// Host processes have to be absent from the pid namespace the agent runs
	// in, not merely from its mount view. A path-based mask hides a name in
	// THIS namespace's filesystem, but /proc/<pid>/root of a process outside
	// it resolves in that process's own mount namespace, so every same-uid
	// host process would be a way around the masks above.
	//
	// The host process is identified by its command line, never its pid:
	// pid numbers are local to a namespace, so the same number can name an
	// unrelated process inside. The same scan finds a sleep started from
	// inside, which proves it can see processes at all and keeps a broken
	// scan from passing as an invisible host.
	t.Run("host processes are not visible", func(t *testing.T) {
		hostToken := fmt.Sprintf("%d.%d", 710000+os.Getpid(), time.Now().UnixNano()%1000)
		innerToken := hostToken + "9"
		hostSleep := exec.Command("sleep", hostToken)
		if err := hostSleep.Start(); err != nil {
			t.Fatalf("start host sleep: %v", err)
		}
		t.Cleanup(func() { _ = hostSleep.Process.Kill(); _ = hostSleep.Wait() })
		if !waitFor(func() bool { return countMatching(hostToken) > 0 }) {
			t.Fatal("the host sleep never started")
		}

		out, err := run(`sleep ` + innerToken + ` &
inner=$!
for p in /proc/[0-9]*; do
	case "$(tr '\0' ' ' < "$p/cmdline" 2>/dev/null)" in
	"sleep ` + hostToken + ` ") echo host ;;
	"sleep ` + innerToken + ` ") echo inner ;;
	esac
done
kill $inner`)
		if err != nil {
			t.Fatalf("scan /proc: %v (%s)", err, out)
		}
		found := strings.Fields(out)
		if !slices.Contains(found, "inner") {
			t.Fatalf("the scan did not find a process started inside the namespace, so it proves nothing: %q", out)
		}
		if slices.Contains(found, "host") {
			t.Errorf("a host process is visible in /proc inside the namespace; the agent shares the host's pid namespace")
		}

		// /proc/1 is the namespace's own init, not the host's: it has to
		// exist and has to be a bwrap process.
		comm, err := run("cat /proc/1/comm")
		if err != nil {
			t.Fatalf("read /proc/1/comm: %v (%s)", err, comm)
		}
		if comm != "bwrap" {
			t.Errorf("/proc/1/comm inside the namespace = %q, want %q (the host's init is visible)", comm, "bwrap")
		}
	})

	t.Run("the run tree is writable but the host root is not", func(t *testing.T) {
		if out, err := run("echo x > /etc/tf-sandbox-probe"); err == nil {
			t.Errorf("a write outside every bind succeeded: %q", out)
		}
	})
}

// TestSandbox_ProcessGroupKillReachesDetachedDescendants pins the teardown
// contract agentproc relies on: SIGKILL to the bwrap process group ends the
// agent and everything it started, including a descendant that left the
// group with setsid. Outside a pid namespace such a descendant is orphaned
// and keeps running after the run is cancelled; inside one, the namespace's
// init dies with the group and the kernel kills what remains.
func TestSandbox_ProcessGroupKillReachesDetachedDescendants(t *testing.T) {
	if err := Probe(); err != nil {
		t.Skipf("bubblewrap unusable here: %v", err)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skipf("no shell: %v", err)
	}
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skipf("no setsid: %v", err)
	}

	base := t.TempDir()
	home := filepath.Join(base, "home")
	runRoot := filepath.Join(base, "tmp", "triagefactory-runs", "mine")
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	host := Host{Home: home, ClaudeDir: filepath.Join(home, ".claude"), StateRoot: filepath.Join(home, ".triagefactory"), TempDir: filepath.Join(base, "tmp"), Cwd: runRoot}
	spec := Spec{RunRoot: runRoot}
	if err := Preflight(spec, host); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	args, err := Args(spec, host)
	if err != nil {
		t.Fatalf("Args: %v", err)
	}

	// A sleep whose argv is unique to this test, so the host-side scan below
	// can find it without knowing which pid it was given in the namespace.
	token := fmt.Sprintf("%d.%d", 700000+os.Getpid(), time.Now().UnixNano()%1000)
	script := setsid + " sleep " + token + " & wait"
	cmd := exec.Command(args[0], append(append([]string(nil), args[1:]...), sh, "-c", script)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		killMatching(token)
	})

	if !waitFor(func() bool { return countMatching(token) > 0 }) {
		t.Fatal("the detached sleep never started")
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill process group: %v", err)
	}
	<-waited
	if !waitFor(func() bool { return countMatching(token) == 0 }) {
		t.Errorf("a descendant that called setsid survived the process-group kill")
	}
}

// countMatching returns how many host processes are the test's sleep: argv
// exactly ["sleep", token]. Exact rather than a substring match because bwrap
// and the shell that wraps it carry the token in their own argv too, and
// counting them would report the sleep as running before it exists.
func countMatching(token string) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		if string(b) == "sleep\x00"+token+"\x00" {
			n++
		}
	}
	return n
}

// killMatching kills every host sleep started with token; the cleanup path
// for a failure that left one behind.
func killMatching(token string) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err == nil && string(b) == "sleep\x00"+token+"\x00" {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// waitFor polls cond until it holds or the deadline passes. The deadline is
// generous because it only bounds the failing case: a passing test returns on
// the first poll that sees the condition, and a loaded CI runner can take
// seconds to schedule a fork.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}
