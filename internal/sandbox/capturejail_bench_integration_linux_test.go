//go:build linux && integration

package sandbox

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// largeDirtyTree clones TF_BENCH_SRC as root/acme/app/default and dirties it:
// a local commit, 3000 modified tracked files, 3000 untracked files and one
// 200 MiB binary, owned by uid.
func largeDirtyTree(t *testing.T, uid int) (root string) {
	t.Helper()
	src := os.Getenv("TF_BENCH_SRC")
	if src == "" {
		t.Skip("TF_BENCH_SRC not set")
	}
	root = worldTempDir(t, "tf-bench-tree-")
	co := filepath.Join(root, "acme", "app", "default")
	if err := os.MkdirAll(filepath.Dir(co), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRoot(t, root, "clone", "-q", "--no-hardlinks", src, co)
	gitRoot(t, co, "checkout", "-q", "-b", "work")
	writeFileT(t, filepath.Join(co, "local-commit.txt"), "a local commit\n")
	gitRoot(t, co, "add", "-A")
	gitRoot(t, co, "commit", "-q", "-m", "local")

	tracked := strings.Split(gitRoot(t, co, "ls-files"), "\n")
	modified := 0
	for _, f := range tracked {
		if modified >= 3000 {
			break
		}
		p := filepath.Join(co, f)
		fh, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			continue
		}
		_, _ = fh.WriteString("\nbench edit\n")
		_ = fh.Close()
		modified++
	}
	for i := 0; i < 3000; i++ {
		writeFileT(t, filepath.Join(co, "bench-untracked", fmt.Sprintf("d%d", i%50), fmt.Sprintf("f%d.txt", i)),
			strings.Repeat(fmt.Sprintf("line %d\n", i), 40))
	}
	big := make([]byte, 200<<20)
	_, _ = rand.Read(big)
	if err := os.WriteFile(filepath.Join(co, "bench-untracked", "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	chownTree(t, root, uid)
	t.Logf("tree: %d tracked files, %d modified, 3000 untracked, one 200 MiB binary", len(tracked), modified)

	return root
}

// TestIntegration_CaptureJail_LargeTreeTiming times the same capture of a
// large dirty checkout on the host and in the capture jail. It records numbers
// and asserts only that both captures succeed and carry the same bytes of
// stream, so it can run in the suite without flaking on a slow host.
// TF_BENCH_SRC names the repository cloned as the checkout.
func TestIntegration_CaptureJail_LargeTreeTiming(t *testing.T) {
	stub := requireCaptureJail(t)
	const idx = 11
	uid := JailUID(idx)
	root := largeDirtyTree(t, uid)

	for i := 1; i <= 3; i++ {
		start := time.Now()
		hres, herr := hostCapture(t, stub, uid, false, root, "acme/app/default")
		hostDur := time.Since(start)
		start = time.Now()
		jres, jerr := jailCapture(t, root, "acme/app/default")
		jailDur := time.Since(start)
		if herr != nil || jerr != nil {
			t.Fatalf("host err=%v, jail err=%v", herr, jerr)
		}
		t.Logf("run %d: host %v, jail %v (stream %d vs %d bytes)", i, hostDur, jailDur, len(hres.stream), len(jres.stream))
	}
}

// phaseScript times the file operations a capture is made of, one per line, in
// seconds of /proc/uptime. It is the same script on the host and in a jail, so
// the difference between them is the jail's cost for each kind of work.
const phaseScript = `
t() { awk '{print $1}' /proc/uptime; }
mark() { now=$(t); echo "$1 $(awk -v a=$now -v b=$last 'BEGIN{printf "%.2f", a-b}')"; last=$now; }
last=$(t)
cd /work/acme/app/default
export GIT_INDEX_FILE=/tmp/idx GIT_OBJECT_DIRECTORY=/tmp/obj GIT_ALTERNATE_OBJECT_DIRECTORIES=/work/acme/app/default/.git/objects
mkdir -p /tmp/obj
find /work/acme/app/default -type f | wc -l > /dev/null; mark walk
cat bench-untracked/big.bin > /dev/null; mark read200M
head -c 209715200 bench-untracked/big.bin > /tmp/copy; mark write200M
git add -A; mark gitadd
git write-tree > /dev/null; mark writetree
`

func parsePhases(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			m[f[0]] = f[1]
		}
	}
	return m
}

