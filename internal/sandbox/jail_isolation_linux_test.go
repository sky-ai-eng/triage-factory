//go:build linux

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// accessProbeEnv gates this test binary's access-probe re-exec: the child
// performs one filesystem or socket operation as whatever identity it was
// started with, prints ALLOWED or DENIED, and exits. Driven from both TestMains
// (runAccessProbeIfRequested), like the sidecar-reap helper.
const accessProbeEnv = "TF_TEST_ACCESS_PROBE"

// runAccessProbeIfRequested is the probe child's whole body. The operation is
// "<op>:<path>":
//
//	connect — dial the unix socket at path
//	read    — read the file at path
//	list    — read the directory at path
//	create  — create (and remove again) a new file inside the directory at path
func runAccessProbeIfRequested() {
	spec := os.Getenv(accessProbeEnv)
	if spec == "" {
		return
	}
	op, path, _ := strings.Cut(spec, ":")
	var err error
	switch op {
	case "connect":
		var c net.Conn
		if c, err = net.DialTimeout("unix", path, 2*time.Second); err == nil {
			_ = c.Close()
		}
	case "read":
		_, err = os.ReadFile(path)
	case "list":
		_, err = os.ReadDir(path)
	case "create":
		name := filepath.Join(path, "probe-"+strconv.Itoa(os.Getpid()))
		var f *os.File
		if f, err = os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
			_ = f.Close()
			_ = os.Remove(name)
		}
	default:
		err = fmt.Errorf("unknown probe op %q", op)
	}
	if err == nil {
		fmt.Print("ALLOWED")
	} else {
		fmt.Print("DENIED: " + err.Error())
	}
	os.Exit(0)
}

// identity is one host identity a probe runs as: a uid, its primary gid and
// its supplementary groups, exactly as a credential the broker would hand out.
type identity struct {
	name   string
	uid    int
	gid    int
	groups []int
}

func jailIdentity(idx uint8) identity {
	return identity{name: fmt.Sprintf("jail[%d]", idx), uid: JailUID(idx), gid: JailGID(idx)}
}

func sidecarIdentity(idx uint8) identity {
	return identity{name: fmt.Sprintf("sidecar[%d]", idx), uid: SidecarUID(idx), gid: SidecarUID(idx), groups: sidecarGroupsInts(SidecarUID(idx))}
}

func sidecarGroupsInts(uid int) []int {
	var out []int
	for _, g := range sidecarGroups(uid) {
		out = append(out, int(g))
	}
	return out
}

// prober runs access probes from a copy of this test binary placed where any
// uid can exec it (the go-build cache directory is 0700).
type prober struct {
	t   *testing.T
	bin string
}

func newProber(t *testing.T) *prober {
	t.Helper()
	return &prober{t: t, bin: copyToAccessibleBinary(t, os.Args[0])}
}

// probe performs op on path as id and reports whether it was allowed, with the
// child's own account of a denial for the failure message.
func (p *prober) probe(id identity, op, path string) (bool, string) {
	p.t.Helper()
	cmd := exec.Command(p.bin)
	cmd.Env = append(os.Environ(), accessProbeEnv+"="+op+":"+path)
	groups := make([]uint32, 0, len(id.groups))
	for _, g := range id.groups {
		groups = append(groups, uint32(g))
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(id.uid), Gid: uint32(id.gid), Groups: groups}}
	out, err := cmd.CombinedOutput()
	if err != nil {
		p.t.Fatalf("probe %s %s as %s did not run: %v (%s)", op, path, id.name, err, out)
	}
	got := strings.TrimSpace(string(out))
	return got == "ALLOWED", got
}

func (p *prober) mustAllow(id identity, op, path string) {
	p.t.Helper()
	if ok, got := p.probe(id, op, path); !ok {
		p.t.Errorf("%s could not %s %s, which is its own run's (%s) — the probe or the grant is broken", id.name, op, path, got)
	}
}

func (p *prober) mustDeny(id identity, op, path string) {
	p.t.Helper()
	if ok, _ := p.probe(id, op, path); ok {
		p.t.Errorf("CROSS-RUN HOLE: %s could %s %s", id.name, op, path)
	}
}

