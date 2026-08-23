package runsidecar

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/sidecarproto"
)

// TestRuntime_SharedOriginHostDropsThePort pins the value GH_HOST is given. gh
// resolves a git remote's host with url.Hostname() — port stripped — and then
// compares it to GH_HOST verbatim, so a ported GH_HOST can never match a remote
// and no repo-contextual gh command resolves from a worktree. The shared origin
// therefore names itself bare.
func TestRuntime_SharedOriginHostDropsThePort(t *testing.T) {
	rt, err := newCredRuntime()
	if err != nil {
		t.Fatalf("newCredRuntime: %v", err)
	}
	if got := rt.sharedOriginHost(); got != "" {
		t.Errorf("sharedOriginHost with no listener = %q, want empty", got)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	rt.setSharedOriginListener(ln)

	got := rt.sharedOriginHost()
	if got != "127.0.0.1" {
		t.Errorf("sharedOriginHost = %q, want the bare host", got)
	}
	if strings.Contains(got, ":") {
		t.Errorf("sharedOriginHost = %q carries a port; gh's remote matching strips ports and would never match it", got)
	}
}

// TestRuntime_StartProxiesRoutesGitAtTheSharedOrigin is the end of the wiring
// this ticket adds: when the broker handed down a shared-origin listener and the
// run wants a gh channel, the sandbox's git config rewrites the upstream onto
// https://<GH_HOST>/ — the same host gh is pointed at — and names the injector's
// trust file, which git needs in its own config because it does not read the
// process-global SSL_CERT_FILE gh does.
func TestRuntime_StartProxiesRoutesGitAtTheSharedOrigin(t *testing.T) {
	orch, rt := dialRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	rt.setSharedOriginListener(ln)

	bundle := &credbundle.Bundle{
		LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
		GitHub: &credbundle.GitHubCreds{
			Mode:       "app",
			BaseURL:    "https://github.com",
			CLIToken:   &credbundle.RepoToken{Token: "ghs_REALCLITOKEN"},
			RepoTokens: map[string]credbundle.RepoToken{"acme/widgets": {Token: "ghs_REALINSTALLTOKEN"}},
		},
	}
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle,
		sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
		t.Fatalf("relay bundle: %v", err)
	}

	stubPrivilegedSideEffects(t)

	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{
		HostVethIP:       "127.0.0.1",
		GitEnabled:       true,
		GHChannelEnabled: true,
		AgentHost:        &sidecarproto.AgentHostInfo{OrgID: "org", ConversationID: "run-shared-origin"},
	}, &res); err != nil {
		t.Fatalf("start proxies: %v", err)
	}

	// The injector took the handed-down listener, so GH_HOST is the bare host —
	// the same one git was just routed at. That agreement is the ticket.
	wantHost, _, _ := net.SplitHostPort(ln.Addr().String())
	if res.GHChannelHost != wantHost {
		t.Errorf("GHChannelHost = %q, want the shared origin's bare host %q", res.GHChannelHost, wantHost)
	}

	cfg := gitConfigFromEnv(t, res.Env)
	host, _, _ := net.SplitHostPort(ln.Addr().String())
	base := "https://" + host + "/"

	if got := cfg["url."+base+".insteadOf"]; got != "https://github.com/" {
		t.Errorf("insteadOf onto the shared origin = %q, want https://github.com/ (git config: %v)", got, cfg)
	}
	if got := cfg["http."+base+".sslCAInfo"]; got != agentproc.SandboxGHInjectorCertPath {
		t.Errorf("sslCAInfo = %q, want the mounted injector trust file %q", got, agentproc.SandboxGHInjectorCertPath)
	}
	// The placeholder the git proxy authenticates — never the real token, on
	// either half of the shared origin.
	header := cfg["http."+base+".extraHeader"]
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, "Authorization: Basic "))
	if err != nil {
		t.Fatalf("decode extraHeader %q: %v", header, err)
	}
	_, presented, ok := strings.Cut(string(raw), ":")
	if !ok || presented == "" {
		t.Fatalf("extraHeader credential is not user:token: %q", raw)
	}
	if presented != res.GitProxyToken {
		t.Errorf("extraHeader carries %q, want this run's git proxy placeholder", presented)
	}
	if strings.Contains(string(raw), "ghs_") {
		t.Fatal("a real GitHub token reached the sandbox git config")
	}
}

