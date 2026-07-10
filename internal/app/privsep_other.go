//go:build !linux

package app

import (
	"context"
	"errors"
)

// startCapBroker on non-Linux always errors. Never actually reached in
// practice — startCapBrokerIfSandboxing's agentproc.WillSandbox() gate is
// always false off Linux — but kept symmetric with the Linux
// implementation so the build doesn't need a call-site branch.
func startCapBroker(_ context.Context) (capBrokerHandle, error) {
	return nil, errors.New("cap-broker: unsupported on this platform (requires Linux)")
}
