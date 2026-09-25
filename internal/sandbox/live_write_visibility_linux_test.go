//go:build linux

package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
)

// TestLiveJail_WritesReachHostWhileRunning is the premise a mid-engagement
// workspace checkpoint rests on: the executor captures a run tree while the
// jail that writes into it is still running, so the host must see the jail's
// writes as they happen, not when the jail exits.
//
// It launches the real tool host under the spec, bundle and runsc argv every
// jail is launched with, has it create a file, append to an existing one, and
// start a background writer holding a file open, and reads all three from the
// host with the tool host still serving. A cached write that only reached the
// host on close or exit fails it.
//
// Needs root, runsc, iproute2, a static busybox for the rootfs shell, and a
// tool host built for a static target (TF_TEST_TOOLHOST_BIN, or the trusted
// path an executor image installs it at). Each missing piece skips.
func TestLiveJail_WritesReachHostWhileRunning(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("launching a jail needs root")
	}
	for _, bin := range []string{"runsc", "ip"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
	busybox := "/bin/busybox"
	if _, err := os.Stat(busybox); err != nil {
		t.Skip("a static busybox is needed for the probe rootfs's shell")
	}
	toolHost := os.Getenv("TF_TEST_TOOLHOST_BIN")
	if toolHost == "" {
		toolHost = trustedToolHostBinaryPath
	}
	if _, err := os.Stat(toolHost); err != nil {
		t.Skipf("no tool host binary at %s (set TF_TEST_TOOLHOST_BIN to a static build)", toolHost)
	}

	suffix := randomProbeSuffix(t)
	rootfs := probeRootfs(t, busybox, toolHost)
	netnsPath := probeNetns(t, "tfprobe-"+suffix)

	worktree := t.TempDir()
	if err := os.Chown(worktree, WorktreeUID, WorktreeGID); err != nil {
		t.Fatalf("chown worktree: %v", err)
	}
	existing := filepath.Join(worktree, "existing.txt")
	if err := os.WriteFile(existing, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(existing, WorktreeUID, WorktreeGID); err != nil {
		t.Fatal(err)
	}

	// The socket directory the jail dials out to, granted to the sandbox
	// identity the way the tool host launch grants it.
	sockDir := t.TempDir()
	if err := os.Chown(sockDir, WorktreeUID, WorktreeGID); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(sockDir, "tools.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("bind tool host socket: %v", err)
	}
	defer func() { _ = listener.Close() }()
	if err := os.Chown(sockPath, WorktreeUID, WorktreeGID); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		ConversationID: "probe-" + suffix,
		Worktree:       worktree,
		Argv:           []string{"/opt/tf/bin/tf-harness-tools", "serve", "--connect", "/run/tf-tools/tools.sock", "--cwd", "/work"},
		Env:            []string{"PATH=/bin", "HOME=/work"},
		ExtraMounts:    []Mount{{Source: sockDir, Destination: "/run/tf-tools", Options: []string{"rw"}}},
	}
	spec, err := buildSpec(cfg, netnsPath)
	if err != nil {
		t.Fatalf("build spec: %v", err)
	}
	bundleDir, err := writeBundle(cfg, spec, rootfs)
	if err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	defer func() { _ = cleanupBundle(bundleDir) }()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID := "tfprobe-" + suffix
	cmd := newRunscCommand(ctx, bundleDir, containerID)
	// runsc's own logs are the only account of a jail that dies at boot.
	if dir := os.Getenv("TF_TEST_RUNSC_DEBUG_DIR"); dir != "" {
		cmd.Args = append([]string{cmd.Args[0], "--debug", "--debug-log=" + dir + "/"}, cmd.Args[1:]...)
	}
	var stderr bytes.Buffer
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start runsc: %v", err)
	}
	// exited closes once the runtime is reaped, and waitErr is readable after.
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	defer func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
		_ = exec.Command("runsc", "delete", "--force", containerID).Run()
	}()

	conns := make(chan net.Conn, 1)
	go func() {
		if c, err := listener.Accept(); err == nil {
			conns <- c
		}
	}()
	var conn net.Conn
	select {
	case conn = <-conns:
	case <-exited:
		t.Fatalf("the jail exited (%v) before its tool host dialed in; stderr: %s", waitErr, stderr.String())
	case <-time.After(60 * time.Second):
		t.Fatalf("the tool host did not dial in; stderr: %s", stderr.String())
	}
	tools := agentloop.NewToolHost(conn, time.Minute)
	defer func() { _ = tools.Close() }()

	call := func(name string, args map[string]any) {
		t.Helper()
		out, err := tools.Call(name, args)
		if err != nil {
			t.Fatalf("%s: transport: %v", name, err)
		}
		if out.Protocol != nil || out.ToolError != "" {
			t.Fatalf("%s: protocol=%v tool_error=%q", name, out.Protocol, out.ToolError)
		}
	}
	jailAlive := func() {
		t.Helper()
		select {
		case <-exited:
			t.Fatalf("the jail exited (%v) before the host read; the probe proves nothing about a live jail", waitErr)
		default:
		}
	}

	// A new file written by the tool host itself, and an append made by a
	// command it runs.
	call("write", map[string]any{"path": "new.txt", "content": "written inside\n"})
	call("bash", map[string]any{"command": "printf 'appended inside\\n' >> existing.txt"})
	jailAlive()

	if got := readProbeFile(t, filepath.Join(worktree, "new.txt")); got != "written inside\n" {
		t.Errorf("host read of a file the live jail created = %q, want %q", got, "written inside\n")
	}
	if got := readProbeFile(t, existing); got != "before\nappended inside\n" {
		t.Errorf("host read of a file the live jail appended to = %q, want %q", got, "before\nappended inside\n")
	}

	// A writer the agent left running holds its file open across calls; its
	// writes must reach the host while it is still writing.
	call("bash", map[string]any{"command": "(i=0; while [ $i -lt 600 ]; do echo line$i; i=$((i+1)); sleep 0.05; done) > bg.log 2>&1 &"})
	bg := filepath.Join(worktree, "bg.log")
	var sizes []int64
	deadline := time.Now().Add(15 * time.Second)
	for len(sizes) < 3 && time.Now().Before(deadline) {
		if fi, err := os.Stat(bg); err == nil && fi.Size() > 0 && (len(sizes) == 0 || fi.Size() > sizes[len(sizes)-1]) {
			sizes = append(sizes, fi.Size())
		}
		time.Sleep(100 * time.Millisecond)
	}
	jailAlive()
	if len(sizes) < 3 {
		t.Errorf("the host saw a background writer's open file grow %d times in 15s (sizes %v); want at least 3 while it runs", len(sizes), sizes)
	}
	t.Logf("background writer's file as the host saw it grow: %v bytes", sizes)
}

