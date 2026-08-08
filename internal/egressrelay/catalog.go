package egressrelay

import "net/url"

// This file is the curated relay catalog: every ecosystem the fetch-relay
// lane serves, expressed as data (a route table + the env entries that aim
// the in-jail tool at the relay). The engine in relay.go is generic and
// should not need changes for a new ecosystem — if an ecosystem seems to
// need more than routes, a synthetic capability probe, and env pointing,
// that is a signal it may not fit this lane; open a design discussion
// before writing code.
//
// Before adding an entry here, read this package's CLAUDE.md: it has the
// probe-first decision procedure (most ecosystems belong in the tunnel
// lane's hostname allowlist, NOT here), the integrity prerequisite (the
// relay is untrusted only because the ecosystem's client verifies content
// itself), and the contributor checklist (tests, env allowlisting, docs).
//
// TFAC-408's sandbox profiles will eventually serialize these entries as
// per-profile egress rules; keeping them as plain data constructors here is
// what makes that a serialization exercise rather than a rewrite.

const (
	// GoModuleUpstream is the public Go module proxy.
	GoModuleUpstream = "https://proxy.golang.org"

	// GoSumDB is the public Go checksum database. It is a SEPARATE
	// upstream from the module proxy, not a path under it:
	// proxy.golang.org answers 404 for /sumdb/... because sumdb relaying
	// is a feature for private proxies whose clients cannot reach the
	// internet — exactly our situation. Verified against the live
	// service; without our own relay the jail would still have to reach
	// sum.golang.org directly.
	GoSumDB = "https://sum.golang.org"
)

// CatalogEntry pairs a relay's route table with the env entries that make
// an in-jail tool use it. The two travel together because a relay nothing
// points at is dead weight and env pointing at a relay that doesn't exist
// is a hang.
type CatalogEntry struct {
	Config Config
	// EnvKeys names every env KEY Env emits, without values. Declared
	// separately because two consumers must reason about the keys before
	// any relay has an address: buildSandboxEnv drops these keys from
	// caller ExtraEnv (env duplicate resolution is first-wins on Linux
	// and the relay's copy is appended last, so an inherited copy would
	// win and point the tool at an unreachable host), and the broker env
	// allowlist drift test checks each key is admitted. A catalog test
	// pins EnvKeys and Env against each other so they cannot drift.
	EnvKeys []string
	// Env returns the env entries for a relay bound at addr
	// (host:port). Keys emitted here must be on the broker's sandbox env
	// allowlist (internal/sandbox launchspec_linux.go) — the agentproc
	// env drift test iterates Catalog() and fails with the missing key's
	// name if one is forgotten.
	Env func(addr string) []string
}

// Catalog returns every relay the executor starts for a sandboxed run.
// Order is not significant. Adding an ecosystem means appending here —
// the per-run wiring (agentproc.startFetchRelaysForSandbox) iterates this
// and needs no changes.
func Catalog() []CatalogEntry {
	return []CatalogEntry{
		{Config: GoModules(), EnvKeys: []string{"GOPROXY"}, Env: GoModulesEnv},
	}
}

// CatalogEnvKeys returns every env key any catalog entry emits — the
// authoritative drop-list for env assemblers that must keep an inherited
// copy of a relay key from shadowing the relay's own (see
// agentproc.buildSandboxEnv).
func CatalogEnvKeys() []string {
	var keys []string
	for _, e := range Catalog() {
		keys = append(keys, e.EnvKeys...)
	}
	return keys
}

// GoModules is the Go ecosystem's relay: the GOPROXY protocol surface,
// relayed. It exists because proxy.golang.org 302s large module zips (and
// every toolchain) to storage.googleapis.com — a shared storage namespace
// the tunnel lane must never admit.
func GoModules() Config {
	return goModules(GoModuleUpstream, GoSumDB)
}

// goModules is GoModules with the upstreams as parameters, so tests can
// point the same route shape at httptest servers.
func goModules(moduleUpstream, sumDB string) Config {
	sumHost := hostOf(sumDB)
	return Config{
		Name: "go-modules",
		Routes: []Route{
			// The sumdb capability probe. cmd/go asks this before routing
			// any checksum traffic through a proxy; a 200 means "I relay
			// the sumdb", and anything else sends it to sum.golang.org
			// DIRECTLY — which the jail cannot reach. Synthesized rather
			// than relayed precisely because the upstream module proxy
			// answers 404 here.
			{Prefix: "/sumdb/" + sumHost + "/supported", Exact: true, SyntheticStatus: 200},

			// Checksum-database traffic, forwarded to the sumdb itself
			// (NOT the module proxy — see the GoSumDB doc). Verification
			// stays fully enforced client-side; only its transport
			// changes.
			{Prefix: "/sumdb/" + sumHost + "/", Upstream: sumDB, StripPrefix: true},

			// A sumdb we don't relay. Fenced rather than falling through
			// to the module proxy, where it would 404 for a misleading
			// reason.
			{Prefix: "/sumdb/", DenyMessage: "egressrelay: unsupported checksum database"},

			// Everything else is the module protocol, path verbatim.
			{Prefix: "/", Upstream: moduleUpstream},
		},
	}
}

// GoModulesEnv aims the sandboxed cmd/go at this run's relay.
//
// The ",direct" fallback is deliberate, and it is about PRIVATE modules.
// `direct` makes cmd/go fetch from the VCS host, and for a github.com
// module path that is a git operation — which the sandbox's git config
// rewrites onto the per-run git proxy, an authenticated path that already
// exists. So the fallback reaches a working, credential-injecting route
// rather than an unreachable host, and it adds no reach: the git proxy
// applies its own per-repo authorization, and any other VCS host is simply
// not allowlisted.
//
// It matters even where the fetch is ultimately refused. A private
// dependency that has not been materialized draws the git proxy's
// actionable denial ("run 'workspace add <repo>' ... then retry"); dying
// at the relay instead would surface as a bare "module not found",
// stranding an agent that had a remedy available.
//
// Note "," not "|": cmd/go falls through only on 404/410, so a relay
// OUTAGE is a hard error rather than a silent bypass to direct fetching.
//
// GOSUMDB is left at its default: the relay serves the /sumdb/ arm, so
// checksum verification flows through this same address and stays fully
// enforced. Nothing here may ever emit GOSUMDB/GONOSUMDB/GOFLAGS — a
// routing change that quietly disabled verification would look like
// routing config (a test pins this).
//
// http:// on the local hop, matching the LLM and git proxies: the jail
// talks cleartext across the veth pair and the relay talks TLS to the real
// upstream. Nothing secret crosses the local hop — the bytes are public
// module data either way.
func GoModulesEnv(addr string) []string {
	return []string{"GOPROXY=http://" + addr + ",direct"}
}

// hostOf extracts the host from a scheme://host upstream string. Catalog
// constructors call it on their own constants, which parseUpstream
// re-validates at New; a malformed constant fails loudly there.
func hostOf(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return upstream
	}
	return u.Host
}
