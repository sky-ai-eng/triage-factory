//go:build !linux

package gvisor

import (
	"context"

	"github.com/sky-ai-eng/triage-factory/internal/cell"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// Launch on non-Linux is unreachable — Provision already failed with the same
// error — and refuses rather than pretending to launch.
func (c *gvisorCell) Launch(context.Context, cell.LaunchSpec) (cell.Jail, error) {
	return nil, sandbox.ErrUnsupportedPlatform
}
