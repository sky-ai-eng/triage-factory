//go:build !linux

package procinfo

// EffectiveCaps and BoundingCaps are no-ops off Linux: capabilities are a Linux
// kernel concept, and the sandbox/privsep split this reports on
// (agentproc.WillSandbox) is Linux-only already. Returning an empty,
// nil-error result lets a boot-line caller log "(empty)" unconditionally
// instead of special-casing the platform.
func EffectiveCaps() ([]string, error) { return nil, nil }
func BoundingCaps() ([]string, error)  { return nil, nil }
