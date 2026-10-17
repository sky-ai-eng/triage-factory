// Package celltest is the conformance suite every cell backend must pass
// (docs/for-agents/specs/sandbox-backends/ §5, §9). It drives a backend
// through internal/cell exactly as a native engagement does — provision the
// cell, bring its sidecar up with a sealed bundle, launch the tool host into a
// handed-off run tree, connect — and asserts each invariant from inside the
// jail, over the tool host's own frame protocol.
//
// The bundle carries canary credentials minted per cell. Nothing a jail can
// read may contain one; that is Property B, checked the only way that means
// anything, by looking.
//
// A backend wires the suite from its own package's integration tests, after
// installing whatever its cells need (gVisor: a running cap-broker):
//
//	func TestConformance(t *testing.T) { celltest.Run(t, celltest.Harness{Backend: New()}) }
package celltest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentloop"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/credseal"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// Harness is a backend under test.
type Harness struct {
	// Backend is the backend whose cells the suite builds.
	Backend cell.Backend
}

// Bounds for the tool host to connect and for one tool call. Generous: the
// suite asserts properties, not latency, and a cold host pays rootfs and
// runtime warm-up on its first cell.
const (
	bringUpTimeout = 2 * time.Minute
	toolTimeout    = time.Minute
	probeTimeout   = 2 // seconds, per in-jail connect attempt
)

// Run runs every conformance case against h.Backend. Cases run one after
// another and each tears down every cell it built before the next starts.
func Run(t *testing.T, h Harness) {
	t.Helper()
	if h.Backend == nil {
		t.Fatal("celltest: Harness.Backend is nil")
	}
	t.Run("PropertyB", func(t *testing.T) { testPropertyB(t, h) })
	t.Run("RunTreeAtWork", func(t *testing.T) { testRunTreeAtWork(t, h) })
	t.Run("JailIsNotRoot", func(t *testing.T) { testJailIsNotRoot(t, h) })
	t.Run("OnlyTheSidecarIsReachable", func(t *testing.T) { testOnlyTheSidecarIsReachable(t, h) })
	t.Run("InternalDenylistUnreachable", func(t *testing.T) { testInternalDenylistUnreachable(t, h) })
	t.Run("CellsAreIsolated", func(t *testing.T) { testCellsAreIsolated(t, h) })
	t.Run("FreezeHoldsTheJailStill", func(t *testing.T) { testFreezeHoldsTheJailStill(t, h) })
	t.Run("StopLeavesNothingRunning", func(t *testing.T) { testStopLeavesNothingRunning(t, h) })
	t.Run("TeardownReleasesTheCell", func(t *testing.T) { testTeardownReleasesTheCell(t, h) })
}

// testPropertyB: no canary credential appears in the jail's environment, its
// main process's argv, or any file under the trees a jail could plausibly be
// handed one in. The positive control is that the placeholders the sidecar
// hands out ARE there, so an empty environment cannot pass by accident.
func testPropertyB(t *testing.T, h Harness) {
	l := start(t, h, newConversationID(t))

	env := l.sh(t, "env; echo; tr '\\0' '\\n' < /proc/1/environ; echo; tr '\\0' ' ' < /proc/1/cmdline")
	for _, want := range []string{"HTTPS_PROXY=", "GH_HOST=", "GH_ENTERPRISE_TOKEN="} {
		if !strings.Contains(env, want) {
			t.Errorf("jail env has no %s — the sidecar's coordinates never reached the jail, so this check proves nothing:\n%s", want, env)
		}
	}
	for _, canary := range l.canaries {
		if strings.Contains(env, canary) {
			t.Errorf("PROPERTY B VIOLATED: a real credential is in the jail's environment or argv:\n%s", env)
		}
	}

	for _, canary := range l.canaries {
		out := l.sh(t, fmt.Sprintf("grep -rlF %s /work /run /opt /etc /tmp /root /home /usr/local/bin 2>/dev/null; echo grep-done", shellQuote(canary)))
		if lines := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(out), "grep-done")); lines != "" {
			t.Errorf("PROPERTY B VIOLATED: a real credential is in a file the jail can read:\n%s", lines)
		}
	}
}

