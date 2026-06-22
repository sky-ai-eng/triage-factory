package entitlements

import "testing"

func TestCommunityDefaultDeniesEverything(t *testing.T) {
	Reset()
	for _, f := range []Feature{FeatureSSO, FeatureSCIM, FeatureSandboxFleet, FeatureAuditExport} {
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
