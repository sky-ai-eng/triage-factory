package curator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	sqlitestore "github.com/sky-ai-eng/triage-factory/internal/db/sqlite"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// fakeTurnSidecar stands in for *delegate.runSidecar behind the
// curator.TurnSidecar seam.
type fakeTurnSidecar struct {
	net    *sandbox.RunNetwork
	env    []string
	closed atomic.Bool
}

func (f *fakeTurnSidecar) Network() *sandbox.RunNetwork                { return f.net }
func (f *fakeTurnSidecar) JailEnv() []string                           { return f.env }
func (f *fakeTurnSidecar) GHChannel(string) *agentproc.GHChannelParams { return nil }
func (f *fakeTurnSidecar) GitCloneAuth(string) worktree.CloneAuth      { return worktree.CloneAuth{} }
func (f *fakeTurnSidecar) Close()                                      { f.closed.Store(true) }

// TestDispatch_ExecutorPath_ConsumesTurnSidecar pins the executor wiring:
// when the SetTurnSidecar seam is present, dispatch stands the turn
// sandbox up, threads its prebuilt network + proxy env into RunOptions, hands
// back the sidecar-hosted socket mount (a noopCloser, not the in-process
// daemon), and tears the sandbox down after the turn.
func TestDispatch_ExecutorPath_ConsumesTurnSidecar(t *testing.T) {
	t.Setenv("TF_STATE_ROOT", t.TempDir())
	database := newTestDB(t)
	projectID := seedProject(t, database, "homed")
	stores := sqlitestore.New(database)
	c := New(stores, nil, "test-model")
	t.Cleanup(c.Shutdown)
	t.Cleanup(startTestClaimLoop(t, stores, c))

	fakeNet := &sandbox.RunNetwork{}
	fakeEnv := []string{"LLM_PROXY_URL=http://127.0.0.1:9"}
	sb := &fakeTurnSidecar{net: fakeNet, env: fakeEnv}

	var gotOrg, gotConv, gotUser, gotTeam string
	bringUpCalled := make(chan struct{}, 1)
	c.SetTurnSidecar(func(_ context.Context, orgID, conversationID, userID, teamID string, _ []string) (TurnSidecar, error) {
		gotOrg, gotConv, gotUser, gotTeam = orgID, conversationID, userID, teamID
		bringUpCalled <- struct{}{}
		return sb, nil
	})

	optsCh := make(chan agentproc.RunOptions, 1)
	c.runAgent = func(_ context.Context, opts agentproc.RunOptions, _ agentproc.Sink) (*agentproc.Outcome, error) {
		optsCh <- opts
		return &agentproc.Outcome{Result: &agentproc.Result{Result: "ok"}}, nil
	}

	if _, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi"); err != nil {
		t.Fatalf("send: %v", err)
	}

	var opts agentproc.RunOptions
	select {
	case opts = <-optsCh:
	case <-time.After(10 * time.Second):
		t.Fatal("runAgent was not called")
	}

	select {
	case <-bringUpCalled:
	default:
		t.Fatal("SetTurnSidecar seam was not invoked on the executor path")
	}
	conv, err := sqlitestore.New(database).Curator.GetLiveConversation(t.Context(), runmode.LocalDefaultOrgID, projectID, runmode.LocalDefaultUserID)
	if err != nil || conv == nil {
		t.Fatalf("get conversation: %v (conv=%v)", err, conv)
	}
	if gotConv != conv.ID || gotOrg != runmode.LocalDefaultOrgID || gotUser != runmode.LocalDefaultUserID || gotTeam != runmode.LocalDefaultTeamID {
		t.Errorf("bring-up args = (org=%q, conv=%q, user=%q, team=%q), want (%q, %q, %q, %q)",
			gotOrg, gotConv, gotUser, gotTeam,
			runmode.LocalDefaultOrgID, conv.ID, runmode.LocalDefaultUserID, runmode.LocalDefaultTeamID)
	}

	if opts.PrebuiltNetwork != fakeNet {
		t.Errorf("PrebuiltNetwork = %v, want the turn sandbox network", opts.PrebuiltNetwork)
	}
	if len(opts.PrebuiltProxyEnv) != 1 || opts.PrebuiltProxyEnv[0] != fakeEnv[0] {
		t.Errorf("PrebuiltProxyEnv = %v, want %v", opts.PrebuiltProxyEnv, fakeEnv)
	}

	// The executor path returns the sidecar-hosted socket mount (a noopCloser),
	// never the in-process agenthost daemon.
	if opts.StartAgentHost == nil {
		t.Fatal("StartAgentHost is nil")
	}
	_, closer, err := opts.StartAgentHost()
	if err != nil {
		t.Fatalf("StartAgentHost: %v", err)
	}
	if _, ok := closer.(noopCloser); !ok {
		t.Errorf("StartAgentHost closer = %T, want noopCloser (sidecar-hosted socket mount)", closer)
	}

	// The sandbox is torn down after the turn completes (deferred Close).
	deadline := time.Now().Add(5 * time.Second)
	for !sb.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatal("turn sandbox was not Closed after the turn")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestDispatch_InProcessPath_SkipsTurnSidecar pins that control/all/local (no
// SetTurnSidecar seam) leave RunOptions' prebuilt fields nil, so agentproc runs
// its own in-process credential resolution + network exactly as before.
func TestDispatch_InProcessPath_SkipsTurnSidecar(t *testing.T) {
	t.Setenv("TF_STATE_ROOT", t.TempDir())
	database := newTestDB(t)
	projectID := seedProject(t, database, "local")
	stores := sqlitestore.New(database)
	c := New(stores, nil, "test-model")
	t.Cleanup(c.Shutdown)
	t.Cleanup(startTestClaimLoop(t, stores, c))
	// No SetTurnSidecar — the untouched in-process path.

	optsCh := make(chan agentproc.RunOptions, 1)
	c.runAgent = func(_ context.Context, opts agentproc.RunOptions, _ agentproc.Sink) (*agentproc.Outcome, error) {
		optsCh <- opts
		return &agentproc.Outcome{Result: &agentproc.Result{Result: "ok"}}, nil
	}

	if _, err := c.SendMessage(t.Context(), projectID, runmode.LocalDefaultOrgID, runmode.LocalDefaultUserID, "hi"); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case opts := <-optsCh:
		if opts.PrebuiltNetwork != nil {
			t.Errorf("PrebuiltNetwork = %v, want nil on the in-process path", opts.PrebuiltNetwork)
		}
		if opts.PrebuiltProxyEnv != nil {
			t.Errorf("PrebuiltProxyEnv = %v, want nil on the in-process path", opts.PrebuiltProxyEnv)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runAgent was not called")
	}
}
