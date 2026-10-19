package sandbox

import "fmt"

// MaxJailPorts bounds how many gateway ports one jail may be admitted to.
// A run's sidecar exposes a handful of jail-facing listeners (the egress
// proxy, the fetch relays, the git proxy, the gh channel); a request for many
// more is not a sidecar describing itself.
const MaxJailPorts = 16

// ValidateJailPorts checks a set of gateway ports to admit a jail to: each a
// TCP port number, none repeated, at most MaxJailPorts. A privileged port is
// refused unless it is SharedOriginPort: the sidecar's own listeners bind
// ephemeral ports, so a request for 22 is not a sidecar describing itself,
// and the host services that live below 1024 are the ones least meant for a
// jail. Shared by the orchestrator, which refuses a bad set before asking,
// and the privileged side, which refuses it whatever it is asked.
func ValidateJailPorts(ports []int) error {
	if len(ports) > MaxJailPorts {
		return fmt.Errorf("sandbox: %d jail ports requested; at most %d may be admitted", len(ports), MaxJailPorts)
	}
	seen := make(map[int]bool, len(ports))
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("sandbox: jail port %d is not a TCP port", p)
		}
		if p < 1024 && p != SharedOriginPort {
			return fmt.Errorf("sandbox: jail port %d is privileged and not the shared origin's", p)
		}
		if seen[p] {
			return fmt.Errorf("sandbox: jail port %d requested twice", p)
		}
		seen[p] = true
	}
	return nil
}
