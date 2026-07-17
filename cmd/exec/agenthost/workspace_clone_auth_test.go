package agenthost

import (
	"context"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// TestWorkspaceCloneAuth pins the credential form workspace-add's host-side clone
// receives in each placement — the security-relevant branch. On the executor
// sidecar the clone MUST route through the run's git proxy carrying only the
// per-run placeholder, so the org's real installation token never enters this
// hostile-traffic-parsing daemon; local mode injects nothing (operator's own
// git); and a sidecar whose run started no git proxy injects nothing rather than
// dereferencing the resolver, which is nil there (the original panic).
func TestWorkspaceCloneAuth(t *testing.T) {
	const cloneURL = "https://github.com/sky-ai-eng/triage-factory.git"
	ctx := context.Background()

	t.Run("local mode injects nothing", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeLocal)
		c := &LocalClient{info: RunInfo{OrgID: "org", RunID: "run"}}
		if got := c.workspaceCloneAuth(ctx, "sky-ai-eng", cloneURL); got != (worktree.CloneAuth{}) {
			t.Errorf("local clone auth = %+v, want the inert zero value", got)
		}
	})

	t.Run("executor sidecar routes through the git proxy with the placeholder", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		c := &LocalClient{
			info: RunInfo{OrgID: "org", RunID: "run"},
			proxyCreds: &ProxyCredentials{
				GitProxyURL:   "http://10.42.0.1:4100",
				GitProxyToken: "per-run-placeholder",
			},
		}
		got := c.workspaceCloneAuth(ctx, "sky-ai-eng", cloneURL)
		want := worktree.CloneAuthViaGitProxy("http://10.42.0.1:4100", "https://github.com", "per-run-placeholder")
		if got != want {
			t.Errorf("sidecar clone auth = %+v, want the git-proxy routing form %+v", got, want)
		}
		if got == (worktree.CloneAuth{}) {
			t.Error("sidecar clone auth is inert; a private-repo clone would go unauthenticated")
		}
	})

	t.Run("executor sidecar with no git proxy injects nothing without touching the resolver", func(t *testing.T) {
		runmode.SetForTest(t, runmode.ModeMulti)
		// proxyCreds present (executor) but the run started no git proxy. The
		// resolver is nil here; a fall-through to it is the panic this guards.
		c := &LocalClient{info: RunInfo{OrgID: "org", RunID: "run"}, proxyCreds: &ProxyCredentials{}}
		if got := c.workspaceCloneAuth(ctx, "sky-ai-eng", cloneURL); got != (worktree.CloneAuth{}) {
			t.Errorf("no-git-proxy clone auth = %+v, want the inert zero value", got)
		}
	})
}
