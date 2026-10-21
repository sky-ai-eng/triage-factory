//go:build linux

package gvisor

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// Launch implements cell.Cell: the tool host as the main process of a runsc
// jail on this cell's network, every input bind-mounted at its fixed in-jail
// path. The broker validates each mount source against its own per-run
// derivation, so the paths named here are requests, not grants.
func (c *gvisorCell) Launch(ctx context.Context, spec cell.LaunchSpec) (cell.Jail, error) {
	return LaunchOnNetwork(ctx, c.net, c.conversationID, spec)
}

// LaunchOnNetwork launches a jail on a run network the caller built itself,
// with no cell around it: the network, and anything the caller started on
// it, are the caller's to close after the jail. It exists for measuring jails
// apart from their sidecars (cmd/sandbox-bench); an engagement launches
// through Cell.Launch, which is this on the cell's own network.
//
// The gateway admits the jail to spec's sidecar endpoints before the jail
// starts, so its first connection already finds them open.
func LaunchOnNetwork(ctx context.Context, rn *sandbox.RunNetwork, conversationID string, spec cell.LaunchSpec) (cell.Jail, error) {
	ports, err := gatewayPorts(rn.HostIP, spec.SidecarEndpoints)
	if err != nil {
		return nil, err
	}
	if err := rn.AdmitJailPorts(ctx, ports); err != nil {
		return nil, fmt.Errorf("gvisor: admit jail to sidecar endpoints: %w", err)
	}
	jail, err := launchToolHost(ctx, rn, conversationID, spec)
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
