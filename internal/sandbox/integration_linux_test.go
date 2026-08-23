//go:build linux && integration

// Integration tests for the sandbox package. Build-tagged so:
//
//   - Standard `go test ./...` skips them (no runsc needed)
//   - CI / dev with runsc installed runs via `go test -tags integration`
//
// Each test calls Wrap() with a busybox-shaped payload and asserts
// against the acceptance criteria (Property B env curation,
// filesystem isolation, loopback isolation, cleanup).
//
// Run prerequisites:
//   - Linux (build tag enforced)
//   - runsc on PATH (https://gvisor.dev/releases)
//   - iptables + iproute2
//   - root or CAP_NET_ADMIN + CAP_SYS_ADMIN (gVisor needs these)
//
// Tests SKIP gracefully when prerequisites are missing rather than
// failing — so the same suite can run on a stripped-down dev box.

package sandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMain pre-warms the rootfs cache once with a generous timeout
// before the per-test suite runs. Without this, the first test to
// call Wrap() on a fresh machine pays the full cold-cache cost
// (download alpine tarball + apk-add ~500MB of toolchain) under its
// own 30-second test context — which it will lose. After this pre-
// warm, every test hits the hot-cache sentinel path and Wrap()
// returns in milliseconds for the rootfs step.
//
// Best-effort: if runsc/chroot/root prereqs aren't met, the pre-warm
// is skipped and individual tests still skip cleanly via require*.
func TestMain(m *testing.M) {
	// Re-exec'd test-child branches must intercept before any suite setup runs,
	// exactly as they do in the default binary: the sidecar-reap comm helper,
	// the live-sidecar stand-in (LaunchSidecarProcess execs this binary with the
	// run-sidecar argv), and the cross-sidecar ptrace probe. Each sets itself up
	// and blocks or exits, never reaching the pre-warm or m.Run below.
	reExecAsSidecarHelperIfRequested()
	maybeRunSidecarSubcommand()
	maybeRunPtraceProbe()

	// Production launches every run through the cap-broker; this package
	// can't spin one up (import cycle), so the suite installs in-process
	// stand-ins built from the same launch primitives the broker uses. Wrap()
	// routes its launch through runLauncher and LaunchSidecar through
	// sidecarLauncher, so both must be set before any test calls them.
	runLauncher = inProcessLauncher{}
	sidecarHarnessCleanup, err := installInProcessSidecarHarness()
	if err != nil {
		// Record it, don't abort: the live-sidecar tests fail loudly with this
		// exact cause (via sidecarHarnessErr), while the non-sidecar integration
		// tests — which don't use the harness — still run.
		sidecarHarnessErr = err
		fmt.Fprintf(os.Stderr, "TestMain: sidecar harness setup failed: %v\n", err)
	}

	if shouldPreWarmRootfs() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		if _, err := ensureRootfs(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "TestMain: rootfs pre-warm failed (tests may time out on cold cache): %v\n", err)
		}
		cancel()
	}
	code := m.Run()
	sidecarHarnessCleanup()
	os.Exit(code)
}

// shouldPreWarmRootfs gates the pre-warm on the same prereqs the
// integration tests themselves check. No point downloading 500MB if
// the suite is going to skip every test anyway — keep this list in
// sync with requireRunsc + requireApk.
func shouldPreWarmRootfs() bool {
	if os.Geteuid() != 0 {
		return false
	}
	for _, bin := range []string{"runsc", "ip", "iptables", "chroot"} {
		if _, err := exec.LookPath(bin); err != nil {
			return false
		}
	}
	return true
}

func requireRunsc(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("runsc not installed; skipping integration test")
	}
	if _, err := exec.LookPath("ip"); err != nil {
		t.Skip("iproute2 not installed; skipping integration test")
	}
	if _, err := exec.LookPath("iptables"); err != nil {
		t.Skip("iptables not installed; skipping integration test")
	}
}