// testCell is one run's host-side cell as production lays it out: its socket
// directories with both sockets bound and granted, its run tree handed to its
// jail, and its entity-memory staging granted to its jail.
type testCell struct {
	conv, task  string
	idx         uint8
	agentSock   string
	toolSock    string
	tree        string
	treeFile    string
	staging     string
	stagingFile string
}

// newTestCell builds the cell for conv at idx, standing in for each process the
// way production divides the work: the broker's ops (hostOps) create and grant,
// a listener chowned to the sidecar's uid stands in for the sidecar's own
// socket, and this process (root) stands in for the orchestrator, which creates
// the tree, the staging and the tool-host socket.
func newTestCell(t *testing.T, conv, task string, idx uint8) testCell {
	t.Helper()
	ctx := context.Background()
	c := testCell{conv: conv, task: task, idx: idx}

	if err := (hostOps{}).PrepareRunSockets(ctx, conv, idx); err != nil {
		t.Fatalf("PrepareRunSockets(%s): %v", conv, err)
	}
	c.agentSock = TrustedAgentHostSocketPath(conv)
	listenForProbes(t, c.agentSock)
	// What the sidecar's grant leaves: its own socket, group the jail gid.
	if err := os.Chown(c.agentSock, SidecarUID(idx), JailGID(idx)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(c.agentSock, 0o660); err != nil {
		t.Fatal(err)
	}
	c.toolSock = filepath.Join(TrustedToolHostSocketDir(conv), "tools.sock")
	listenForProbes(t, c.toolSock)
	if err := os.Chmod(c.toolSock, 0o660); err != nil {
		t.Fatal(err)
	}

	c.tree = RunTreeRoot(task)
	if err := os.MkdirAll(c.tree, 0o700); err != nil {
		t.Fatal(err)
	}
	c.treeFile = filepath.Join(c.tree, "checkout.txt")
	if err := os.WriteFile(c.treeFile, []byte(conv), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (hostOps{}).ChownRunTree(ctx, c.tree, "", JailUID(idx)); err != nil {
		t.Fatalf("ChownRunTree(%s): %v", task, err)
	}

	c.staging = TrustedMemorySourcePath(conv)
	if err := os.MkdirAll(c.staging, 0o700); err != nil {
		t.Fatal(err)
	}
	c.stagingFile = filepath.Join(c.staging, "task-context.md")
	if err := os.WriteFile(c.stagingFile, []byte("prior memory of "+conv), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (hostOps{}).GrantRunStaging(ctx, c.staging, idx); err != nil {
		t.Fatalf("GrantRunStaging(%s): %v", conv, err)
	}
	return c
}

// listenForProbes binds a unix socket at path that accepts and drops every
// connection, so a dial that gets through succeeds.
func listenForProbes(t *testing.T, path string) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
}

// withJailHost points every host path the cell uses — the socket root, the runs
// base, the staging bases — into a fresh directory any uid may traverse, laid
// out with the modes production gives them, and returns the socket root.
func withJailHost(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	// t.TempDir's own directories are 0700; a probe at another uid has to be
	// able to traverse to the paths under test, as it can to /run/tf and /tmp.
	for _, d := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(d, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", base)

	root := filepath.Join(base, "run-tf")
	if err := os.Mkdir(root, 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o711); err != nil {
		t.Fatal(err)
	}
	orig := trustedAgentHostSocketRoot
	trustedAgentHostSocketRoot = root
	t.Cleanup(func() { trustedAgentHostSocketRoot = orig })

	// The runs base at production's mode. The staging bases are opened to
	// traversal too, which production does not need — a jail reads its staging
	// through its mount, never by this path — so the positive controls can reach
	// a run's own staging from the host; what keeps the others out is each
	// staging directory's own grant, which is what the negatives test.
	for _, d := range []string{filepath.Dir(RunTreeRoot("x")), SkillStagingBase(), MemoryStagingBase()} {
		if err := os.Mkdir(d, 0o711); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestJailIdentity_HostProcessesCannotReachAnotherRun is the host-side half of
// per-run jail identity, with two live cells built the way production builds
// them. A host process holding run A's jail identity — the capture child's —
// or run A's sidecar identity reaches run A's sockets, tree and staging and
// none of run B's, and neither can create anything in the socket root.
//
// Every denial is paired with the same probe allowed against the run's own
// cell, so a denial is the boundary and not a broken probe.
func TestJailIdentity_HostProcessesCannotReachAnotherRun(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to build two cells and probe them at other uids")
	}
	socketRoot := withJailHost(t)
	a := newTestCell(t, "conv-a", "task-a", 3)
	b := newTestCell(t, "conv-b", "task-b", 5)
	p := newProber(t)

	jailA, sidecarA := jailIdentity(a.idx), sidecarIdentity(a.idx)

	// Run A's jail identity: its own cell, all of it.
	p.mustAllow(jailA, "connect", a.agentSock)
	p.mustAllow(jailA, "connect", a.toolSock)
	p.mustAllow(jailA, "read", a.treeFile)
	p.mustAllow(jailA, "create", a.tree)
	p.mustAllow(jailA, "read", a.stagingFile)

	// … and none of run B's.
	p.mustDeny(jailA, "connect", b.agentSock)
	p.mustDeny(jailA, "connect", b.toolSock)
	p.mustDeny(jailA, "read", b.treeFile)
	p.mustDeny(jailA, "list", b.tree)
	p.mustDeny(jailA, "create", b.tree)
	p.mustDeny(jailA, "read", b.stagingFile)
	p.mustDeny(jailA, "list", b.staging)
	p.mustDeny(jailA, "create", socketRoot)

	// Run A's sidecar: its own socket, none of run B's, nothing in the root.
	p.mustAllow(sidecarA, "connect", a.agentSock)
	p.mustDeny(sidecarA, "connect", b.agentSock)
	p.mustDeny(sidecarA, "connect", b.toolSock)
	p.mustDeny(sidecarA, "read", b.treeFile)
	p.mustDeny(sidecarA, "read", b.stagingFile)
	p.mustDeny(sidecarA, "create", socketRoot)
	p.mustDeny(sidecarA, "create", RunSocketDir(b.conv))
}

// TestJailIdentity_SecondClaimWithAnotherIndex covers a task's tree across two
// engagements. When the first ends, its tree is parked and its staging grant
// taken back, so its jail identity — free to be handed to another run — reaches
// neither. The next claim of the same task holds a different index, and the
// hand-off gives it the whole tree, readable and writable.
func TestJailIdentity_SecondClaimWithAnotherIndex(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to build a cell and probe it at other uids")
	}
	withJailHost(t)
	first := newTestCell(t, "conv-a", "task-a", 3)
	p := newProber(t)
	ctx := context.Background()

	// The first engagement ends.
	if err := (hostOps{}).ParkRunTree(ctx, first.tree); err != nil {
		t.Fatalf("ParkRunTree: %v", err)
	}
	if err := revokeRunStaging(first.staging); err != nil {
		t.Fatalf("revoke staging: %v", err)
	}
	if err := (hostOps{}).ReleaseRunSockets(ctx, first.conv, first.idx); err != nil {
		t.Fatalf("ReleaseRunSockets: %v", err)
	}
	released := jailIdentity(first.idx)
	p.mustDeny(released, "read", first.treeFile)
	p.mustDeny(released, "create", first.tree)
	p.mustDeny(released, "read", first.stagingFile)
	if _, err := os.Lstat(RunSocketDir(first.conv)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the released engagement's socket directory is still there (lstat err %v)", err)
	}

	// The next claim of the same task, at another index.
	const nextIdx = 7
	if err := (hostOps{}).ChownRunTree(ctx, first.tree, "", JailUID(nextIdx)); err != nil {
		t.Fatalf("ChownRunTree to the next engagement: %v", err)
	}
	if err := (hostOps{}).GrantRunStaging(ctx, first.staging, nextIdx); err != nil {
		t.Fatalf("GrantRunStaging to the next engagement: %v", err)
	}
	next := jailIdentity(nextIdx)
	p.mustAllow(next, "read", first.treeFile)
	p.mustAllow(next, "create", first.tree)
	p.mustAllow(next, "read", first.stagingFile)
	p.mustDeny(released, "read", first.treeFile)
}