// testRunTreeAtWork: the run tree the caller handed off is the jail's /work.
// What the caller wrote before launch is visible inside, and what the jail
// writes is in the caller's tree once the jail has stopped.
func testRunTreeAtWork(t *testing.T, h Harness) {
	l := start(t, h, newConversationID(t))

	if got := strings.TrimSpace(l.sh(t, "cat /work/"+seedFileName)); got != l.seed {
		t.Fatalf("/work/%s in the jail = %q, want the caller's %q", seedFileName, got, l.seed)
	}
	marker := "from-the-jail-" + randomHex(t, 8)
	l.sh(t, fmt.Sprintf("printf %%s %s > /work/jail-wrote.txt; sync; echo ok", shellQuote(marker)))
	if err := l.jail.Stop(); err != nil {
		t.Fatalf("stop the jail before reading its tree: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(l.runTree, "jail-wrote.txt"))
	if err != nil {
		t.Fatalf("the jail's write is not in the caller's run tree: %v", err)
	}
	if string(got) != marker {
		t.Fatalf("run tree file = %q, want %q", got, marker)
	}
}

// testJailIsNotRoot: the agent does not run as uid 0. A backend that declares
// the root capability is exempt; none does yet, so the case is unconditional.
func testJailIsNotRoot(t *testing.T, h Harness) {
	l := start(t, h, newConversationID(t))
	out := strings.TrimSpace(l.sh(t, "id -u"))
	uid, err := strconv.Atoi(out)
	if err != nil {
		t.Fatalf("id -u = %q, not a uid", out)
	}
	if uid == 0 {
		t.Fatal("the jail runs as root")
	}
}

// testOnlyTheSidecarIsReachable: the jail reaches its own sidecar's egress
// proxy, and nothing else on the host that serves it — not even a listener
// that happens to be bound on every interface, which is what an executor's
// metrics or debug endpoint would be.
func testOnlyTheSidecarIsReachable(t *testing.T, h Harness) {
	l := start(t, h, newConversationID(t))

	host, port := l.egressProxy(t)
	if !l.reaches(t, host, port) {
		t.Fatalf("the jail cannot reach its own sidecar's egress proxy at %s:%d — every negative below would pass vacuously", host, port)
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen on every interface: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go acceptAndClose(ln)
	other := ln.Addr().(*net.TCPAddr).Port
	if l.reaches(t, l.cell.ProxyHost(), other) {
		t.Errorf("the jail reached a non-sidecar listener at its proxy host %s:%d: anything the executor binds on all interfaces is open to the agent", l.cell.ProxyHost(), other)
	}
}

// testInternalDenylistUnreachable: cloud metadata, the public internet
// directly, and the executor's own addresses are all closed to the jail.
func testInternalDenylistUnreachable(t *testing.T, h Harness) {
	l := start(t, h, newConversationID(t))

	host, port := l.egressProxy(t)
	if !l.reaches(t, host, port) {
		t.Fatalf("the jail cannot reach its own sidecar at %s:%d — the negatives below would pass vacuously", host, port)
	}

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen on every interface: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go acceptAndClose(ln)
	listenerPort := ln.Addr().(*net.TCPAddr).Port

	targets := []struct {
		name string
		host string
		port int
	}{
		{"cloud metadata", "169.254.169.254", 80},
		{"public internet, directly", "1.1.1.1", 443},
		{"public DNS, directly", "8.8.8.8", 53},
	}
	for _, addr := range executorAddresses(t, l.cell.ProxyHost()) {
		targets = append(targets, struct {
			name string
			host string
			port int
		}{"executor address " + addr, addr, listenerPort})
	}
	for _, tgt := range targets {
		if l.reaches(t, tgt.host, tgt.port) {
			t.Errorf("the jail reached %s at %s:%d", tgt.name, tgt.host, tgt.port)
		}
	}
}

// testCellsAreIsolated: two live cells, and neither jail reaches the other's
// sidecar or sees the other's run tree.
func testCellsAreIsolated(t *testing.T, h Harness) {
	a := start(t, h, newConversationID(t))
	b := start(t, h, newConversationID(t))

	for _, pair := range []struct {
		name      string
		from, rel *liveCell
	}{{"A→B", a, b}, {"B→A", b, a}} {
		ownHost, ownPort := pair.from.egressProxy(t)
		if !pair.from.reaches(t, ownHost, ownPort) {
			t.Fatalf("%s: the jail cannot reach its own sidecar — the cross-cell negative would pass vacuously", pair.name)
		}
		otherHost, otherPort := pair.rel.egressProxy(t)
		if pair.from.reaches(t, otherHost, otherPort) {
			t.Errorf("%s: the jail reached the other cell's sidecar at %s:%d", pair.name, otherHost, otherPort)
		}
		if got := strings.TrimSpace(pair.from.sh(t, "cat /work/"+seedFileName)); got != pair.from.seed {
			t.Errorf("%s: /work/%s = %q, want this cell's own %q", pair.name, seedFileName, got, pair.from.seed)
		}
		if strings.Contains(pair.from.sh(t, "grep -rlF "+shellQuote(pair.rel.seed)+" /work /tmp 2>/dev/null; echo grep-done"), "/") {
			t.Errorf("%s: the other cell's run tree is visible to this jail", pair.name)
		}
	}
}

// testFreezeHoldsTheJailStill: a tool call issued against a frozen jail makes
// no progress until the jail is thawed, and then completes.
func testFreezeHoldsTheJailStill(t *testing.T, h Harness) {
	l := start(t, h, newConversationID(t))
	ctx := context.Background()

	if err := l.jail.Freeze(ctx); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	done := make(chan agentloop.ToolOutcome, 1)
	errs := make(chan error, 1)
	go func() {
		out, err := l.tools.Call("bash", map[string]any{"command": "echo thawed", "timeout": 60})
		if err != nil {
			errs <- err
			return
		}
		done <- out
	}()
	select {
	case out := <-done:
		_ = l.jail.Thaw(ctx)
		t.Fatalf("a tool call completed against a frozen jail: %+v", out)
	case err := <-errs:
		_ = l.jail.Thaw(ctx)
		t.Fatalf("a tool call against a frozen jail failed instead of waiting: %v", err)
	case <-time.After(2 * time.Second):
	}
	if err := l.jail.Thaw(ctx); err != nil {
		t.Fatalf("thaw: %v", err)
	}
	select {
	case out := <-done:
		if !strings.Contains(out.Content, "thawed") {
			t.Fatalf("tool call after thaw = %+v, want its output", out)
		}
	case err := <-errs:
		t.Fatalf("tool call after thaw: %v", err)
	case <-time.After(toolTimeout):
		t.Fatal("the tool call did not complete after the jail was thawed")
	}
}

// testStopLeavesNothingRunning: Stop returns with the jail's processes gone,
// the tool host no longer answers, and a second Stop is harmless.
func testStopLeavesNothingRunning(t *testing.T, h Harness) {
	l := start(t, h, newConversationID(t))

	if err := l.jail.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !l.jail.Exited() {
		t.Fatal("Stop returned but the jail reports processes still running")
	}
	if out, err := l.tools.Call("bash", map[string]any{"command": "echo alive", "timeout": 10}); err == nil && out.Protocol == nil {
		t.Fatalf("the tool host answered after Stop: %+v", out)
	}
	if err := l.jail.Stop(); err != nil {
		t.Fatalf("a second Stop failed: %v", err)
	}
}

// testTeardownReleasesTheCell: a cell closed in contract order leaves nothing
// that stops the same conversation's next engagement from provisioning and
// launching, and the boot-time reap still runs cleanly afterwards.
func testTeardownReleasesTheCell(t *testing.T, h Harness) {
	id := newConversationID(t)

	first := start(t, h, id)
	first.sh(t, "echo first")
	first.teardown()

	second := start(t, h, id)
	if got := strings.TrimSpace(second.sh(t, "echo second")); got != "second" {
		t.Fatalf("the second engagement's tool host answered %q", got)
	}
	second.teardown()

	if err := h.Backend.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("reap after a clean teardown: %v", err)
	}
}

// seedFileName is the file start writes into every run tree before launch.
const seedFileName = "celltest-seed.txt"

// liveCell is one provisioned cell with its sidecar up and its tool host
// connected, plus what the cases assert against.
type liveCell struct {
	conversationID string
	runTree        string
	seed           string
	canaries       []string

	cell  cell.Cell
	conn  *sidecarproto.Conn
	res   *sidecarproto.StartProxiesResult
	jail  cell.Jail
	tools agentloop.ToolHost

	cancel   context.CancelFunc
	tornDown bool
}

// start builds a live cell for conversationID and registers its teardown.
// The cell runs under its own engagement context, cancelled last in teardown:
// a jail lives no longer than the context it was launched with.
func start(t *testing.T, h Harness, conversationID string) *liveCell {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	l := &liveCell{
		cancel:         cancel,
		conversationID: conversationID,
		seed:           "seed-" + randomHex(t, 12),
		canaries: []string{
			"sk-ant-celltest-" + randomHex(t, 16),
			"ghp_celltest" + randomHex(t, 16),
			"ghs_celltest" + randomHex(t, 16),
			"ghs_celltestrepo" + randomHex(t, 16),
		},
	}
	t.Cleanup(l.teardown)

	runTree, err := worktree.MakeRunRoot(conversationID)
	if err != nil {
		t.Fatalf("make run root: %v", err)
	}
	l.runTree = runTree
	if err := os.WriteFile(filepath.Join(runTree, seedFileName), []byte(l.seed+"\n"), 0o644); err != nil {
		t.Fatalf("seed run tree: %v", err)
	}

	c, err := h.Backend.Provision(ctx, cell.ProvisionSpec{ConversationID: conversationID})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	l.cell = c

	res, conn, err := agentproc.BringUpRunSidecar(ctx, c.Sidecar(), l.provision, agentproc.SidecarBringUpParams{
		HostVethIP:       c.ProxyHost(),
		Git:              &agentproc.GitProxyConfig{},
		Relay:            refusingRelay{},
		GitHubAPIEnabled: true,
		GHChannelEnabled: true,
		AgentHost: &sidecarproto.AgentHostInfo{
			OrgID:          "celltest",
			UserID:         "celltest",
			TeamID:         "celltest",
			ConversationID: conversationID,
		},
	})
	if err != nil {
		t.Fatalf("bring up sidecar: %v", err)
	}
	l.res, l.conn = res, conn

	var gh *cell.GHChannel
	if res.GHChannelHost != "" {
		gh = &cell.GHChannel{Host: res.GHChannelHost, Token: res.GHChannelToken, CertSourcePath: agenthost.CertPathFor(conversationID)}
	}
	jail, err := c.Launch(ctx, cell.LaunchSpec{
		WorkspaceKey:    conversationID,
		RunTree:         runTree,
		ProxyEnv:        res.Env,
		AgentHostSocket: agenthost.SocketPathFor(conversationID),
		GHChannel:       gh,
	})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	l.jail = jail

	toolConn, err := jail.Connect(ctx, bringUpTimeout)
	if err != nil {
		t.Fatalf("connect to the tool host: %v", err)
	}
	l.tools = agentloop.NewToolHost(toolConn, toolTimeout)
	return l
}

// teardown closes the cell in contract order: tool host connection, jail
// (stopped, then closed), sidecar protocol, cell, then the run tree.
// Idempotent.
func (l *liveCell) teardown() {
	if l.tornDown {
		return
	}
	l.tornDown = true
	if l.tools != nil {
		_ = l.tools.Close()
	}
	if l.jail != nil {
		_ = l.jail.Stop()
		_ = l.jail.Close()
	}
	if l.conn != nil {
		_ = l.conn.Close()
	}
	if l.cell != nil {
		_ = l.cell.Close()
	}
	if l.runTree != "" {
		worktree.RemoveRunRoot(l.conversationID)
	}
	l.cancel()
}

// provision is the brain's side of the credential handshake, reduced to what
// the sidecar needs: a bundle of this cell's canaries sealed to the key the
// sidecar announced.
func (l *liveCell) provision(_ context.Context, pubKeyB64 string) ([]byte, int64, error) {
	raw, err := base64.StdEncoding.DecodeString(pubKeyB64)
	if err != nil || len(raw) != 32 {
		return nil, 0, fmt.Errorf("celltest: sidecar announced a malformed key (%d bytes, %v)", len(raw), err)
	}
	var pub [32]byte
	copy(pub[:], raw)
	const bootEpoch = 1
	bundle := credbundle.Bundle{
		BootEpoch: bootEpoch,
		LLM:       map[string]string{"ANTHROPIC_API_KEY": l.canaries[0]},
		GitHub: &credbundle.GitHubCreds{
			Mode:     credbundle.GitHubModePAT,
			PAT:      l.canaries[1],
			BaseURL:  "https://github.com",
			CLIToken: &credbundle.RepoToken{Token: l.canaries[2], ExpiresAt: time.Now().Add(time.Hour)},
			RepoTokens: map[string]credbundle.RepoToken{
				"celltest/repo": {Token: l.canaries[3], ExpiresAt: time.Now().Add(time.Hour)},
			},
		},
	}
	plaintext, err := bundle.Marshal()
	if err != nil {
		return nil, 0, err
	}
	sealed, err := credseal.Seal(pub, plaintext)
	if err != nil {
		return nil, 0, err
	}
	return sealed, bootEpoch, nil
}

// sh runs command in the jail through the tool host's bash tool and returns
// its output. A command that fails is returned, not fatal: probes encode their
// own verdicts in what they print.
func (l *liveCell) sh(t *testing.T, command string) string {
	t.Helper()
	out, err := l.tools.Call("bash", map[string]any{"command": command, "timeout": int(toolTimeout / time.Second)})
	if err != nil {
		t.Fatalf("tool host transport failed running %q: %v", command, err)
	}
	if out.Protocol != nil {
		t.Fatalf("tool host protocol error running %q: %v", command, out.Protocol)
	}
	if out.ToolError != "" {
		return out.ToolError
	}
	return out.Content
}

// reaches reports whether a TCP connect from inside the jail to host:port
// completes.
func (l *liveCell) reaches(t *testing.T, host string, port int) bool {
	t.Helper()
	out := l.sh(t, fmt.Sprintf("if nc -z -w %d %s %d; then echo VERDICT-REACHED; else echo VERDICT-BLOCKED; fi", probeTimeout, host, port))
	switch {
	case strings.Contains(out, "VERDICT-REACHED"):
		return true
	case strings.Contains(out, "VERDICT-BLOCKED"):
		return false
	}
	t.Fatalf("connect probe to %s:%d gave no verdict (is nc in the rootfs?): %s", host, port, out)
	return false
}

// egressProxy is the host and port of the sidecar's egress proxy, as the jail
// was told them.
func (l *liveCell) egressProxy(t *testing.T) (string, int) {
	t.Helper()
	for _, kv := range l.res.Env {
		v, ok := strings.CutPrefix(kv, "HTTPS_PROXY=")
		if !ok {
			continue
		}
		u, err := url.Parse(v)
		if err != nil {
			t.Fatalf("HTTPS_PROXY %q: %v", v, err)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil {
			t.Fatalf("HTTPS_PROXY %q has no port", v)
		}
		return u.Hostname(), port
	}
	t.Fatalf("the sidecar handed the jail no HTTPS_PROXY: %v", l.res.Env)
	return "", 0
}

// executorAddresses lists this host's non-loopback IPv4 addresses other than
// the cell's own proxy host: the addresses an executor's services answer on.
func executorAddresses(t *testing.T, proxyHost string) []string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("list host addresses: %v", err)
	}
	var out []string
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.To4() == nil || ipn.IP.IsLoopback() {
			continue
		}
		if ip := ipn.IP.String(); ip != proxyHost {
			out = append(out, ip)
		}
	}
	return out
}

// refusingRelay is the orchestrator's relay with nothing behind it: every op
// fails closed and every audit record is dropped. No case runs an exec verb.
type refusingRelay struct{}

func (refusingRelay) DispatchCall(context.Context, string, string, json.RawMessage) (json.RawMessage, error) {
	return nil, errors.New("celltest: no relay ops are served")
}

func (refusingRelay) DispatchRecord(context.Context, string, string, string, json.RawMessage) error {
	return nil
}

func acceptAndClose(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = c.Close()
	}
}

// newConversationID is a random UUID-shaped id: the shape per-run resource
// names are validated against.
func newConversationID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random id: %v", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
