//go:build linux

package localsandbox

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
		// /run holds exactly the one file we put there. Everything else the
		// host keeps in /run is gone with the tmpfs — including /run/user,
		// which is where the session D-Bus and Secret Service sockets live,
		// so the OS keychain has no path in from here.
		listing := strings.Fields(strings.TrimPrefix(got, "socket-ok"))
		if len(listing) != 1 || listing[0] != "tf.sock" {
			t.Errorf("/run lists %v inside the namespace, want only tf.sock", listing)
		}
	})

	t.Run("the run tree is writable but the host root is not", func(t *testing.T) {
		if out, err := run("echo x > /etc/tf-sandbox-probe"); err == nil {
			t.Errorf("a write outside every bind succeeded: %q", out)
		}
	})
}
