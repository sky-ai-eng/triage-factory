//go:build linux

package gvisor

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// Launch implements cell.Cell: the tool host as the main process of a runsc
// jail on this cell's network, every input bind-mounted at its fixed in-jail
// path. The broker validates each mount source against its own per-run
// derivation, so the paths named here are requests, not grants.
func (c *gvisorCell) Launch(ctx context.Context, spec cell.LaunchSpec) (cell.Jail, error) {
	jail, err := agentproc.LaunchToolHost(ctx, agentproc.ToolHostOptions{
		ConversationID:   c.conversationID,
		WorkspaceKey:     spec.WorkspaceKey,
		Worktree:         spec.RunTree,
		ExtraEnv:         spec.Env,
		PrebuiltNetwork:  c.net,
		PrebuiltProxyEnv: spec.ProxyEnv,
		AgentHostSocket: sandbox.Mount{
			Source:      spec.AgentHostSocket,
			Destination: sandbox.TrustedAgentHostSocketDestination,
		},
		GHChannel:            ghChannelParams(spec.GHChannel),
		SkillsSourcePath:     spec.SkillsSource,
		MemorySourcePath:     spec.MemorySource,
		MemoryLimitMB:        spec.MemoryLimitMB,
		OrgID:                spec.OrgID,
		ClaimID:              spec.ClaimID,
		RecordSandboxActuals: spec.RecordActuals,
	})
	if err != nil {
		return nil, err
	}
	return jail, nil
}

func ghChannelParams(gc *cell.GHChannel) *agentproc.GHChannelParams {
	if gc == nil {
		return nil
	}
	return &agentproc.GHChannelParams{Host: gc.Host, Token: gc.Token, CertSourcePath: gc.CertSourcePath}
}
