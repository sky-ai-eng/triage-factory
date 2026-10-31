package gitproxy_test

import (
	"context"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/proxy/gitproxy"
)

// TestReceivePack_RealGitRefusal pushes two refs with a real git client,
// through the proxy, to `git http-backend` over a bare repository whose hook
// refuses one of them with GH013-style text. receive-pack reports that inside
// an HTTP 200, as GitHub does: the refused ref must record as rejected with
// the hook's reason and text, and the accepted one exactly as before.
//
// The hook is `update` rather than `pre-receive`: pre-receive decides for the
// whole push, and this case needs one ref through and one refused.
func TestReceivePack_RealGitRefusal(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "octo", "repo.git")
	work := t.TempDir()
	execPath := gitExecPath(t)
	backend := filepath.Join(execPath, "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not present at %s: %v", backend, err)
	}

	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	runGit(t, bare, "config", "http.receivepack", "true")
	hook := "#!/bin/sh\n" +
		"if [ \"$1\" = refs/heads/blocked ]; then\n" +
		"  echo \"error: GH013: Repository rule violations found for $1.\" >&2\n" +
		"  echo \"- Commits must have verified signatures.\" >&2\n" +
		"  exit 1\n" +
		"fi\n"
	if err := os.WriteFile(filepath.Join(bare, "hooks", "update"), []byte(hook), 0o755); err != nil {
		t.Fatalf("write update hook: %v", err)
	}

	upstream := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=tester"},
	})
	defer upstream.Close()

	var (
		mu     sync.Mutex
		pushes []gitproxy.PushedRef
	)
	srv, err := gitproxy.New(gitproxy.Config{
		Upstream: upstream.URL,
		TokenSource: func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs_tok"}, nil
		},
		Authorize: func(context.Context, string, string) (gitproxy.Decision, error) {
			return gitproxy.Decision{Allowed: true, AllowedRefs: []string{"refs/heads/ok", "refs/heads/blocked"}}, nil
		},
		RecordPush: func(_ context.Context, p gitproxy.PushedRef) {
			mu.Lock()
			pushes = append(pushes, p)
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("gitproxy.New: %v", err)
	}
	addr, err := srv.Start("")
	if err != nil {
		t.Fatalf("Server.Start: %v", err)
	}

	runGit(t, "", "init", "--initial-branch=main", work)
	runGit(t, work, "config", "user.email", "agent@example.test")
	runGit(t, work, "config", "user.name", "Agent")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "change")
	head := strings.TrimSpace(runGit(t, work, "rev-parse", "HEAD"))

	out, err := gitOutput(work, nil, "push", "http://"+addr+"/octo/repo.git", "HEAD:refs/heads/ok", "HEAD:refs/heads/blocked")
	if err == nil {
		t.Fatalf("push succeeded, want git to report the refused ref:\n%s", out)
	}
	if !strings.Contains(out, "GH013") {
		t.Errorf("git's output does not carry the hook's text:\n%s", out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)

	if got := strings.TrimSpace(runGit(t, bare, "rev-parse", "refs/heads/ok")); got != head {
		t.Fatalf("refs/heads/ok = %s on the upstream, want %s", got, head)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(pushes) != 2 {
		t.Fatalf("recorded %d pushes, want 2: %+v\ngit output:\n%s", len(pushes), pushes, out)
	}
	byRef := map[string]gitproxy.PushedRef{}
	for _, p := range pushes {
		byRef[p.Ref] = p
	}
	ok, blocked := byRef["refs/heads/ok"], byRef["refs/heads/blocked"]
	if want := (gitproxy.PushedRef{Repo: "octo/repo", Ref: "refs/heads/ok", NewSHA: head, Created: true, Status: http.StatusOK,
		RemoteMessage: ok.RemoteMessage}); ok != want || !ok.Succeeded() {
		t.Errorf("accepted ref recorded as %+v, want %+v", ok, want)
	}
	if blocked.Status != http.StatusOK || !blocked.Rejected || blocked.Succeeded() {
		t.Errorf("refused ref recorded as %+v, want a rejected ref under a 200", blocked)
	}
	if blocked.Reason != "hook declined" {
		t.Errorf("reason = %q, want receive-pack's %q", blocked.Reason, "hook declined")
	}
	want := "error: GH013: Repository rule violations found for refs/heads/blocked.\n- Commits must have verified signatures."
	if !strings.Contains(blocked.RemoteMessage, want) {
		t.Errorf("remote message = %q, want it to carry %q", blocked.RemoteMessage, want)
	}
	if strings.Contains(blocked.RemoteMessage, "\r") {
		t.Errorf("remote message %q carries a progress meter", blocked.RemoteMessage)
	}
}

// TestReceivePack_RealGitShowsGateRefusal pushes a ref the gate does not
// allow with a real git client: git must print the gate's own explanation as a
// remote rejection, nothing may reach the upstream, and the refusal is audited.
func TestReceivePack_RealGitShowsGateRefusal(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "octo", "repo.git")
	work := t.TempDir()
	backend := filepath.Join(gitExecPath(t), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skipf("git-http-backend not present at %s: %v", backend, err)
	}
	runGit(t, "", "init", "--bare", "--initial-branch=main", bare)
	runGit(t, bare, "config", "http.receivepack", "true")
	upstream := httptest.NewServer(&cgi.Handler{
		Path: backend,
		Env:  []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=tester"},
	})
	defer upstream.Close()

	sink := newDenialSink()
	_, proxyURL := startGatedProxy(t,
		func(context.Context, string, string) (gitproxy.Token, error) {
			return gitproxy.Token{Value: "ghs_tok"}, nil
		},
		upstream.URL,
		func(context.Context, string, string) (gitproxy.Decision, error) {
			return gitproxy.Decision{Allowed: true, AllowedRefs: []string{"refs/heads/mine"}}, nil
		},
		sink.record)

	runGit(t, "", "init", "--initial-branch=main", work)
	runGit(t, work, "config", "user.email", "agent@example.test")
	runGit(t, work, "config", "user.name", "Agent")
	runGit(t, work, "commit", "--allow-empty", "-m", "change")

	// Three refused refs make a report longer than one side-band packet, and
	// the allowed ref is refused with them because the push is one stream.
	out, err := gitOutput(work, nil, "push", proxyURL+"/octo/repo.git",
		"HEAD:refs/heads/mine", "HEAD:refs/heads/theirs", "HEAD:refs/heads/theirs-2", "HEAD:refs/heads/theirs-3")
	if err == nil {
		t.Fatalf("push succeeded, want the gate to refuse it:\n%s", out)
	}
	for _, want := range []string{
		"[remote rejected] HEAD -> mine (gitproxy: not pushed, because another ref in this push was refused)",
		"refs/heads/theirs is not pushable in octo/repo",
		"refs/heads/theirs-3 is not pushable in octo/repo",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("git's output does not carry %q:\n%s", want, out)
		}
	}
	if refs := runGit(t, bare, "for-each-ref"); refs != "" {
		t.Errorf("the upstream received refs: %s", refs)
	}
	if d := sink.next(t); d.Reason != "ref-not-allowed" || d.Ref != "refs/heads/theirs" {
		t.Errorf("denial = %+v, want ref-not-allowed for refs/heads/theirs", d)
	}
}

func gitExecPath(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not installed: %v", err)
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skipf("git --exec-path failed: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(dir, nil, args...)
	if err != nil {
		t.Fatalf("git %s in %q: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return out
}

func gitOutput(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// A clean environment: the surrounding CI/dev box may carry its own
	// GIT_CONFIG_*, http_proxy, or insteadOf rewrites, any of which would
	// route the push somewhere other than the proxy under test.
	cmd.Env = append([]string{
		"HOME=" + dir,
		"PATH=" + os.Getenv("PATH"),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	}, env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
