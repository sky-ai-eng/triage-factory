//go:build !linux

package capbroker

import "errors"

// runBroker on non-Linux always errors. The privileged operations this
// package brokers (internal/sandbox) require Linux; the orchestrator only
// ever spawns cap-broker when agentproc.WillSandbox() (multi mode +
// Linux), so this should never fire in production — it exists so the
// package still compiles on a Darwin dev box and fails loudly rather than
// silently for a misconfigured direct invocation.
func runBroker(_ []string) error {
	return errors.New("cap-broker: unsupported on this platform (requires Linux)")
}
