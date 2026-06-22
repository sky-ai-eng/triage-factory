// Package dnsoverride is a tiny neutral seam for injecting a fake DNS-TXT
// resolver into the SSO domain-verification path under test. It exists so a
// test in package server (which cannot import ee/sso — that would cycle, since
// ee/sso imports server) and the ee/sso verify handler can rendezvous on a
// resolver override through a shared leaf package that imports neither.
//
// Production never sets an override: Get returns nil and the handler uses its
// real net.DefaultResolver. This is a test seam, kept minimal and dependency-
// free on purpose.
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

var override atomic.Value // stores Resolver

// Set installs a test override resolver (or clears it with nil). Safe for
// concurrent use; intended for tests only.
func Set(r Resolver) {
	if r == nil {
		override.Store((Resolver)(nil))
		return
	}
	override.Store(r)
}

// Get returns the installed override, or nil when none is set (the production
// case — callers fall back to their real resolver).
func Get() Resolver {
	r, _ := override.Load().(Resolver)
	return r
}