// requireApk gates the rootfs-toolchain integration tests on a host
// that can actually populate the cache (chroot + apk-add). Same
// skip pattern as requireRunsc — the suite runs end-to-end on the
// sandbox-CI box but degrades gracefully on stripped-down dev boxes.
func requireApk(t *testing.T) {
	t.Helper()
	requireRunsc(t)
	if _, err := exec.LookPath("chroot"); err != nil {
		t.Skip("chroot not installed; skipping rootfs-toolchain test")
	}
	if os.Geteuid() != 0 {
		t.Skip("rootfs toolchain build needs root for chroot; skipping")
	}
}

// runToCompletion starts the launched run, drains its stdout, waits for
// exit, and returns the combined stdout+stderr — the P2 replacement for the
// pre-split *exec.Cmd.CombinedOutput the Wrap tests used before Wrap returned
// a LaunchedRun. Draining before Wait mirrors CombinedOutput's ordering so a
// chatty payload never blocks on a full pipe.
func runToCompletion(t *testing.T, run LaunchedRun) ([]byte, error) {
	t.Helper()
	if err := run.Start(); err != nil {
		return nil, err
	}
	out, readErr := io.ReadAll(run.Stdout())
	waitErr := run.Wait()
	combined := append(out, []byte(run.Stderr())...)
	// The exit error is the primary signal (mirrors CombinedOutput); surface
	// a stdout read failure only when the run otherwise exited cleanly, so a
	// truncated/broken stream isn't masked by a zero exit.
	if waitErr != nil {
		return combined, waitErr
	}
	return combined, readErr
}

// minimalConfig builds a Config for tests with sensible defaults +
// a unique ConversationID. Caller can mutate Argv to choose the payload.
func minimalConfig(t *testing.T) Config {
	t.Helper()
	worktree := t.TempDir()
	if err := os.Chown(worktree, WorktreeUID, WorktreeGID); err != nil {
		t.Skipf("can't chown worktree to UID %d (probably not root): %v", WorktreeUID, err)
	}
	return Config{
		ConversationID: "itest" + t.Name()[:min(len(t.Name()), 6)],
		Worktree:       worktree,
		Argv:           []string{"/bin/echo", "hello"},
		Env: []string{
			"PATH=/usr/local/bin:/usr/bin:/bin",
			"HOME=/work",
		},
	}
}

// TestIntegration_BootBusyboxPayload — Wrap, Start+Wait an echo
// command, observe "hello" on stdout. Smoke test that the whole
// pipeline (subnet alloc → netns → veth → bundle → runsc) works.
func TestIntegration_BootBusyboxPayload(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	out, err := runToCompletion(t, run)
	if err != nil {
		t.Fatalf("run: %v (output: %s)", err, out)
	}
	if !strings.Contains(string(out), "hello") {
		t.Errorf("output missing 'hello': %s", out)
	}
}

// TestIntegration_PropertyB_NoCredentialsInEnv is the load-bearing
// security test. Sets sentinel env vars in the test process; the
// sandboxed `env` payload must NOT see them.
func TestIntegration_PropertyB_NoCredentialsInEnv(t *testing.T) {
	requireRunsc(t)

	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-MUST-NOT-LEAK")
	t.Setenv("GITHUB_TOKEN", "ghp_MUST_NOT_LEAK")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "AWS-MUST-NOT-LEAK")

	cfg := minimalConfig(t)
	cfg.Argv = []string{"/usr/bin/env"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	out, err := runToCompletion(t, run)
	if err != nil {
		t.Fatalf("run: %v (output: %s)", err, out)
	}
	for _, sentinel := range []string{
		"sk-ant-MUST-NOT-LEAK",
		"ghp_MUST_NOT_LEAK",
		"AWS-MUST-NOT-LEAK",
	} {
		if strings.Contains(string(out), sentinel) {
			t.Errorf("Property B VIOLATED: sandbox env contains host sentinel %q\n--- env output ---\n%s",
				sentinel, out)
		}
	}
}

// TestIntegration_NonRootUID confirms the agent runs as UID 10000,
// not root. T3 hardening requires this.
func TestIntegration_NonRootUID(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)
	cfg.Argv = []string{"/usr/bin/id", "-u"}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	out, err := runToCompletion(t, run)
	if err != nil {
		t.Fatalf("run: %v (output: %s)", err, out)
	}
	got := strings.TrimSpace(string(out))
	want := "10000"
	if got != want {
		t.Errorf("sandbox UID = %q, want %q (non-root for T3 hardening)", got, want)
	}
}

