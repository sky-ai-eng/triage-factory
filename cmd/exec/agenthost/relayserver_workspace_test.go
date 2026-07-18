package agenthost

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/db"
)

// TestRelayServer_CreateWorkspaceCheckout_FailsCleanWithoutProxy pins the
// security-relevant edge of relaying workspace-add materialization to the
// orchestrator: the create clones through the run's git proxy, so if an op
// arrives before the sidecar bring-up set the proxy coords (SetProxyCreds), it
// must fail clean — never fall through to an unauthenticated clone. In practice
// bring-up completes before the agent runs, so this only guards a regression in
// that ordering.
func TestRelayServer_CreateWorkspaceCheckout_FailsCleanWithoutProxy(t *testing.T) {
	srv := NewRelayServer(db.Stores{}, RunInfo{OrgID: "org", RunID: "run"}, nil)
	args, err := json.Marshal(createWorkspaceCheckoutArgs{Owner: "sky-ai-eng", Repo: "triage-factory"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	// proxyCreds is nil (SetProxyCreds never called) — the guard must reject
	// before the handler builds a client or touches stores/FS.
	_, err = srv.DispatchCall(context.Background(), agentproc.RelayNamespaceCore, opCreateWorkspaceCheckout, args)
	if err == nil {
		t.Fatal("create_workspace_checkout with no proxy coords returned nil error; want a clean 'git proxy not ready' rejection")
	}
	if !strings.Contains(err.Error(), "git proxy was ready") {
		t.Errorf("error = %v; want the pre-proxy rejection rather than a fall-through into materialization", err)
	}
}