// TestRuntime_StartProxiesWithoutSharedOriginKeepsProxyRouting pins the
// degradation: with no listener handed down, git routes at the git proxy's own
// address exactly as it did before, and nothing names the shared origin.
func TestRuntime_StartProxiesWithoutSharedOriginKeepsProxyRouting(t *testing.T) {
	orch, rt := dialRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	bundle := &credbundle.Bundle{
		LLM: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-real"},
		GitHub: &credbundle.GitHubCreds{
			Mode:       "app",
			BaseURL:    "https://github.com",
			RepoTokens: map[string]credbundle.RepoToken{"acme/widgets": {Token: "ghs_REALINSTALLTOKEN"}},
		},
	}
	if err := orch.Call(ctx, sidecarproto.KindSealedBundle,
		sidecarproto.SealedBundleBody{Sealed: sealBundleTo(t, rt.keypair.Public, bundle)}, nil); err != nil {
		t.Fatalf("relay bundle: %v", err)
	}

	var res sidecarproto.StartProxiesResult
	if err := orch.Call(ctx, sidecarproto.KindStartProxies, sidecarproto.StartProxiesBody{
		HostVethIP:       "127.0.0.1",
		GitEnabled:       true,
		GHChannelEnabled: true,
	}, &res); err != nil {
		t.Fatalf("start proxies: %v", err)
	}

	cfg := gitConfigFromEnv(t, res.Env)
	if got := cfg["url."+strings.TrimRight(res.GitProxyURL, "/")+"/.insteadOf"]; got != "https://github.com/" {
		t.Errorf("insteadOf onto the git proxy = %q, want https://github.com/ (git config: %v)", got, cfg)
	}
	for k := range cfg {
		if strings.Contains(k, ".sslCAInfo") {
			t.Errorf("git config names a CA (%q) with no shared origin to trust", k)
		}
	}
}

// TestGHChannelWanted pins the one predicate both the injector start and the git
// routing read. They must agree: routing the sandbox's git at a shared origin no
// injector serves would break every git operation in the jail.
func TestGHChannelWanted(t *testing.T) {
	withID := &sidecarproto.AgentHostInfo{ConversationID: "run-1"}
	for _, c := range []struct {
		name string
		req  sidecarproto.StartProxiesBody
		want bool
	}{
		{"enabled with a run identity", sidecarproto.StartProxiesBody{GHChannelEnabled: true, AgentHost: withID}, true},
		{"disabled", sidecarproto.StartProxiesBody{AgentHost: withID}, false},
		{"no agent host", sidecarproto.StartProxiesBody{GHChannelEnabled: true}, false},
		{"agent host without a run id", sidecarproto.StartProxiesBody{GHChannelEnabled: true, AgentHost: &sidecarproto.AgentHostInfo{}}, false},
	} {
		if got := ghChannelWanted(c.req); got != c.want {
			t.Errorf("%s: ghChannelWanted = %v, want %v", c.name, got, c.want)
		}
	}
}

// stubPrivilegedSideEffects redirects the two things startProxies does under the
// root-only /run/tf socket root — publishing the injector's certificate and
// binding the exec-verb socket — into a temp dir and a no-op.
//
// Both are required together for any test that passes an AgentHost, which a gh
// channel needs for the run identity behind its cert path. Neither is what such a
// test is about, and a CI runner is unprivileged: without these the test does not
// assert something weaker, it fails outright on a bind it was never exercising.
func stubPrivilegedSideEffects(t *testing.T) {
	t.Helper()
	origWrite, origStart := writeInjectorCert, startAgentHostSocket
	dir := t.TempDir()
	writeInjectorCert = func(conversationID string, pem []byte) error {
		return os.WriteFile(filepath.Join(dir, conversationID+".crt"), pem, 0o600)
	}
	startAgentHostSocket = func(*agenthost.Server, string) (*agenthost.HostDaemon, sandbox.Mount, error) {
		return nil, sandbox.Mount{}, nil
	}
	t.Cleanup(func() {
		writeInjectorCert, startAgentHostSocket = origWrite, origStart
	})
}

// gitConfigFromEnv decodes git's indexed env-config form back into a map, so a
// test asserts on settings rather than on positional env strings.
func gitConfigFromEnv(t *testing.T, env []string) map[string]string {
	t.Helper()
	vals := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			vals[k] = v
		}
	}
	out := map[string]string{}
	for i := 0; ; i++ {
		key, ok := vals["GIT_CONFIG_KEY_"+strconv.Itoa(i)]
		if !ok {
			return out
		}
		out[key] = vals["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]
	}
}
