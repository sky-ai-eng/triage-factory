//go:build linux

package agentproc

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/cabundle"
	"github.com/sky-ai-eng/triage-factory/internal/egressproxy"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// Live in-jail test of the egress interceptor (TFAC-1117): every toolchain
// the rootfs ships fetches from a real registry through the run's egress
// proxy, inside a real gVisor jail, trusting the interceptor through the
// variables caBundleEnv sets and nothing else. A toolchain added to the
// rootfs that reads none of them fails here, which is what fixes the list.
// It also checks the write path closed: `npm publish` from the jail is
// refused by name.
//
// SKIP-by-default, like the gh-injector live test. All must hold:
//
//	TF_TEST_EGRESS_INTERCEPT_LIVE=1  opt in (it reaches registry.npmjs.org and PyPI)
//	runsc on PATH                    gVisor runtime
//	euid 0                           the sandbox needs CAP_NET_ADMIN / CAP_SYS_ADMIN
//
// The rootfs is built on first use, which needs the Alpine package mirror.
//
//	sudo TF_TEST_EGRESS_INTERCEPT_LIVE=1 \
//	  go test ./internal/agentproc -run TestIntegration_EgressIntercept -v
func requireEgressInterceptLive(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_TEST_EGRESS_INTERCEPT_LIVE") != "1" {
		t.Skip("set TF_TEST_EGRESS_INTERCEPT_LIVE=1 to run the live egress interception test")
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("runsc not installed; skipping")
	}
	if os.Geteuid() != 0 {
		t.Skip("gVisor sandbox needs root (CAP_NET_ADMIN / CAP_SYS_ADMIN); skipping")
	}
}

// interceptToolchainScript runs each toolchain's fetch and prints OK or FAIL
// per step, with the step's output after a FAIL. The packages are small and
// long-lived: left-pad for npm and pnpm, six for pip.
const interceptToolchainScript = `
set -u
cd /work
step() {
  name=$1; shift
  if "$@" >"/work/$name.log" 2>&1; then echo "OK $name"; else echo "FAIL $name"; cat "/work/$name.log"; fi
}
step curl curl -fsS -o /dev/null https://registry.npmjs.org/left-pad
step npm_install sh -c 'mkdir -p /work/npmproj && cd /work/npmproj && printf "{\"name\":\"probe\",\"version\":\"0.0.0\",\"private\":true}" > package.json && npm install --no-fund left-pad@1.3.0 && grep -q "audited\|vulnerabilit" /work/npm_install.log'
step pnpm_install sh -c 'mkdir -p /work/pnpmproj && cd /work/pnpmproj && printf "{\"name\":\"probe\",\"version\":\"0.0.0\",\"private\":true}" > package.json && pnpm add left-pad@1.3.0'
step pip_venv sh -c 'python3 -m venv /work/venv && /work/venv/bin/pip install --no-cache-dir six==1.16.0'
mkdir -p /work/gofetch
cat > /work/gofetch/main.go <<'EOF'
package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	resp, err := http.Get("https://pypi.org/simple/six/")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Println(resp.Status)
		os.Exit(1)
	}
}
EOF
step go_client sh -c 'cd /work/gofetch && GOTOOLCHAIN=local go run main.go'
mkdir -p /work/pub && cd /work/pub
printf '{"name":"tf-intercept-publish-probe-%s","version":"0.0.1"}' "$RANDOM$RANDOM" > package.json
printf '//registry.npmjs.org/:_authToken=npm_NOTAREALTOKEN\n' > .npmrc
if npm publish >/work/publish.log 2>&1; then
  echo "FAIL publish_refused"; cat /work/publish.log
elif grep -q "read-only" /work/publish.log; then
  echo "OK publish_refused"
else
  echo "FAIL publish_refused"; cat /work/publish.log
fi
`

func TestIntegration_EgressIntercept_ToolchainsInTheJail(t *testing.T) {
	requireEgressInterceptLive(t)
	installInProcessLauncher(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	const conversationID = "itest-egress-intercept"
	net, err := sandbox.SetupRunNetwork(ctx, conversationID)
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	t.Cleanup(func() { _ = net.Close() })

	var mu sync.Mutex
	var denials []egressproxy.DeniedConnect
	proxyEnv, srv, addr, err := startEgressProxyForSandbox(net.HostIP, egressproxy.GitHubHosts{}, func(_ context.Context, d egressproxy.DeniedConnect) {
		mu.Lock()
		denials = append(denials, d)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("start egress proxy: %v", err)
	}
	t.Cleanup(func() {
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	})
	admitInjector(ctx, t, net, addr)

	// The bundle the sidecar writes: system roots plus the engagement's CA,
	// at the path the broker validates the mount against. Inlined rather than
	// agenthost.WriteCABundle, which this package cannot import.
	bundlePath := sandbox.TrustedCABundlePath(conversationID)
	if err := os.MkdirAll(filepath.Dir(bundlePath), 0o700); err != nil {
		t.Fatalf("mkdir bundle dir: %v", err)
	}
	if err := os.WriteFile(bundlePath, cabundle.WithSystemRoots(srv.CACertPEM()), 0o640); err != nil {
		t.Fatalf("write CA bundle: %v", err)
	}
	if err := os.Chown(bundlePath, net.JailUID(), sandbox.JailGID(net.Idx)); err != nil {
		t.Fatalf("chown CA bundle to the jail identity: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Remove(bundlePath)
		_ = os.Remove(filepath.Dir(bundlePath))
	})

	env := []string{"PATH=/opt/tf/bin:/usr/local/bin:/usr/bin:/bin", "HOME=/work"}
	env = append(env, proxyEnv...)
	env = append(env, caBundleEnv()...)

	worktree := t.TempDir()
	if err := os.Chown(worktree, net.JailUID(), sandbox.JailGID(net.Idx)); err != nil {
		t.Skipf("can't chown worktree to UID %d: %v", net.JailUID(), err)
	}
	lr, _, err := sandbox.Wrap(ctx, sandbox.Config{
		ConversationID: conversationID,
		Worktree:       worktree,
		Argv:           []string{"/bin/bash", "-c", interceptToolchainScript},
		Env:            env,
		ExtraMounts:    []sandbox.Mount{{Source: bundlePath, Destination: sandboxCABundle, Options: []string{"ro"}}},
		Network:        net,
	})
	if err != nil {
		t.Fatalf("sandbox.Wrap: %v", err)
	}
	defer func() { _ = lr.Close() }()
	if err := lr.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	out, _ := io.ReadAll(lr.Stdout())
	waitErr := lr.Wait()
	transcript := string(out) + lr.Stderr()
	t.Logf("jail output:\n%s", transcript)
	if waitErr != nil {
		t.Errorf("jail exited with %v", waitErr)
	}

	for _, step := range []string{"curl", "npm_install", "pnpm_install", "pip_venv", "go_client", "publish_refused"} {
		if !strings.Contains(transcript, "OK "+step+"\n") {
			t.Errorf("step %s did not pass in the jail", step)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	var sawPublish bool
	for _, d := range denials {
		t.Logf("denial: %s %s", d.Target, d.Reason)
		if strings.HasPrefix(d.Reason, "PUT ") {
			sawPublish = true
		}
		// A client that does not trust the CA shows up as a failed handshake;
		// one here means a toolchain is missing its trust variable.
		if strings.Contains(d.Reason, "TLS handshake with the sandbox client failed") {
			t.Errorf("a client in the jail refused the interceptor's certificate: %s", d.Reason)
		}
	}
	if !sawPublish {
		t.Error("no denial recorded for npm publish's PUT")
	}
}
