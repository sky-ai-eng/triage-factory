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
// of running them in-process.
func startCapBroker(ctx context.Context) (capBrokerHandle, error) {
	handle, client, err := capbroker.Start(ctx)
	if err != nil {
		return nil, err
	}
	sandbox.SetPrivilegedOps(client)
	return handle, nil
}
