//go:build live

// Live tests in this file hit api.anthropic.com with a real API key.
// They're build-tagged so CI doesn't run them by default and so we
// don't burn API credits on every `go test ./...` run.
//
// To run: `go test -tags live ./internal/proxy/llmproxy/ -v -run TestLive`
// Cost: ~$0.02–0.03 per full run (3 tests × small Haiku call).
// Requires: ANTHROPIC_API_KEY in env (the test process's env, NOT
// the agent subprocess's).

package llmproxy_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/proxy/llmproxy"
)

// liveAPIKey reads the Anthropic API key the live tests use to seed
// the proxy. The proxy injects it into upstream requests; the agent
// subprocess never sees it. Skips the test if the key isn't set so
// CI / developer machines without an API key don't fail.
func liveAPIKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("ANTHROPIC_API_KEY")
	if key == "" {
		t.Skip("ANTHROPIC_API_KEY not set; skipping live test")
	}
	return key
}

// TestLive_ProxyForwardsRealAnthropicRequest is the cheapest live
// test — it doesn't involve Node or the SDK, just a raw HTTP call
// to /v1/messages through the proxy. Validates that the rewritten
// x-api-key header is accepted by the real Anthropic API.
//
// If this fails, the proxy's header rewrite is broken (or Anthropic
// changed their auth) and there's no point running the more expensive
// SDK tests below.
func TestLive_ProxyForwardsRealAnthropicRequest(t *testing.T) {
	apiKey := liveAPIKey(t)

	_, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, apiKey, "https://api.anthropic.com")

	// Smallest possible messages request — one Haiku turn, ~10 tokens.
	body := strings.NewReader(`{
		"model": "claude-haiku-4-5",
		"max_tokens": 16,
		"messages": [{"role": "user", "content": "Reply with the single word: PROXY_OK"}]
	}`)
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	// Caller-supplied x-api-key is deliberately empty — proxy
	// injection is what makes the request authenticate.

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("roundtrip: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Fatalf("upstream status = %d, want 200. body = %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "PROXY_OK") {
		t.Errorf("response missing PROXY_OK token; got: %s", respBody)
	}
}

