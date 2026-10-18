//go:build linux

package gvisor

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/agentproc"
	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// Launch implements cell.Cell: the tool host as the main process of a runsc
// jail on this cell's network, every input bind-mounted at its fixed in-jail
// path. The broker validates each mount source against its own per-run
// derivation, so the paths named here are requests, not grants.
//
// The gateway admits the jail to the sidecar's endpoints before the jail
// starts, so its first connection already finds them open.
func (c *gvisorCell) Launch(ctx context.Context, spec cell.LaunchSpec) (cell.Jail, error) {
	ports, err := gatewayPorts(c.net.HostIP, spec.SidecarEndpoints)
	if err != nil {
		return nil, err
	}
	if err := c.net.AdmitJailPorts(ctx, ports); err != nil {
		return nil, fmt.Errorf("gvisor: admit jail to sidecar endpoints: %w", err)
	}
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

// gatewayPorts parses endpoints into the gateway ports they name. Every
// endpoint must be on the gateway itself: the jail reaches nothing else on the
// host, so an endpoint elsewhere is one it could never dial.
func gatewayPorts(gateway string, endpoints []string) ([]int, error) {
	ports := make([]int, 0, len(endpoints))
	for _, ep := range endpoints {
		host, portStr, err := net.SplitHostPort(ep)
		if err != nil {
			return nil, fmt.Errorf("gvisor: sidecar endpoint %q: %w", ep, err)
		}
		if host != gateway {
			return nil, fmt.Errorf("gvisor: sidecar endpoint %q is not on the cell's gateway %s", ep, gateway)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("gvisor: sidecar endpoint %q: bad port", ep)
		}
		ports = append(ports, port)
	}
	return ports, nil
}

func ghChannelParams(gc *cell.GHChannel) *agentproc.GHChannelParams {
	if gc == nil {
		return nil
	}
	return &agentproc.GHChannelParams{Host: gc.Host, Token: gc.Token, CertSourcePath: gc.CertSourcePath}
}
