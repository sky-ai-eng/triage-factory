//go:build linux

package sandbox

import "context"

// reapOrphansImpl is the Linux entry point reapOrphans (reaper.go)
// dispatches to. It routes through defaultOps like every other
// privileged-op entry point in this package (wrap, Close) — the
// actual netns/veth/iptables/cgroup sweep lives in hostOps.ReapOrphans
// (hostops_linux.go).
func reapOrphansImpl(ctx context.Context) error {
	return defaultOps.ReapOrphans(ctx)
}