// TestLive_SDKViaProxy is the load-bearing Phase 1 test. Spawns Node
// running wrapper.mjs with:
//
//   - ANTHROPIC_BASE_URL pointing at the proxy
//   - ANTHROPIC_API_KEY set to a placeholder so the SDK initializes
//     in API-key mode (rather than trying OAuth / subscription auth);
//     the proxy overwrites it with the real key before the request
//     reaches Anthropic.
//   - PATH + HOME (the minimum Node needs to function)
//   - NO inheritance from the test process's env
//
// Verifies:
//
//  1. Agent completes successfully and the response contains the
//     expected token (proves the proxy actually routes traffic).
//  2. Proxy's RequestCount > 0 (proves traffic went through the
//     proxy, not direct to Anthropic via some fallback).
//  3. The subprocess env we constructed contains no real API key
//     (assertion at construction time — the test is partly *about*
//     this allowlist working).
func TestLive_SDKViaProxy(t *testing.T) {
	apiKey := liveAPIKey(t)

	wrapperPath, err := agentproc.EnsureSDK()
	if err != nil {
		t.Fatalf("EnsureSDK: %v", err)
	}

	srv, proxyURL := startProxyWithAddr(t, llmproxy.ProviderAnthropic, apiKey, "https://api.anthropic.com")

	// Build the explicit env allowlist. Node needs PATH (find node)
	// and HOME (npm cache, claude config dir). ANTHROPIC_API_KEY is
	// a placeholder the proxy overwrites; ANTHROPIC_BASE_URL routes
	// the SDK at the proxy.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		t.Fatal("test process has no PATH; can't construct subprocess env")
	}
	subprocessEnv := []string{
		"PATH=" + pathEnv,
		"HOME=" + home,
		"ANTHROPIC_BASE_URL=" + proxyURL,
		"ANTHROPIC_API_KEY=sk-ant-PROXY-PLACEHOLDER",
	}

	// Sanity-check the env we constructed contains no real key.
	// Bug in the env construction would silently make the test
	// meaningless (agent would auth direct via the real key in
	// its own env).
	for _, e := range subprocessEnv {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") && strings.Contains(e, apiKey) {
			t.Fatalf("subprocess env contains the real API key — test setup is wrong; saw %q", e)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", wrapperPath,
		"-p", "Reply with the single word: PROXY_OK",
		"--model", "claude-haiku-4-5",
		"--max-turns", "1")
	cmd.Env = subprocessEnv
	cwd, err := os.MkdirTemp("", "llmproxy-live-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	cmd.Dir = cwd
	defer os.RemoveAll(cwd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("node wrapper.mjs failed: %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "PROXY_OK") {
		t.Errorf("agent output missing PROXY_OK token.\nstdout: %s\nstderr: %s", out, stderr.String())
	}
	if got := srv.RequestCount(); got == 0 {
		t.Errorf("proxy RequestCount = 0; agent traffic must have bypassed the proxy.\nstdout: %s", out)
	}

	t.Logf("proxy observed %d upstream call(s)", srv.RequestCount())
	t.Logf("wrapper.mjs at %s; cwd %s", wrapperPath, cmd.Dir)
}

// TestLive_AgentFailsWithBadProxyURL is the negative companion to
// TestLive_SDKViaProxy. Points ANTHROPIC_BASE_URL at a port nothing
// is listening on. Pins that the agent fails loudly (connection
// error from the SDK) rather than silently falling back to
// api.anthropic.com — which would mean the proxy isn't actually
// gating traffic and the architecture doesn't deliver what we
// claim it does.
//
// Skipped if the wrapper.mjs install fails for any reason — the
// negative result only matters if the positive path also exists.
func TestLive_AgentFailsWithBadProxyURL(t *testing.T) {
	_ = liveAPIKey(t) // Skip-if-unset gate; key not actually used here.

	wrapperPath, err := agentproc.EnsureSDK()
	if err != nil {
		t.Skipf("EnsureSDK: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	pathEnv := os.Getenv("PATH")
	if pathEnv == "" {
		t.Fatal("test process has no PATH; the subprocess wouldn't be able to find node and the test signal would be confounded")
	}
	// Port 1 is reserved and never bound — guaranteed connection
	// refused. Using a deterministic bad address rather than a random
	// port the kernel might briefly bind elsewhere.
	const badProxy = "http://127.0.0.1:1"
	subprocessEnv := []string{
		"PATH=" + pathEnv,
		"HOME=" + home,
		"ANTHROPIC_BASE_URL=" + badProxy,
		"ANTHROPIC_API_KEY=sk-ant-PROXY-PLACEHOLDER",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "node", wrapperPath,
		"-p", "say hi",
		"--model", "claude-haiku-4-5",
		"--max-turns", "1")
	cmd.Env = subprocessEnv
	cwd, err := os.MkdirTemp("", "llmproxy-bad-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	cmd.Dir = cwd
	defer os.RemoveAll(cwd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	combined := stdout.String() + stderr.String()

	// wrapper.mjs exits non-zero on SDK errors, so the *only* valid
	// pass condition is runErr != nil. A nil runErr means the agent
	// completed — which against a closed port means it bypassed the
	// proxy URL entirely, defeating the architecture. The previous
	// version of this check also accepted "exit 0 with 'error' in
	// output", which would have missed that regression.
	if runErr == nil {
		t.Errorf("agent succeeded against a non-existent proxy — base URL is NOT actually gating traffic.\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}
	// Soft check on the failure mode: the error mentions the bad
	// proxy or a connection-shape error. If neither, the SDK might
	// be masking the connection error in a way that's harder to
	// debug — log but don't fail.
	hasConnectionSignal := strings.Contains(combined, "127.0.0.1:1") ||
		strings.Contains(strings.ToLower(combined), "econnrefused") ||
		strings.Contains(strings.ToLower(combined), "connection refused") ||
		strings.Contains(strings.ToLower(combined), "fetch failed")
	if !hasConnectionSignal {
		t.Logf("agent failed but the error doesn't obviously mention the bad proxy address; double-check the SDK isn't masking the connection error\nstdout: %s\nstderr: %s", stdout.String(), stderr.String())
	}

	t.Logf("agent failure (expected): %v", runErr)
	t.Logf("combined out (truncated): %s", truncate(combined, 600))
}

// truncate returns at most n bytes of s, with an ellipsis if cut.
// Local helper to keep log messages bounded.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
