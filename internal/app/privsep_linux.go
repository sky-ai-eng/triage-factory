//go:build linux

package app

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/cmd/capbroker"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// startCapBroker spawns the cap-broker subprocess and installs its IPC
// client as internal/sandbox's PrivilegedOps implementation, so every
// subsequent sandbox.Wrap / Close / ReapOrphans on this process routes
// its netns/iptables/cgroup/rootfs operations through the broker instead
// of running them in-process. It also returns a Ping seam over the same
// client for the executor healthz's broker_ok probe.
func startCapBroker(ctx context.Context) (capBrokerHandle, func(context.Context) error, error) {
	handle, client, err := capbroker.Start(ctx)
	if err != nil {
		return nil, nil, err
	}
	sandbox.SetPrivilegedOps(client)
	var ping func(context.Context) error
	if p, ok := client.(interface{ Ping(context.Context) error }); ok {
		ping = p.Ping
	}
	return handle, ping, nil
}