// TestIntegration_WorktreeIsolation confirms host paths outside the
// worktree bind-mount are invisible to the sandbox.
func TestIntegration_WorktreeIsolation(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)
	// Stash a sentinel on the host that the sandbox shouldn't see.
	if err := os.WriteFile("/tmp/host-sentinel-"+t.Name(),
		[]byte("must-not-be-readable-from-sandbox"), 0o644); err != nil {
		t.Skipf("can't write host sentinel: %v", err)
	}
	defer os.Remove("/tmp/host-sentinel-" + t.Name())

	cfg.Argv = []string{"/bin/cat", "/tmp/host-sentinel-" + t.Name()}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	out, _ := runToCompletion(t, run)
	if strings.Contains(string(out), "must-not-be-readable") {
		t.Errorf("filesystem isolation broken: sandbox read host sentinel\n--- output ---\n%s", out)
	}
}

// TestIntegration_CleanupRemovesNetns runs Wrap → Close and asserts
// the netns no longer exists at /var/run/netns/.
func TestIntegration_CleanupRemovesNetns(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer run.Close()
	netnsPath := sb.NetnsPath
	_, _ = runToCompletion(t, run) // run + finish

	if _, err := os.Stat(netnsPath); err != nil {
		t.Errorf("netns gone before Close — that's wrong, runsc shouldn't auto-clean")
	}

	if err := sb.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if _, err := os.Stat(netnsPath); !os.IsNotExist(err) {
		t.Errorf("netns %s still exists after Close (stat err = %v)", netnsPath, err)
	}
}

// TestIntegration_ReapOrphans creates an orphan netns by skipping
// Close, then verifies ReapOrphans cleans it up.
func TestIntegration_ReapOrphans(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer run.Close()
	netnsPath := sb.NetnsPath
	// Skip sb.Close — leave an orphan netns for ReapOrphans. run.Close only
	// reclaims the run's pipes/cgroup, not the netns.

	if err := ReapOrphans(ctx); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if _, err := os.Stat(netnsPath); !os.IsNotExist(err) {
		t.Errorf("orphan netns %s not reaped", netnsPath)
	}
}

// --- Rootfs toolchain tests ------------------------------------------------
//
// These confirm the apk-installed toolchain in the cached rootfs is
// actually reachable from inside the sandbox. Each test runs a tiny
// "are you there?" probe against one of the packages installed by
// installToolchain (see rootfs_linux.go). If any of these fail, real
// agent runs will fail at the first Bash(...) invocation that tries
// to use that tool.

func toolchainTest(t *testing.T, argv []string, wantSubstring string, extraEnv ...string) {
	t.Helper()
	requireApk(t)
	cfg := minimalConfig(t)
	cfg.Argv = argv
	cfg.Env = append(cfg.Env, extraEnv...)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	out, err := runToCompletion(t, run)
	if err != nil {
		t.Fatalf("%v inside sandbox failed: %v (output: %s)", argv, err, out)
	}
	if !strings.Contains(string(out), wantSubstring) {
		t.Errorf("%v output missing %q:\n%s", argv, wantSubstring, out)
	}
}

func TestIntegration_RootfsHasNode(t *testing.T) {
	toolchainTest(t, []string{"/usr/bin/node", "-e", "console.log('ok')"}, "ok")
}

func TestIntegration_RootfsHasGit(t *testing.T) {
	toolchainTest(t, []string{"/usr/bin/git", "--version"}, "git version")
}

func TestIntegration_RootfsHasRipgrep(t *testing.T) {
	toolchainTest(t, []string{"/usr/bin/rg", "--version"}, "ripgrep")
}

func TestIntegration_RootfsHasPython(t *testing.T) {
	toolchainTest(t, []string{"/usr/bin/python3", "-c", "print('ok')"}, "ok")
}

func TestIntegration_RootfsHasGo(t *testing.T) {
	toolchainTest(t, []string{"/usr/bin/go", "version"}, "go version")
}

