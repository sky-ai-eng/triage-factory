package main

import (
	"os"
	"strings"
	"testing"

	"github.com/sky-ai-eng/triage-factory/cmd/exec/agenthost"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// TestRegisteredFeaturesAreDeclared asserts that every feature referenced by
// a registration seam (event-source gates, agenthost extension namespaces) is
// a member of entitlements.AllFeatures().
//
// Why this must live at the composition root: package main is the sole
// importer of ee/, so this test process — and only this test process — sees
// exactly the real init()-time registrations. Seam tests elsewhere register
// synthetic features (Feature("test-...")) with t.Cleanup resets; those never
// appear here, which is why this is a test and not a registration-time panic
// (a membership panic would outlaw that synthetic-feature test pattern).
//
// Why membership matters: gating works either way (Has is a plain map
// lookup), but /api/entitlements answers by iterating AllFeatures() — a
// feature missing from it never reaches the frontend probe, so every
// render-gated surface stays dark even for licensed orgs.
func TestRegisteredFeaturesAreDeclared(t *testing.T) {
	declared := map[entitlements.Feature]bool{}
	for _, f := range entitlements.AllFeatures() {
		declared[f] = true
	}

	check := func(source string, features []entitlements.Feature) {
		for _, f := range features {
			if !declared[f] {
				t.Errorf("%s references feature %q, which is not in entitlements.AllFeatures() — add it to allFeatures (and mirror it in useEntitlements.ts) or the /api/entitlements probe will never surface it", source, f)
			}
		}
	}
	check("entitlements.GateEventSource", entitlements.RegisteredEventGateFeatures())
	check("agenthost.RegisterExtension", agenthost.RegisteredExtensionFeatures())
}

// TestFrontendMirrorsAllFeatures asserts that every feature in
// entitlements.AllFeatures() has its wire value declared in the frontend
// useEntitlements hook — the second hand-maintained mirror of allFeatures,
// and the likelier one to drift since no Go compiler sees it. The hook's
// convention is one `export const Feature<X> = '<id>' as const` per feature
// (frontend/src/hooks/useEntitlements.ts); this matches on the literal.
func TestFrontendMirrorsAllFeatures(t *testing.T) {
	src, err := os.ReadFile("frontend/src/hooks/useEntitlements.ts")
	if err != nil {
		t.Fatalf("read useEntitlements.ts: %v", err)
	}
	content := string(src)
	for _, f := range entitlements.AllFeatures() {
		needle := "'" + string(f) + "' as const"
		if !strings.Contains(content, needle) {
			t.Errorf("feature %q is not mirrored in useEntitlements.ts — expected a `export const Feature<X> = %s` declaration", f, needle)
		}
	}
}