// TestNewRunscCommand_LeavesBindMountsUncached holds the flags the live probe
// above depends on, for the ordinary `go test` run that has no runsc. gVisor
// writes a bind mount through to the host when its file access is shared,
// which is the default, and would cache writes in the sandbox if the mount
// were exclusive or sat under an overlay. So the runsc argv must set neither.
func TestNewRunscCommand_LeavesBindMountsUncached(t *testing.T) {
	cmd := newRunscCommand(context.Background(), "/tmp/bundle", "cid-test")
	for _, arg := range cmd.Args {
		norm := strings.TrimLeft(arg, "-")
		if strings.HasPrefix(norm, "file-access-mounts") {
			t.Errorf("runsc argv sets %q; the run tree must stay on gVisor's shared default so a live checkpoint reads what the jail wrote", arg)
		}
		if v, ok := strings.CutPrefix(norm, "overlay2="); ok && !strings.HasPrefix(v, "root:") && v != "none" {
			t.Errorf("runsc argv sets %q; an overlay over the bind mounts would keep the jail's writes off the host", arg)
		}
		if norm == "overlay2" {
			t.Errorf("runsc argv sets --overlay2 in split form; hold it to the root-only overlay (args %v)", cmd.Args)
		}
	}
}

// probeRootfs builds a minimal read-only-able rootfs: a static busybox as the
// shell, the tool host at the path the launch pins, and the mount points the
// spec binds onto.
func probeRootfs(t *testing.T, busybox, toolHost string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"bin", "opt/tf/bin", "proc", "dev", "sys", "tmp", "work", "run/tf-tools", "etc/ssl/certs"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "etc/resolv.conf"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	copyProbeBinary(t, busybox, filepath.Join(root, "bin/busybox"))
	copyProbeBinary(t, toolHost, filepath.Join(root, "opt/tf/bin/tf-harness-tools"))
	for _, applet := range []string{"sh", "printf", "echo", "sleep", "cat"} {
		if err := os.Symlink("busybox", filepath.Join(root, "bin", applet)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func copyProbeBinary(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
}

// probeNetns creates an empty named network namespace for the jail to join,
// which is all the spec needs of the per-run network for this probe.
func probeNetns(t *testing.T, name string) string {
	t.Helper()
	if out, err := exec.Command("ip", "netns", "add", name).CombinedOutput(); err != nil {
		t.Skipf("cannot create a network namespace here: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("ip", "netns", "del", name).Run() })
	return "/var/run/netns/" + name
}

func readProbeFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("host read %s: %v", path, err)
	}
	return string(data)
}

func randomProbeSuffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return strings.ToLower(hex.EncodeToString(b))
}