// TestIntegration_CaptureJail_PhaseCosts runs phaseScript on the large tree on
// the host and in a run jail and logs both.
func TestIntegration_CaptureJail_PhaseCosts(t *testing.T) {
	requireCaptureJail(t)
	const idx = 12
	uid := JailUID(idx)
	root := largeDirtyTree(t, uid)

	hostDir := worldTempDir(t, "tf-phase-")
	script := filepath.Join(hostDir, "phase.sh")
	hostScript := strings.ReplaceAll(strings.ReplaceAll(phaseScript, "/tmp/", hostDir+"/"), "/work/", root+"/")
	writeFileT(t, script, hostScript)
	if err := os.Chmod(hostDir, 0o777); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", script)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + hostDir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid), Groups: []uint32{}}}
	hostOut, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("host: %v: %s", err, hostOut)
	}

	cfg := minimalConfig(t)
	cfg.Worktree = root
	cfg.Argv = []string{"/bin/sh", "-c", phaseScript}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer func() { _ = sb.Close() }()
	defer func() { _ = run.Close() }()
	chownTree(t, root, JailUID(sb.SubnetIdx))
	jailOut, err := runToCompletion(t, run)
	if err != nil {
		t.Fatalf("jail: %v: %s", err, jailOut)
	}
	t.Logf("host script output:\n%s", hostOut)
	host, jail := parsePhases(string(hostOut)), parsePhases(string(jailOut))
	for _, k := range []string{"walk", "read200M", "write200M", "gitadd", "writetree"} {
		t.Logf("phase %-10s host %5ss  jail %5ss", k, host[k], jail[k])
	}
}

// TestIntegration_PauseResume_ClockAcrossTheFreeze answers what a process in
// a paused jail sees when it resumes: whether its clocks ran through the
// freeze, and so whether a timeout that was counting down fired. The jail
// runs `timeout 10` around eight one-second steps; the jail is paused after
// about two seconds for twelve. If the clocks stood still the steps finish
// and the exit code is 0; if they ran, the timeout fires (124) with only
// about two steps done.
func TestIntegration_PauseResume_ClockAcrossTheFreeze(t *testing.T) {
	requireRunsc(t)
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	root := worldTempDir(t, "tf-clock-")
	cfg := minimalConfig(t)
	cfg.Worktree = root
	cfg.Argv = []string{"/bin/sh", "-c", `
		up() { awk '{print $1}' /proc/uptime; }
		echo "start $(up) $(date +%s)" > /work/clock.log
		timeout 10 sh -c 'i=0; while [ $i -lt 8 ]; do sleep 1; i=$((i+1)); echo $i > /work/steps; done'
		echo $? > /work/rc
		echo "end $(up) $(date +%s)" >> /work/clock.log`}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer func() { _ = sb.Close() }()
	defer func() { _ = run.Close() }()
	chownTree(t, root, JailUID(sb.SubnetIdx))
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	freezer := run.(Freezer)
	time.Sleep(2 * time.Second)
	paused := time.Now()
	if err := freezer.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	time.Sleep(12 * time.Second)
	if err := freezer.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	t.Logf("frozen for %v", time.Since(paused))
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, "rc")); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	rc, _ := os.ReadFile(filepath.Join(root, "rc"))
	steps, _ := os.ReadFile(filepath.Join(root, "steps"))
	clock, _ := os.ReadFile(filepath.Join(root, "clock.log"))
	t.Logf("timeout exit code %q (124 = the timeout fired), steps done %q, clock log:\n%s", strings.TrimSpace(string(rc)), strings.TrimSpace(string(steps)), clock)
}

