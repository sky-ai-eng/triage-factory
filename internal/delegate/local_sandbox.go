package delegate

import (
	"errors"
	"fmt"
	"io"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/agentproc/localsandbox"
	"github.com/sky-ai-eng/triage-factory/internal/paths"
	"github.com/sky-ai-eng/triage-factory/internal/runmode"
	"github.com/sky-ai-eng/triage-factory/internal/worktree"
)

// localSandboxChannel is one local run's mount-namespace plan together with
// the agenthost daemon its exec verbs dial. The two are created and torn down
// as a unit: the plan binds the daemon's socket, and a plan whose socket has
// been removed is a run whose `tfac exec` calls all fail.
type localSandboxChannel struct {
	spec   *localsandbox.Spec
	daemon io.Closer
}

// Close stops the daemon and removes its socket. Nil-safe, so the caller
// defers it unconditionally.
func (c *localSandboxChannel) Close() error {
	if c == nil || c.daemon == nil {
		return nil
	}
	return c.daemon.Close()
}

// runSpec is the plan agentproc wraps the spawn in, or nil when this run
// isn't sandboxed — which is the same nil that means "today's behavior".
func (c *localSandboxChannel) runSpec() *localsandbox.Spec {
	if c == nil {
		return nil
	}
	return c.spec
}

// startLocalSandbox builds this run's mount-namespace plan and stands up the
// per-run agenthost daemon that serves its exec verbs.
//
// The daemon is the half that makes the plan survivable. Inside the namespace
// the state root is masked, so the agent's `tfac exec` invocations cannot open
// the SQLite database the host CLI would have opened — and they don't try:
// the marker agentproc sets flips the binary to its jail-cli identity, where
// every verb is an RPC over this socket and every non-exec subcommand is
// refused by name. That is the multi-mode posture arriving in local mode with
// no new client code, and the refusals are correct rather than merely
// tolerable: the pre-push hook stands down before shelling out whenever the
// git channel is up, and a run without a channel has no push credential to
// begin with (the operator's is masked along with the rest of their home).
//
// Returns (nil, nil) when the sandbox is off, which is every path that never
// resolved the posture — a CLI invocation, an un-inited test — as well as an
// operator who set TF_LOCAL_SANDBOX=off.
//
// A failure here fails the run. The boot probe already established that
// bubblewrap works on this host, so there is nothing left that would make a
// silent unsandboxed fallback the honest answer.
func (s *Spawner) startLocalSandbox(conversationID, rootKey string, ghChannel *agentproc.GHChannelParams, info agenthost.ConversationInfo) (*localSandboxChannel, error) {
	if !runmode.LocalSandboxEnabled() {
		return nil, nil
	}
	if rootKey == "" {
		return nil, errors.New("local sandbox: run-tree key is required")
	}
	stores, ok := s.getStores()
	if !ok {
		return nil, errors.New("local sandbox: stores are unwired, so the run's exec verbs would have no daemon to dial")
	}
	daemon, err := agenthost.StartLocal(stores, info, paths.AgentHostSocketPath(conversationID))
	if err != nil {
		return nil, fmt.Errorf("local sandbox: %w", err)
	}
	spec := &localsandbox.Spec{
		// Keyed by the run-tree key rather than the conversation: a blueprint's
		// steps share one tree, so this is the same directory across them and
		// the same directory a resumed step rebuilds at.
		RunRoot:         worktree.RunRoot(rootKey),
		AgentHostSocket: daemon.SocketPath(),
	}
	if ghChannel != nil {
		// The conversation's own gh config + trust file. Only this
		// conversation's dir comes back through the state-root mask, so a
		// concurrent run's channel is as absent as the database is.
		spec.GHChannelDir = paths.GHChannelDir(conversationID)
	}
	return &localSandboxChannel{spec: spec, daemon: daemon}, nil
}
