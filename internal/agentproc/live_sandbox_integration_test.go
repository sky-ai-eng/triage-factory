package agentproc

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/egressproxy"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// Live interactive-sandbox integration tests: they drive RunInteractive
// through the real gVisor sandbox (multi mode + Linux + runsc), exercising the
// same streaming-input live path local runs use — message, interrupt,
// permission round-trip, graceful close — over the bidirectional stdio channel
// TFAC-322 validated, and asserting teardown leaves no netns/veth behind.
//
// Heavily gated and SKIP-by-default (mirrors internal/sandbox's
// integration_linux_test.go prereq pattern + TestSDK_LiveSmoke's TF_TEST_SDK_LIVE
// switch). All of these must hold or the test skips:
//
//	TF_TEST_SDK_LIVE=1     opt in to live, billable SDK runs
//	runsc on PATH          gVisor runtime
//	euid 0                 sandbox needs CAP_NET_ADMIN / CAP_SYS_ADMIN
//	ANTHROPIC_API_KEY set   a real key, injected via the per-run LLM proxy
//	                       (Property B: it reaches the host proxy, never the box)
//
// CI lacks runsc + a real key, so these skip there; they're the staging /
// sandbox-CI verification gate the ticket calls for. Run with, e.g.:
//
//	sudo TF_TEST_SDK_LIVE=1 ANTHROPIC_API_KEY=sk-ant-… \
//	  go test ./internal/agentproc -run TestIntegration_InteractiveSandbox -v

// staticSecrets is a fixed SecretsReader returning the configured key for any
// org — enough to drive resolveCredentials down the Anthropic-direct path so
// the sandbox's per-run LLM proxy has a real upstream credential to inject.
type staticSecrets map[string]string

func (s staticSecrets) Get(_ context.Context, _, key string) (string, error) {
	return s[key], nil
}

// requireInteractiveSandbox enforces the prereqs above (skipping otherwise),
// forces multi mode for the duration of the test so shouldSandbox() routes
// RunInteractive through gVisor, and returns the per-org secret store + orgID
// the run resolves credentials against.
func requireInteractiveSandbox(t *testing.T) (SecretsReader, string) {
	t.Helper()
	if os.Getenv("TF_TEST_SDK_LIVE") != "1" {
		t.Skip("set TF_TEST_SDK_LIVE=1 to run the live interactive-sandbox integration test")
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("runsc not installed; skipping interactive-sandbox integration test")
	}
	if os.Geteuid() != 0 {
		t.Skip("gVisor sandbox needs root (CAP_NET_ADMIN / CAP_SYS_ADMIN); skipping")
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("set ANTHROPIC_API_KEY to a real key (injected via the per-run LLM proxy, never into the sandbox env)")
	}
	// shouldSandbox() == ModeMulti && linux. Restored after the test.
	runmode.SetForTest(t, runmode.ModeMulti)
	secrets := staticSecrets{secretAnthropicAPIKey: apiKey}
	// A non-sentinel org so the multi-mode credential path is taken in full.
	orgID := "00000000-0000-0000-0000-0000000000aa"
	return secrets, orgID
}

// prebuiltRunHarness stands up what the delegate sidecar bring-up
// provides in production — the per-run network plus the LLM/git/egress
// proxies over the resolved org credential — so these tests exercise the
// prebuilt-network shape that is now the ONLY sandbox launch path (the
// in-process bring-up inside newSandboxCommand no longer exists). The proxies
// run in this test process rather than a credential sidecar, which preserves
// the property the jail-side assertions care about: the sandbox env carries
// only proxy URLs + per-run placeholders, never the real key.
//
// Returns the network, the sandbox proxy env, and a close func the test MUST
// call before asserting on leaked netns — the network is caller-owned (the
// production contract: the delegate closes it after Run returns), so the
// LiveRun's own teardown deliberately leaves it up. close is idempotent and
// also registered as a cleanup backstop.
func prebuiltRunHarness(t *testing.T, secrets SecretsReader, orgID, conversationID string) (*sandbox.RunNetwork, []string, func()) {
	t.Helper()
	ctx := context.Background()
	creds, err := resolveCredentials(ctx, secrets, orgID, "", nil)
	if err != nil {
		t.Fatalf("resolve credentials: %v", err)
	}
	net, err := sandbox.SetupRunNetwork(ctx, conversationID)
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	proxies, env, err := startProxiesForSandbox(ctx, net.HostIP, creds, true, nil, egressproxy.GitHubHosts{}, nil, nil)
	if err != nil {
		_ = net.Close()
		t.Fatalf("startProxiesForSandbox: %v", err)
	}
	var once sync.Once
	closeHarness := func() {
		once.Do(func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := proxies.Shutdown(sctx); err != nil {
				t.Logf("proxy shutdown: %v", err)
			}
			_ = net.Close()
		})
	}
	t.Cleanup(closeHarness)
	return net, env, closeHarness
}

