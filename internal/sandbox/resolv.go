package sandbox

import (
	"os"
	"path/filepath"
)

// resolvConfContent is the resolv.conf bind-mounted into the
// sandbox at /etc/resolv.conf. Public DNS resolvers because the
// validation probe established (README "What was learned" point 5)
// that Fly's internal fdaa::3 resolver isn't reachable through the
// sandbox's IPv4 NAT.
//
// Self-host customers may want to override this to use their own
// internal resolvers; a future Config.DNSServers field can add that
// without changing the bind-mount mechanism. For now we hardcode.
const resolvConfContent = `nameserver 1.1.1.1
nameserver 8.8.8.8
options edns0
`

// writeResolvConf writes the synthesized resolv.conf into bundleDir
// for bind-mounting at /etc/resolv.conf inside the sandbox.
func writeResolvConf(bundleDir string) (string, error) {
	path := filepath.Join(bundleDir, "resolv.conf")
	if err := os.WriteFile(path, []byte(resolvConfContent), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
