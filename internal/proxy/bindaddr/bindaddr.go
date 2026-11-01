// Package bindaddr checks the address a local proxy is about to listen on.
// Every proxy binds loopback by default; the sandbox's veth-IP bind is the
// one deliberate exception, which a proxy opts into with AllowNonLoopback
// and does not run this check for.
package bindaddr

import (
	"fmt"
	"net"
)

// RequireLoopback returns nil iff addr binds to a loopback interface. A
// hostname resolves through the OS resolver and every address it resolves to
// must be loopback, so "localhost" passes and a name pointing at a routable
// LAN address does not. An empty host (the ":NNNN" form), 0.0.0.0 and :: bind
// all interfaces and are refused. The errors name no caller; each proxy
// prefixes its own name.
func RequireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse bind address %q: %w", addr, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Errorf("bind address %q binds to all interfaces — set AllowNonLoopback=true to confirm intent", addr)
	}
	if ip := net.ParseIP(host); ip != nil {
		if !ip.IsLoopback() {
			return fmt.Errorf("bind address %q is not loopback — set AllowNonLoopback=true to confirm intent", addr)
		}
		return nil
	}
	addrs, err := net.LookupHost(host)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", host, err)
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("bind host %q resolves to %s (not loopback) — set AllowNonLoopback=true to confirm intent", host, a)
		}
	}
	return nil
}