// TestIntegration_RootfsHasPnpm asserts pnpm is reachable on PATH inside
// the sandbox: nodejs/npm alone don't provide it, so installToolchain
// (rootfs_linux.go) npm-global-installs the pinned pnpm on top. A plain
// `pnpm --version` runs the baked binary straight off PATH with no
// network and must report pnpmVersion — the version this repo's frontend
// pins.
func TestIntegration_RootfsHasPnpm(t *testing.T) {
	toolchainTest(t, []string{"/usr/bin/pnpm", "--version"}, pnpmVersion)
}

// TestIntegration_ConfigureProxies_InjectsEnv is the
// sandbox-side proxy wiring test. Asserts:
//
//   - The ConfigureProxies callback is invoked with a Sandbox whose
//     HostIP is populated (the network is up by the time it fires)
//   - Env entries returned by the callback show up in the agent's
//     /proc/self/environ exactly as written
//   - The original Config.Env is preserved alongside (the callback
//     ADDS to, doesn't replace)
//
// Pins the load-bearing behavior of this hook so a future
// refactor that misorders the network/spec phases or drops the env
// merge will fail loudly.
func TestIntegration_ConfigureProxies_InjectsEnv(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)
	cfg.Argv = []string{"/usr/bin/env"}

	var observedHostIP string
	cfg.ConfigureProxies = func(s *Sandbox) ([]string, error) {
		observedHostIP = s.HostIP
		// Sentinel env entries the agent should see. Real callers
		// inject ANTHROPIC_BASE_URL etc — we use neutral sentinels
		// so the test doesn't conflate "callback ran" with "real
		// proxy is up".
		return []string{
			"SKY335_PROXY_URL=http://" + s.HostIP + ":54321",
			"SKY335_PLACEHOLDER=sk-PROXY-PLACEHOLDER",
		}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	if observedHostIP == "" {
		t.Fatal("ConfigureProxies invoked but HostIP empty; network setup must complete before callback")
	}
	if observedHostIP != sb.HostIP {
		t.Errorf("callback saw HostIP %q, Sandbox reports %q; mismatch", observedHostIP, sb.HostIP)
	}

	out, err := runToCompletion(t, run)
	if err != nil {
		t.Fatalf("env in sandbox: %v (output: %s)", err, out)
	}

	if !strings.Contains(string(out), "SKY335_PROXY_URL=http://"+observedHostIP+":54321") {
		t.Errorf("sandbox env missing proxy URL injected by callback:\n%s", out)
	}
	if !strings.Contains(string(out), "SKY335_PLACEHOLDER=sk-PROXY-PLACEHOLDER") {
		t.Errorf("sandbox env missing placeholder injected by callback:\n%s", out)
	}
	// Original cfg.Env entries must also survive — the callback
	// adds to, doesn't replace.
	if !strings.Contains(string(out), "PATH=/usr/local/bin:/usr/bin:/bin") {
		t.Errorf("sandbox env missing original PATH; callback overrode instead of appending:\n%s", out)
	}
}

