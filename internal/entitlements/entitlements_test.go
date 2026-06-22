package entitlements

import "testing"

func TestCommunityDefaultDeniesEverything(t *testing.T) {
	Reset()
	// The community checker denies every feature — the one real one and any
	// hypothetical future identifier alike (using literals so the test doesn't
	// depend on a constant existing yet).
	for _, f := range []Feature{FeatureSSO, "scim", "sandbox_fleet", "any_future_feature"} {
		if Active().Has(f) {
			t.Fatalf("community build must not have %q", f)
		}
	}
}

type allOn struct{}

func (allOn) Has(Feature) bool { return true }

func TestRegisterActivates(t *testing.T) {
	t.Cleanup(Reset)
	Register(allOn{})
	if !Active().Has(FeatureSSO) {
		t.Fatal("registered checker should grant SSO")
	}
}

func TestRegisterNilIgnored(t *testing.T) {
	t.Cleanup(Reset)
	Register(allOn{})
	Register(nil)
	if !Active().Has(FeatureSSO) {
		t.Fatal("nil register must not blank out the active checker")
	}
}
