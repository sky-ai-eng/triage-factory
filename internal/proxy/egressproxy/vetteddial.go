package egressproxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
)

// resolveFunc and dialFunc name the two seams the vetted dial is built
// on. Production uses the host resolver and a plain net.Dialer; tests
// substitute a fake resolver to steer a "public" hostname at a denied or
// loopback address and observe exactly what would be dialed.
type resolveFunc func(ctx context.Context, host string) ([]netip.Addr, error)
type dialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// vettedDial resolves host on the HOST side, drops every denylisted
// address, and dials the survivors as IP literals in resolver order.
// Dialing the vetted literal — never the hostname — is the DNS-rebinding
// defense: there is no second resolution between check and connect for an
// attacker-controlled record to flip.
//
// Shared by the CONNECT proxy's own dial and by VettedDialContext so the
// denylist is applied by exactly one piece of code. A second copy of this
// loop would be a place for the two to drift, and the whole point of the
// compiled-in denylist is that no path around it exists.
func vettedDial(ctx context.Context, resolve resolveFunc, dial dialFunc, host, port string) (net.Conn, error) {
	addrs, err := resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(addrs) == 0 {
		// Distinct from the all-denylisted case below: DefaultResolver
		// normally errors on no records, but a custom resolver (or a
		// future seam consumer) may return an empty, error-free answer —
		// don't accuse an empty result of being "internal".
		return nil, policyDenial{reason: fmt.Sprintf(
			"host %q resolved to no addresses", host)}
	}
	vetted := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if !ipDenied(a) {
			vetted = append(vetted, a)
		}
	}
	if len(vetted) == 0 {
		return nil, policyDenial{reason: fmt.Sprintf(
			"host %q resolves only to internal/denied addresses; refusing to connect", host)}
	}
	var lastErr error
	for _, a := range vetted {
		dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		conn, derr := dial(dialCtx, "tcp", net.JoinHostPort(a.Unmap().String(), port))
		cancel()
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	return nil, fmt.Errorf("dial %s: %w", host, lastErr)
}

// VettedDialContext is the internal denylist gate in http.Transport
// DialContext shape, for host-side clients that fetch on a sandbox's
// behalf rather than tunnelling for it.
//
// Such a client is a confused deputy waiting to happen: it runs host-side,
// where the cloud metadata endpoint, the operator's private network, and
// every sibling run's gateway are all reachable, and it fetches a URL the
// sandbox named. Worse, it follows redirects, so the *upstream* also gets
// to choose a destination. Applying the denylist at the dial means every
// hop is vetted — the initial request and each redirect alike — because
// each one dials through this same Transport. There is no redirect
// bookkeeping to forget.
//
// It deliberately enforces only the operator-owned internal denylist, not
// the per-run public allowlist: the caller has already decided which
// upstream service it is talking to. This is the floor under that choice,
// not a replacement for it.
func VettedDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("egressproxy: vetted dial target %q: %w", addr, err)
	}
	return vettedDial(ctx, defaultResolve, defaultDial, host, port)
}

// defaultResolve / defaultDial are the production seam values, named here
// so the Server and VettedDialContext share one definition of "the real
// resolver" and "a plain dialer".
func defaultResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

func defaultDial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}