// TestIntegration_ConfigureProxies_ErrorAborts pins the error
// propagation invariant: when the callback returns an error, Wrap
// fails (no sandbox is started, no bundle is left on disk) and the
// caller can defer Shutdown safely on the nil return.
func TestIntegration_ConfigureProxies_ErrorAborts(t *testing.T) {
	requireRunsc(t)
	cfg := minimalConfig(t)
	wantErr := "synthetic proxy startup failure"
	cfg.ConfigureProxies = func(s *Sandbox) ([]string, error) {
		return nil, &configureProxyError{msg: wantErr}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err == nil {
		if sb != nil {
			_ = sb.Close()
		}
		t.Fatal("Wrap returned nil err despite ConfigureProxies failure")
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("err = %v; want it to wrap %q", err, wantErr)
	}
	if run != nil {
		t.Error("Wrap returned non-nil run on error path; caller would mistakenly try to Start")
	}
	if sb != nil {
		t.Error("Wrap returned non-nil Sandbox on error path; caller's defer Close would target a torn-down state")
	}
}

type configureProxyError struct{ msg string }

func (e *configureProxyError) Error() string { return e.msg }

// TestIntegration_AgentHostIPC_RoundTrip — end-to-end.
//
// Builds cmd/exec-test-stub as a pure-Go static binary, bind-mounts
// it into the sandbox at /usr/local/bin/triagefactory, starts a
// listener on a temp socket the sandbox can reach via bind mount at
// /run/tf.sock, and exec's the stub. The stub dials /run/tf.sock,
// sends a LookupConversation probe RPC, and prints the result on stdout. The
// test asserts the round-trip succeeded and the response carries the
// run id the host registered.
//
// Why not the real `triagefactory` binary: the host TF binary is
// glibc-linked on most dev/CI systems, and the sandbox rootfs is
// alpine (musl) — a bind-mounted glibc binary fails to exec inside
// alpine because the dynamic loader path doesn't resolve. The
// production fix is a static-built musl image of the real binary;
// this test proves the IPC pipe with the stub.
func TestIntegration_AgentHostIPC_RoundTrip(t *testing.T) {
	requireRunsc(t)
	requireApk(t)

	// Build the test stub as a pure-Go static binary so it can exec
	// inside alpine. CGO_ENABLED=0 skips the glibc/musl linker entirely.
	stubBin := filepath.Join(t.TempDir(), "exec-test-stub")
	build := exec.Command("go", "build", "-o", stubBin,
		"github.com/sky-ai-eng/triage-factory/cmd/exec-test-stub")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test stub: %v (output: %s)", err, out)
	}
	// The bind-mount source path must be reachable by root inside the
	// sandbox-launch process; t.TempDir is /tmp/<random> which works.
	if err := os.Chmod(stubBin, 0o755); err != nil {
		t.Fatalf("chmod stub: %v", err)
	}

	// Per-run socket on the host. Mode 0700 on the parent dir; the
	// listener inherits from umask, then we chown + chmod to the
	// sandbox UID exactly as the production agenthost.StartWithServer does.
	sockDir := t.TempDir()
	if err := os.Chmod(sockDir, 0o700); err != nil {
		t.Fatalf("chmod sock dir: %v", err)
	}
	sockPath := filepath.Join(sockDir, "test.sock")
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen on %s: %v", sockPath, err)
	}
	if err := os.Chown(sockPath, WorktreeUID, WorktreeGID); err != nil {
		t.Skipf("can't chown socket to UID %d (probably not root): %v", WorktreeUID, err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		t.Fatalf("chmod sock: %v", err)
	}
	// Parent dir must be reachable by the sandbox UID too — chown the
	// parent so the runsc bind-mount can traverse to the socket.
	if err := os.Chown(sockDir, WorktreeUID, WorktreeGID); err != nil {
		t.Skipf("can't chown sock dir to UID %d: %v", WorktreeUID, err)
	}

	// Minimal echo daemon: accept one connection, read one frame,
	// send back a synthetic LookupConversation result. We don't want to drag
	// the full agenthost.Server (and its db.Stores dep) into the
	// sandbox-package test — the IPC pipe round-trip is what we're
	// proving, and the frame format is identical to what the real
	// daemon uses. Stop the goroutine via listener close at test end.
	type req struct {
		V uint32          `json:"v"`
		M string          `json:"m"`
		A json.RawMessage `json:"a,omitempty"`
	}
	type resp struct {
		R json.RawMessage `json:"r,omitempty"`
		E string          `json:"e,omitempty"`
	}
	sentinelConversationID := "itest-agenthost-ipc"
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		var header [4]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		length := binary.BigEndian.Uint32(header[:])
		body := make([]byte, length)
		if _, err := io.ReadFull(conn, body); err != nil {
			return
		}
		var r req
		_ = json.Unmarshal(body, &r)
		// Echo back a LookupConversation-shaped response with our sentinel run id.
		result := []byte(`{"info":{"org_id":"00000000-0000-0000-0000-000000000001","user_id":"","conversation_id":"` + sentinelConversationID + `","is_event_triggered":false}}`)
		respBody, _ := json.Marshal(resp{R: result})
		var outHeader [4]byte
		binary.BigEndian.PutUint32(outHeader[:], uint32(len(respBody)))
		_, _ = conn.Write(outHeader[:])
		_, _ = conn.Write(respBody)
	}()
	t.Cleanup(func() { _ = listener.Close() })

	cfg := minimalConfig(t)
	cfg.Argv = []string{"/usr/local/bin/triagefactory"}
	cfg.ExtraMounts = []Mount{
		{Source: stubBin, Destination: "/usr/local/bin/triagefactory", Options: []string{"ro"}},
		{Source: sockPath, Destination: "/run/tf.sock"},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	defer run.Close()

	out, err := runToCompletion(t, run)
	if err != nil {
		t.Fatalf("stub exec: %v (output: %s)", err, out)
	}
	if !strings.Contains(string(out), sentinelConversationID) {
		t.Errorf("expected stub stdout to echo run id %q, got: %s", sentinelConversationID, out)
	}
}

