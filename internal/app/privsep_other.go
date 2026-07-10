//go:build !linux

package app

import (
	"context"
	"errors"
)

// startCapBroker on non-Linux always errors. Never actually reached in
// practice — startCapBrokerIfSandboxing's agentproc.WillSandbox() gate is
// always false off Linux — but kept symmetric with the Linux
// implementation so the build doesn't need a call-site branch. The nil
// Ping seam matches: there is no broker off Linux to probe.
func startCapBroker(_ context.Context) (capBrokerHandle, func(context.Context) error, error) {
	return nil, nil, errors.New("cap-broker: unsupported on this platform (requires Linux)")
}
