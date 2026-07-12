//go:build !linux

package agenthost

import (
	"context"
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/credbundle"
	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// HostDaemon is the non-Linux stub. The sandbox path is gated to
// Linux upstream (shouldSandbox in agentproc), so production code
// never calls Start off Linux; this exists to keep the package
// compileable on Darwin dev boxes.
type HostDaemon struct{}

// Start always returns an "unsupported platform" error off Linux.
// Callers wire it under the same Linux build tag the sandbox runner
// requires.
func Start(_ db.Stores, _ RunInfo, _ func(ctx context.Context) (*credbundle.Bundle, bool)) (*HostDaemon, sandbox.Mount, error) {
	return nil, sandbox.Mount{}, errors.New("agenthost: HostDaemon not supported on this platform")
}

// Close is a no-op on non-Linux.
func (h *HostDaemon) Close() error { return nil }

// SocketPath returns "" on non-Linux.
func (h *HostDaemon) SocketPath() string { return "" }
