//go:build !linux

package capinfo

// Effective and Bounding are no-ops off Linux: capabilities are a Linux
// kernel concept, and the sandbox/privsep split this reports on
// (agentproc.WillSandbox) is Linux-only already. Returning an empty,
// nil-error result lets a boot-line caller log "(empty)" unconditionally
// instead of special-casing the platform.
func Effective() ([]string, error) { return nil, nil }
func Bounding() ([]string, error)  { return nil, nil }
