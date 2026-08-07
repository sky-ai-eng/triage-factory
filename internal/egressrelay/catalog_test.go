package egressrelay

import (
	"strings"
	"testing"
)

// TestCatalog_EveryEntryConstructsAndCarriesEnv is the floor under the
// catalog contract: agentproc.startFetchRelaysForSandbox iterates
// Catalog() blindly, so an entry whose Config fails New, or whose Env is
// nil, would fail every sandbox run at proxy bring-up. A contributor
// adding an entry hits this before anything reaches an executor.
func TestCatalog_EveryEntryConstructsAndCarriesEnv(t *testing.T) {
	entries := Catalog()
	if len(entries) == 0 {
		t.Fatal("Catalog() is empty; the go-modules entry should always exist")
	}
	for _, e := range entries {
		if _, err := New(e.Config); err != nil {
			t.Errorf("catalog entry %q does not construct: %v", e.Config.Name, err)
		}
		if e.Env == nil {
			t.Errorf("catalog entry %q has a nil Env; the relay would start with nothing pointing at it", e.Config.Name)
			continue
		}
		if env := e.Env("10.42.1.1:9001"); len(env) == 0 {
			t.Errorf("catalog entry %q Env returned no entries", e.Config.Name)
		}
	}
}

// TestGoModules_RouteShape pins the Go entry's load-bearing structure: the
// synthetic sumdb probe, the sumdb relay targeting the checksum host (not
// the module proxy), the fence keeping unknown sumdbs from falling
// through, and the module-protocol catch-all — in that order, because
// first-match-wins.
func TestGoModules_RouteShape(t *testing.T) {
	cfg := GoModules()
	if cfg.Name != "go-modules" {
		t.Errorf("Name = %q, want go-modules", cfg.Name)
	}
	if len(cfg.Routes) != 4 {
		t.Fatalf("got %d routes, want 4: %+v", len(cfg.Routes), cfg.Routes)
	}
	probe, sum, fence, all := cfg.Routes[0], cfg.Routes[1], cfg.Routes[2], cfg.Routes[3]

	if probe.Prefix != "/sumdb/sum.golang.org/supported" || !probe.Exact || probe.SyntheticStatus != 200 {
		t.Errorf("probe route = %+v; want exact synthetic 200 at /sumdb/sum.golang.org/supported", probe)
	}
	if sum.Prefix != "/sumdb/sum.golang.org/" || sum.Upstream != GoSumDB || !sum.StripPrefix {
		t.Errorf("sumdb route = %+v; want strip-prefix relay to %s", sum, GoSumDB)
	}
	if fence.Prefix != "/sumdb/" || fence.Upstream != "" || fence.SyntheticStatus != 0 {
		t.Errorf("fence route = %+v; want a 404 fence on /sumdb/", fence)
	}
	if all.Prefix != "/" || all.Upstream != GoModuleUpstream || all.StripPrefix {
		t.Errorf("catch-all route = %+v; want verbatim relay to %s", all, GoModuleUpstream)
	}
}

// TestGoModulesEnv_KeepsDirectFallback pins the private-module path.
//
// A private dependency is never served by the public proxy, so it reaches
// the VCS only through the ",direct" arm — and for a github.com module
// path that is a git operation, which the sandbox's git config rewrites
// onto the authenticated per-run git proxy. Dropping the fallback would
// strand those fetches at the relay with a bare "module not found" instead
// of the git proxy's actionable denial, so the arm is load-bearing rather
// than decorative.
//
// The separator is asserted too: "," falls through on 404/410 only,
// whereas "|" would fall through on ANY error and turn a relay outage into
// a silent bypass to direct fetching.
func TestGoModulesEnv_KeepsDirectFallback(t *testing.T) {
	env := GoModulesEnv("10.42.1.1:9001")
	if len(env) != 1 {
		t.Fatalf("GoModulesEnv emitted %d entries, want 1: %v", len(env), env)
	}
	const want = "GOPROXY=http://10.42.1.1:9001,direct"
	if env[0] != want {
		t.Errorf("GOPROXY = %q, want %q", env[0], want)
	}
	if strings.Contains(env[0], "|") {
		t.Errorf("GOPROXY %q uses the |-separator; a relay outage would silently bypass to direct fetching", env[0])
	}
}

// TestCatalogEnv_NeverWeakensClientVerification guards the integrity
// prerequisite for the whole lane, across every entry at once: the relay
// is untrusted ONLY because each ecosystem's client verifies content
// itself, so no catalog env may ever disable that verification. For Go
// that is GOSUMDB/GONOSUMDB/GOFLAGS (e.g. -insecure) and the
// GOPRIVATE/GONOPROXY pair that exempts paths from checksum lookup. A
// routing change that quietly disabled verification would look exactly
// like routing config, which is why this is a test and not a review note.
func TestCatalogEnv_NeverWeakensClientVerification(t *testing.T) {
	banned := []string{"GOSUMDB", "GONOSUMDB", "GONOSUMCHECK", "GOFLAGS", "GOPRIVATE", "GONOPROXY", "GOINSECURE"}
	for _, e := range Catalog() {
		for _, kv := range e.Env("10.42.1.1:9001") {
			for _, b := range banned {
				if strings.HasPrefix(kv, b+"=") {
					t.Errorf("catalog entry %q emits %q; relay env must never weaken client-side verification", e.Config.Name, kv)
				}
			}
		}
	}
}