// tfNetnsEntries snapshots the set of tf-<frag>-<idx> network namespaces — the
// per-run netns the sandbox creates. The teardown assertions diff against a
// baseline so a parallel/leftover run on the box doesn't false-positive.
func tfNetnsEntries(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	entries, err := os.ReadDir("/var/run/netns")
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatalf("read /var/run/netns: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tf-") {
			out[e.Name()] = true
		}
	}
	return out
}

// assertNoNewNetns fails if any tf- netns appeared (and survived) relative to
// the baseline. cleanup runs synchronously inside readLoop before Done closes,
// so by the time the caller has observed <-lr.Done() the netns is already gone;
// the short retry only absorbs any lag in the kernel's async link teardown.
func assertNoNewNetns(t *testing.T, before map[string]bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var leaked []string
		for name := range tfNetnsEntries(t) {
			if !before[name] {
				leaked = append(leaked, name)
			}
		}
		if len(leaked) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("teardown leaked netns: %v (no tf- veth/netns should survive Close)", leaked)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestIntegration_InteractiveSandbox_SteerAndTeardown is the headline probe:
// a sandboxed run streams a first turn, takes a second steering message on the
// same warm process (proving LiveRun.Send over the gVisor channel and survival
// of the inter-turn idle), then closes cleanly with no netns/veth left behind.
// That an assistant turn surfaces BEFORE Close is also the per-line-flush
// regression (gotcha 2): a coalesced channel would only flush at process exit.
func TestIntegration_InteractiveSandbox_SteerAndTeardown(t *testing.T) {
	secrets, orgID := requireInteractiveSandbox(t)
	before := tfNetnsEntries(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	net, proxyEnv, closeHarness := prebuiltRunHarness(t, secrets, orgID, "itest-live-sandbox-steer")
	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:              t.TempDir(),
		Message:          "Reply with exactly the word PING and nothing else.",
		Model:            "haiku",
		TraceID:          "itest-live-sandbox-steer",
		OrgID:            orgID,
		PrebuiltNetwork:  net,
		PrebuiltProxyEnv: proxyEnv,
	}, sink, denyAllPermissions)
	if err != nil {
		t.Fatalf("RunInteractive (sandbox): %v", err)
	}
	defer func() { _ = lr.Close() }()

	// Liveness + per-line-flush: an assistant turn arrives over the channel
	// while the process is still live (not coalesced into an end-of-turn blob
	// delivered only at Close).
	first := sink.waitAssistant(t, 120*time.Second)
	t.Logf("first turn: %q", first.Content)

	sid := lr.SessionID()
	if sid == "" {
		t.Fatal("expected a session id after the first turn")
	}

	// Steer the same warm sandboxed process with a second message.
	if err := lr.Send(ctx, "Now reply with exactly the word PONG and nothing else."); err != nil {
		t.Fatalf("Send (steer): %v", err)
	}
	second := sink.waitAssistant(t, 120*time.Second)
	t.Logf("second turn: %q", second.Content)
	if lr.SessionID() != sid {
		t.Errorf("session id changed across turns: %q -> %q (no warm-process continuity)", sid, lr.SessionID())
	}

	if err := lr.Close(); err != nil {
		t.Logf("close returned (non-fatal): %v", err)
	}
	<-lr.Done()

	// Teardown ownership matches production: LiveRun ran its cleanup after
	// cmd.Wait, and the caller (here the harness, in production delegate)
	// closes the network it owns — after which no tf-netns/veth survives.
	closeHarness()
	assertNoNewNetns(t, before)
}

