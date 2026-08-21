//go:build !linux

package localsandbox

import "errors"

// The local sandbox is a Linux mount namespace, so every operative function
// is Linux-tagged and these stubs stand in elsewhere. They are unreachable
// rather than merely unused: runmode.ResolveLocalSandbox refuses an explicit
// TF_LOCAL_SANDBOX=on off Linux and resolves an unset one to off, so nothing
// ever builds a Spec on a platform that lands here. The stubs exist so the
// spawn seam — which names *Spec on every platform — still compiles on a
// Darwin dev box, exactly like cmd/exec/agenthost's socket_other.go.

var errUnsupported = errors.New("localsandbox: the local agent sandbox (bubblewrap) is not supported on this platform")

// Args always errors off Linux.
func Args(_ Spec, _ Host) ([]string, error) { return nil, errUnsupported }

// Preflight always errors off Linux.
func Preflight(_ Spec, _ Host) error { return errUnsupported }

// Probe always errors off Linux, which is what a boot that somehow reached
// it with the sandbox on should hear.
func Probe() error { return errUnsupported }
