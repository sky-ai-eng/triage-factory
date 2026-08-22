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
//
// TODO(TFAC-885): macOS has a mechanism and currently gets nothing — a local
// run there is still a plain child of this process with the operator's whole
// filesystem in view. The seam is deliberately ready for it: Spec and the
// spawn-side prefix are untagged, so darwin needs its own Args/Probe over
// sandbox-exec rather than a re-plumb. It is not a port of the Linux plan,
// though — Seatbelt filters paths where bubblewrap remaps them, so the profile
// inverts to a deny-by-default allowlist, every path needs canonicalizing
// before it becomes a rule (Seatbelt matches literal strings and macOS reaches
// its temp dir through symlinks), and worktree.selfContainedRunTrees must stop
// keying on the sandbox alone, since a filtered bare cache is still reachable
// and darwin keeps its zero-copy worktrees.

var errUnsupported = errors.New("localsandbox: the local agent sandbox (bubblewrap) is not supported on this platform")

// Resolve always errors off Linux. It is the untagged spawn seam's entry
// point, so it must exist on every platform even though nothing there can
// reach it with the sandbox resolved off.
func Resolve() (string, error) { return "", errUnsupported }

// Args always errors off Linux.
func Args(_ Spec, _ Host) ([]string, error) { return nil, errUnsupported }

// Preflight always errors off Linux.
func Preflight(_ Spec, _ Host) error { return errUnsupported }

// Probe always errors off Linux, which is what a boot that somehow reached
// it with the sandbox on should hear.
func Probe() error { return errUnsupported }

// DNSPaths is empty off Linux. The spawn seam calls it unconditionally while
// building the read-only set, so it returns a value rather than an error —
// there is no mask to bind anything back through here.
func DNSPaths() []string { return nil }