// TestIntegration_InteractiveSandbox_Interrupt asserts interrupt() lands over
// the sandbox channel (mirrors TestSDK_LiveSmoke_InteractiveInterrupt): the
// in-flight turn ends error_during_execution, and teardown is clean.
func TestIntegration_InteractiveSandbox_Interrupt(t *testing.T) {
	secrets, orgID := requireInteractiveSandbox(t)
	before := tfNetnsEntries(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	net, proxyEnv, closeHarness := prebuiltRunHarness(t, secrets, orgID, "itest-live-sandbox-interrupt")
	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:              t.TempDir(),
		Message:          "Write a very long, detailed essay of at least 3000 words about the full history of computing. Take your time and be thorough.",
		Model:            "haiku",
		TraceID:          "itest-live-sandbox-interrupt",
		OrgID:            orgID,
		PrebuiltNetwork:  net,
		PrebuiltProxyEnv: proxyEnv,
	}, sink, denyAllPermissions)
	if err != nil {
		t.Fatalf("RunInteractive (sandbox): %v", err)
	}
	defer func() { _ = lr.Close() }()

	// Interrupt mid-flight, which means we can't wait on an assistant turn:
	// the assistant message only flushes at stop_reason (the very turn-end
	// we're pre-empting). So gate on the session id (emitted early in the
	// stream) and give the turn a few seconds to get underway before the
	// interrupt — same timing assumption as the non-sandbox smoke test.
	waitSession(t, lr, 120*time.Second)
	time.Sleep(3 * time.Second)

	if err := lr.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if err := lr.Close(); err != nil {
		t.Logf("close returned (non-fatal): %v", err)
	}
	<-lr.Done()

	res := lr.Result()
	if res == nil {
		t.Fatal("expected a Result after interrupt")
	}
	if res.Subtype != "error_during_execution" {
		t.Errorf("interrupt result subtype = %q, want error_during_execution", res.Subtype)
	}
	closeHarness()
	assertNoNewNetns(t, before)
}

// TestIntegration_InteractiveSandbox_Permission asserts the canUseTool bridge
// works over the sandbox channel (mirrors TestSDK_LiveSmoke_InteractivePermission):
// an off-allowlist Write surfaces a permission_request and a deny is honored —
// the file is never written to the worktree (the sandbox's bind-mounted /work).
func TestIntegration_InteractiveSandbox_Permission(t *testing.T) {
	secrets, orgID := requireInteractiveSandbox(t)
	before := tfNetnsEntries(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	target := filepath.Join(cwd, "secret.txt") // agent sees /work/secret.txt

	permCh := make(chan PermissionRequest, 8)
	handler := func(req PermissionRequest) PermissionDecision {
		select {
		case permCh <- req:
		default:
		}
		return PermissionDecision{Behavior: "deny", Message: "denied by test"}
	}

	net, proxyEnv, closeHarness := prebuiltRunHarness(t, secrets, orgID, "itest-live-sandbox-perm")
	sink := newLiveSink()
	lr, err := RunInteractive(ctx, RunOptions{
		Cwd:              cwd,
		Message:          "Use the Write tool to create a file named secret.txt containing the text 'hello'. Do it now without asking.",
		Model:            "sonnet-4-6",
		TraceID:          "itest-live-sandbox-perm",
		OrgID:            orgID,
		PrebuiltNetwork:  net,
		PrebuiltProxyEnv: proxyEnv,
	}, sink, handler)
	if err != nil {
		t.Fatalf("RunInteractive (sandbox): %v", err)
	}
	defer func() { _ = lr.Close() }()

	select {
	case req := <-permCh:
		t.Logf("permission requested over the sandbox channel: tool=%s input=%v", req.ToolName, req.Input)
	case <-time.After(150 * time.Second):
		t.Fatal("expected a permission_request for the Write tool over the sandbox channel")
	}

	_ = lr.Close()
	<-lr.Done()

	if _, err := os.Stat(target); err == nil {
		t.Errorf("file %s was written despite a denied permission", target)
	}
	closeHarness()
	assertNoNewNetns(t, before)
}
