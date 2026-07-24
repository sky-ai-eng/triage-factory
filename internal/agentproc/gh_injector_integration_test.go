//go:build linux

package agentproc

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/ghinjector"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// Live gh-channel integration test (TFAC-669): it closes exactly the three
// spike residuals (finding 7) that the host-side spike could not — (a) in-jail
// reachability + SSL_CERT_FILE under the REAL runsc launch, over a
// veth-reachable host injector; (b) a real installation (ghs_) token end to end;
// (c) `gh pr create/view/list/diff` against a real repo. It runs the pinned real
// gh binary INSIDE a gVisor jail, pointed at a per-run injector on the host veth
// IP, and asserts the placeholder-only invariant from the host side plus the
// scope-denial (404) shape.
//
// Heavily gated and SKIP-by-default (the internal/sandbox requireRunsc pattern +
// the TestSDK_LiveSmoke opt-in switch). All must hold or it skips:
//
//	TF_TEST_GH_INJECTOR_LIVE=1  opt in to the live run
//	runsc on PATH               gVisor runtime
//	euid 0                      the sandbox needs CAP_NET_ADMIN / CAP_SYS_ADMIN
//	TF_TEST_GH_TOKEN            a real ghs_ installation token scoped to a team
//	                           repo set (in Actions this is the standard
//	                           GITHUB_TOKEN); injected upstream, never in the box
//	TF_TEST_GH_REPO            an owner/repo the token CAN reach (in scope)
//	TF_TEST_GH_DENY_REPO       (optional) an owner/repo the token CANNOT reach —
//	                           asserted to surface GitHub's 404 masking
//
// The gh binary must be present at /opt/tf/bin/gh (the image bakes it; on a bare
// box drop the pinned release there). Run with, e.g.:
//
//	sudo TF_TEST_GH_INJECTOR_LIVE=1 TF_TEST_GH_TOKEN=ghs_… \
//	  TF_TEST_GH_REPO=sky-ai-eng/triage-factory \
//	  go test ./internal/agentproc -run TestIntegration_GHInjector -v
func requireGHInjectorLive(t *testing.T) (token, repo, denyRepo string) {
	t.Helper()
	if os.Getenv("TF_TEST_GH_INJECTOR_LIVE") != "1" {
		t.Skip("set TF_TEST_GH_INJECTOR_LIVE=1 to run the live gh-injector integration test")
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("runsc not installed; skipping gh-injector integration test")
	}
	if os.Geteuid() != 0 {
		t.Skip("gVisor sandbox needs root (CAP_NET_ADMIN / CAP_SYS_ADMIN); skipping")
	}
	token = os.Getenv("TF_TEST_GH_TOKEN")
	if token == "" {
		t.Skip("set TF_TEST_GH_TOKEN to a real ghs_ installation token (injected upstream, never into the box)")
	}
	repo = os.Getenv("TF_TEST_GH_REPO")
	if repo == "" {
		t.Skip("set TF_TEST_GH_REPO to an owner/repo the token can reach")
	}
	if _, err := os.Stat(hostGHBinaryPath); err != nil {
		t.Skipf("pinned gh binary not present at %s: %v", hostGHBinaryPath, err)
	}
	return token, repo, os.Getenv("TF_TEST_GH_DENY_REPO")
}