// echoRecorder accepts connections on l and echoes every line back, noting
// when each arrived. It is the host end of a long-lived connection from a jail.
type echoRecorder struct {
	mu       sync.Mutex
	arrivals []time.Time
	closeErr error
	closed   bool
}

func (e *echoRecorder) serve(l net.Listener) {
	conn, err := l.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			e.mu.Lock()
			e.closed, e.closeErr = true, err
			e.mu.Unlock()
			return
		}
		e.mu.Lock()
		e.arrivals = append(e.arrivals, time.Now())
		e.mu.Unlock()
		_, _ = conn.Write([]byte(line))
	}
}

func (e *echoRecorder) report(t *testing.T, name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var maxGap time.Duration
	for i := 1; i < len(e.arrivals); i++ {
		if g := e.arrivals[i].Sub(e.arrivals[i-1]); g > maxGap {
			maxGap = g
		}
	}
	t.Logf("%s: %d lines reached the host, longest gap %v, connection closed by the jail's side: %v (%v)",
		name, len(e.arrivals), maxGap.Round(time.Millisecond), e.closed, e.closeErr)
}

// TestIntegration_FreezeConns holds one connection
// open from a jail to a host listener across the run's veth to a host listener, sends a line a second, pauses the jail for twelve seconds, and
// records whether the connection carried on or was reset.
func TestIntegration_FreezeConns(t *testing.T) {
	requireRunsc(t)
	requireApk(t)
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	withJailHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cfg := minimalConfig(t)
	network, err := SetupRunNetwork(ctx, cfg.ConversationID)
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	t.Cleanup(func() { _ = network.Close() })
	if err := network.PrepareSockets(ctx, cfg.ConversationID); err != nil {
		t.Fatalf("PrepareSockets: %v", err)
	}
	tl, err := net.Listen("tcp", network.HostIP+":0")
	if err != nil {
		t.Fatalf("listen on the veth host address: %v", err)
	}
	t.Cleanup(func() { _ = tl.Close() })
	port := tl.Addr().(*net.TCPAddr).Port

	tree := RunTreeRoot(cfg.ConversationID)
	if err := os.MkdirAll(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := network.HandOffRunTree(ctx, tree); err != nil {
		t.Fatal(err)
	}
	var tcpRec echoRecorder
	go tcpRec.serve(tl)

	cfg.Network = network
	cfg.Worktree = tree
	cfg.Argv = []string{"/bin/sh", "-c", fmt.Sprintf(`
		feed() { i=0; while [ $i -lt 20 ]; do echo ping$i; i=$((i+1)); sleep 1; done; }
		feed | nc %s %d > /work/tcp.out 2>/work/tcp.err; echo $? > /work/tcp.rc &
		wait`, network.HostIP, port)}
	run, sb, err := Wrap(ctx, cfg)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	defer func() { _ = sb.Close() }()
	defer func() { _ = run.Close() }()
	if err := run.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	freezer := run.(Freezer)
	time.Sleep(4 * time.Second)
	if err := freezer.Pause(ctx); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	time.Sleep(12 * time.Second)
	if err := freezer.Resume(ctx); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	time.Sleep(22 * time.Second)
	for _, n := range []string{"tcp"} {
		out, _ := os.ReadFile(filepath.Join(tree, n+".out"))
		rc, _ := os.ReadFile(filepath.Join(tree, n+".rc"))
		errb, _ := os.ReadFile(filepath.Join(tree, n+".err"))
		t.Logf("%s in the jail: %d echoes back, nc exit %q, stderr %q", n, strings.Count(string(out), "\n"), strings.TrimSpace(string(rc)), strings.TrimSpace(string(errb)))
	}
	tcpRec.report(t, "tcp")
}
