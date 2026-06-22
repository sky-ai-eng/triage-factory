// Package dnsoverride is a tiny seam for injecting a fake DNS-TXT resolver into
// the SSO domain-verification path under test. The verify suite drives ee/sso
// black-box over HTTP, so it can't reach the handler's resolver field through
// the request boundary — it rendezvous on a process-global override here, which
// the verify handler consults before falling back to its real net.DefaultResolver.
//
// Production never sets an override: Get returns nil and the handler uses its
// real resolver. This is a test seam, kept a dependency-free leaf so neither
// ee/sso nor its tests pull anything extra in.
package dnsoverride

import (
	"context"
	"sync/atomic"
)

// Resolver is the DNS-TXT lookup shape the verify handler needs. *net.Resolver
// and test fakes both satisfy it.
type Resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// override always holds a resolverHolder, never a bare Resolver interface:
// atomic.Value panics both on a nil value and on two differently-typed values
// over its lifetime, and the stored Resolver's dynamic type varies (or is nil
// when cleared). Wrapping in a fixed struct type sidesteps both.
var override atomic.Value // stores resolverHolder

type resolverHolder struct{ r Resolver }

// Set installs a test override resolver (or clears it with nil). Safe for
// concurrent use; intended for tests only.
func Set(r Resolver) { override.Store(resolverHolder{r: r}) }

// Get returns the installed override, or nil when none is set (the production
// case — callers fall back to their real resolver).
func Get() Resolver {
	h, _ := override.Load().(resolverHolder)
	return h.r
}
