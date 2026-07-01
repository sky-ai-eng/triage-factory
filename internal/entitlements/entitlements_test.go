package entitlements

import "testing"

func TestCommunityDefaultDeniesEverything(t *testing.T) {
	Reset()
	// The Static (everything off) default denies every feature — the one real
	// one and any hypothetical future identifier alike (using literals so the
	// test doesn't depend on a constant existing yet).
	for _, f := range []Feature{FeatureSSO, "scim", "sandbox_fleet", "any_future_feature"} {
		if For("any-org").Has(f) {
			t.Fatalf("community build must not have %q", f)
		}
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

// TestEntitlementsNew_CopiesLimitsMap pins the immutable-snapshot contract:
// New must copy the caller's limits map rather than store it by reference, so
// a caller that mutates its own map after the call (or a Provider that reuses
// one map across multiple For(orgID) results) can never reach back into an
// already-issued Entitlements. Sharing the backing map would also make
// concurrent Limit() reads race with that mutation.
func TestEntitlementsNew_CopiesLimitsMap(t *testing.T) {
	src := map[Limit]int{"seats": 10}
	e := New([]Feature{FeatureGovernance}, src)
	src["seats"] = 999
	src["new_key"] = 1
	if v, ok := e.Limit("seats"); !ok || v != 10 {
		t.Fatalf("Limit(seats) = (%d, %v), want (10, true); mutating the caller's map leaked into the snapshot", v, ok)
	}
	if _, ok := e.Limit("new_key"); ok {
		t.Fatal("a key added to the caller's map after New must not appear in the snapshot")
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