// Ensure filepath is used (defensive; gofmt would otherwise drop it
// if the only reference goes away during a future edit).
var _ = filepath.Join

// runtimeStateEntriesFor lists the entries runsc still holds under its state
// root for one container id — the on-disk residue a killed jail leaves and a
// graceful one does not.
func runtimeStateEntriesFor(containerID string) []string {
	entries, err := os.ReadDir(runscStateRoot())
	if err != nil {
		return nil
	}
	var found []string
	for _, e := range entries {
		if id, ok := containerIDFromRuntimeStateEntry(e.Name()); ok && id == containerID {
			found = append(found, e.Name())
		}
	}
	return found
}

// TestIntegration_KilledJailLeavesNoRuntimeState pins the eager half of the
// state hygiene against a real runsc: `runsc run` removes its own state only
// on a graceful exit, so a SIGKILLed jail — which is how every stop and every
// cancellation ends one — would otherwise leave it behind forever. After the
// reap (Kill then Wait, the broker's own sequence) nothing may remain.
func TestIntegration_KilledJailLeavesNoRuntimeState(t *testing.T) {
	requireRunsc(t)

	cfg := minimalConfig(t)
	cfg.Argv = []string{"/bin/sleep", "60"}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer sb.Close()
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_ = run.Close() // SIGKILL, exactly as a stop does
	_ = run.Wait()  // the reap that follows it

	if left := runtimeStateEntriesFor(sb.ContainerID); len(left) > 0 {
		t.Errorf("runsc state for %s survived the reap: %v", sb.ContainerID, left)
	}
}

// TestIntegration_ReusedContainerIDAfterUnreapedKill is the reported failure
// end to end: stop a run, follow up on it, and the resume mints the identical
// container id — same conversation, and the allocator hands back the index
// the stop just freed. Here the kill is deliberately left unreaped (a crashed
// broker, a host that died mid-run), so the corpse's state is still on disk
// when the second launch asks runsc to create over it.
func TestIntegration_ReusedContainerIDAfterUnreapedKill(t *testing.T) {
	requireRunsc(t)

	cfg := minimalConfig(t)
	cfg.Argv = []string{"/bin/sleep", "60"}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	first, sb1, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap (first): %v", err)
	}
	if err := first.Start(); err != nil {
		t.Fatalf("Start (first): %v", err)
	}
	killedID := sb1.ContainerID

	// Kill and abandon: no Wait, so nothing reaps the state. Closing the
	// sandbox returns the subnet index to the pool, which is what makes the
	// next launch re-mint this exact id.
	_ = first.Close()
	_ = sb1.Close()

	cfg.Argv = []string{"/bin/echo", "resumed"}
	second, sb2, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap (resume): %v", err)
	}
	defer sb2.Close()
	defer second.Close()
	if sb2.ContainerID != killedID {
		t.Fatalf("resume container id = %q, want the killed jail's %q — this test no longer exercises id reuse", sb2.ContainerID, killedID)
	}

	out, err := runToCompletion(t, second)
	if err != nil {
		t.Fatalf("resumed run failed on a reused container id: %v (output: %s)", err, out)
	}
	if !strings.Contains(string(out), "resumed") {
		t.Errorf("resumed run output = %q, want the payload to have run", out)
	}
}
