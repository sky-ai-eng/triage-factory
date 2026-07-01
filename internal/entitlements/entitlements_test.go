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

func TestFor_PerOrgAutoAll(t *testing.T) {
	t.Cleanup(Reset)
	RegisterProvider(Static(FeatureGovernance))
	for _, org := range []string{"orgA", "orgB"} {
		if !For(org).Has(FeatureGovernance) {
			t.Fatalf("For(%q) should grant governance (auto-all Static provider)", org)
		}
	}
}

func TestEntitlementsNew_LimitAbsent(t *testing.T) {
	e := New([]Feature{FeatureGovernance}, nil)
	if _, ok := e.Limit("anyKey"); ok {
		t.Fatal("Limit should report (0, false) when no limits were set")
	}
}

func TestEntitlements_ZeroValueDeniesEverything(t *testing.T) {
	var e Entitlements
	if e.Has(FeatureSSO) || e.Has(FeatureGovernance) {
		t.Fatal("zero-value Entitlements must deny every feature")
	}
	if _, ok := e.Limit("anyKey"); ok {
		t.Fatal("zero-value Entitlements must report every limit as unset")
	}
}

func TestRegisterProviderNilIgnored(t *testing.T) {
	t.Cleanup(Reset)
	RegisterProvider(Static(FeatureGovernance))
	RegisterProvider(nil)
	if !For("any-org").Has(FeatureGovernance) {
		t.Fatal("nil RegisterProvider must not blank out the registered provider")
	}
}