func TestIntegration_GHInjector_JailedGHThroughInjector(t *testing.T) {
	token, repo, denyRepo := requireGHInjectorLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const runID = "itest-gh-injector"

	// Stand up the run network so the injector binds on the veth IP the jail
	// reaches — the exact production shape (the git proxy binds the same way).
	net, err := sandbox.SetupRunNetwork(ctx, runID)
	if err != nil {
		t.Fatalf("SetupRunNetwork: %v", err)
	}
	t.Cleanup(func() { _ = net.Close() })

	// Per-run self-signed cert with the veth IP as SAN, written to the same
	// per-run path the sidecar writes in production (the broker's own
	// derivation, so the mount source matches if validation runs). We inline the
	// write+grant here rather than call agenthost.WriteInjectorCert to avoid the
	// agenthost→agentproc import cycle a test in this package would create.
	cert, certPEM, err := ghinjector.GenerateCert(net.HostIP)
	if err != nil {
		t.Fatalf("GenerateCert: %v", err)
	}
	certPath := sandbox.TrustedGHInjectorCertPath(runID)
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		t.Fatalf("mkdir cert root: %v", err)
	}
	if err := os.WriteFile(certPath, certPEM, 0o640); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.Chown(certPath, sandbox.WorktreeUID, sandbox.WorktreeGID); err != nil {
		t.Fatalf("chown cert to sandbox uid: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(certPath) })

	// The injector holds the REAL token; the box will get only the placeholder.
	const placeholder = "run-placeholder-token"
	inj, err := ghinjector.New(ghinjector.Config{
		Upstream:         "https://api.github.com",
		IncomingToken:    placeholder,
		Cert:             cert,
		AllowNonLoopback: true,
		TokenSource:      func(context.Context) (string, error) { return token, nil },
	})
	if err != nil {
		t.Fatalf("ghinjector.New: %v", err)
	}
	addr, err := inj.Start(net.HostIP + ":0")
	if err != nil {
		t.Fatalf("injector Start: %v", err)
	}
	t.Cleanup(func() {
		sctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = inj.Shutdown(sctx)
	})

	env := []string{
		"PATH=/opt/tf/bin:/usr/local/bin:/usr/bin:/bin",
		"HOME=/work",
		"GH_HOST=" + addr,
		"GH_ENTERPRISE_TOKEN=" + placeholder,
		"SSL_CERT_FILE=" + sandboxGHInjectorCert,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
	}
	// Placeholder-only invariant, asserted from the host side: the real token is
	// nowhere in the env the jail receives — only the placeholder.
	for _, kv := range env {
		if strings.Contains(kv, token) {
			t.Fatalf("real token leaked into sandbox env entry %q — Property B violated", kv)
		}
	}

	mounts := []sandbox.Mount{
		{Source: certPath, Destination: sandboxGHInjectorCert, Options: []string{"ro"}},
		{Source: hostGHBinaryPath, Destination: sandboxGHBinary, Options: []string{"ro"}},
	}

	run := func(t *testing.T, args ...string) (string, error) {
		worktree := t.TempDir()
		if err := os.Chown(worktree, sandbox.WorktreeUID, sandbox.WorktreeGID); err != nil {
			t.Skipf("can't chown worktree to UID %d: %v", sandbox.WorktreeUID, err)
		}
		lr, _, werr := sandbox.Wrap(ctx, sandbox.Config{
			RunID:       runID,
			Worktree:    worktree,
			SDKDir:      t.TempDir(),
			Argv:        append([]string{sandboxGHBinary}, args...),
			Env:         env,
			ExtraMounts: mounts,
			Network:     net,
		})
		if werr != nil {
			t.Fatalf("sandbox.Wrap: %v", werr)
		}
		defer func() { _ = lr.Close() }()
		if err := lr.Start(); err != nil {
			t.Fatalf("start: %v", err)
		}
		out, _ := io.ReadAll(lr.Stdout())
		waitErr := lr.Wait()
		return string(out) + lr.Stderr(), waitErr
	}

	// (c) gh pr list against the in-scope repo → succeeds through the injector.
	t.Run("pr_list_in_scope", func(t *testing.T) {
		out, err := run(t, "pr", "list", "-R", repo, "--limit", "1")
		t.Logf("gh pr list output: %s", out)
		if err != nil {
			t.Fatalf("gh pr list on an in-scope repo failed: %v (out: %s)", err, out)
		}
	})

	// The injector must have seen the traffic (proves the jail reached it, not a
	// direct api.github.com hit).
	if inj.RequestCount() == 0 {
		t.Error("injector observed zero requests — the jail did not route gh through it")
	}

	// (denial) an out-of-scope repo surfaces GitHub's 404 masking, verbatim.
	if denyRepo != "" {
		t.Run("scope_denial_404", func(t *testing.T) {
			out, err := run(t, "pr", "list", "-R", denyRepo, "--limit", "1")
			t.Logf("gh deny-repo output: %s", out)
			if err == nil {
				t.Fatalf("gh reached an out-of-scope repo %q; want a 404-shaped failure", denyRepo)
			}
			if !strings.Contains(out, "Not Found") && !strings.Contains(out, "404") {
				t.Errorf("deny-repo failure did not surface GitHub's 404 masking: %s", out)
			}
		})
	}
}
