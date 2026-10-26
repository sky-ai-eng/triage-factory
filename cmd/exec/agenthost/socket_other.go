//go:build !linux

package agenthost

import (
	"errors"

	"github.com/sky-ai-eng/triage-factory/internal/db"
	"github.com/sky-ai-eng/triage-factory/internal/sandbox"
)

// HostDaemon is the non-Linux stub. The sandbox path is gated to
// Linux upstream (shouldSandbox in agentproc), so production code
// never starts one off Linux; this exists to keep the package
// compileable on Darwin dev boxes.
type HostDaemon struct{}

// StartLocal always returns an "unsupported platform" error off Linux. The
// local sandbox is a Linux mount namespace, so nothing off Linux ever asks
// for its socket (runmode resolves the posture to off there).
func StartLocal(_ db.Stores, _ ConversationInfo, _ string, _ *LocalCommitSigning) (*HostDaemon, error) {
	return nil, errors.New("agenthost: HostDaemon not supported on this platform")
}

// StartWithServer always returns an "unsupported platform" error off Linux.
func StartWithServer(_ *Server, _ string, _ int) (*HostDaemon, sandbox.Mount, error) {
	return nil, sandbox.Mount{}, errors.New("agenthost: HostDaemon not supported on this platform")
}

// Close is a no-op on non-Linux.
func (h *HostDaemon) Close() error { return nil }

// SocketPath returns "" on non-Linux.
func (h *HostDaemon) SocketPath() string { return "" }

// SocketPathFor returns "" on non-Linux (the sandbox path is Linux-only).
func SocketPathFor(_ string) string { return "" }

// SocketMountFor returns a zero mount on non-Linux.
func SocketMountFor(_ string) sandbox.Mount { return sandbox.Mount{} }

// CABundlePathFor returns "" on non-Linux (the sandbox path is Linux-only).
func CABundlePathFor(_ string) string { return "" }

// WriteCABundle is unsupported off Linux.
func WriteCABundle(_ string, _ []byte, _ int) error {
	return errors.New("agenthost: the jail's CA bundle is not supported on this platform")
}
