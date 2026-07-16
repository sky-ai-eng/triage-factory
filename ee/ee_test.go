package ee

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/ee/license"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// The production public key ships as publicKeyB64's source default, so a bad
// edit would silently produce a build that ignores (or worse, mis-verifies)
// every official license. Pinning exact equality catches a *valid but wrong*
// key too — e.g. a dev key accidentally committed after local debugging.
// Rotating the prod key updates this pin, the source default, and
// scripts/verify-license-key.sh's EXPECTED in the same change.
func TestShippedPublicKeyDefaultLoads(t *testing.T) {
	const prodKey = "MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAExwX8JiPaEIRK4S1IxytI/FbY28LzBhg1F1q5uwLy47IosslwxxzsxUAFx0xpnGPqoGeadQr9Gw4Um2vuksHdhQ=="
	if publicKeyB64 != prodKey {
		t.Fatalf("publicKeyB64 source default is not the tf-license-prod public key — official builds would reject (wrong key) or ignore (empty/corrupt) every official license\n got: %q", publicKeyB64)
	}
	if loadPublicKey() == nil {
		t.Fatal("publicKeyB64 source default does not parse as a P-256 SPKI public key — official builds would ignore TF_LICENSE entirely")
	}
}

func TestLicenseProvider_ValidClaimsGrantEveryOrg(t *testing.T) {
	claims := license.Claims{
		Org:      "acme",
		Features: []string{string(entitlements.FeatureGovernance)},
		Expires:  time.Now().Add(time.Hour).Unix(),
	}
	p := licenseProvider{
		claims: claims,
		ent:    entitlements.New([]entitlements.Feature{entitlements.FeatureGovernance}, nil),
	}
	for _, org := range []string{"orgA", "orgB"} {
		if !p.For(org).Has(entitlements.FeatureGovernance) {
			t.Fatalf("For(%q) should grant governance under a valid, self-host auto-all license", org)
		}
	}
}

func TestLicenseProvider_ExpiredClaimsDenyLive(t *testing.T) {
	claims := license.Claims{
		Org:      "acme",
		Features: []string{string(entitlements.FeatureGovernance)},
		Expires:  time.Now().Add(-time.Hour).Unix(),
	}
	p := licenseProvider{
		claims: claims,
		ent:    entitlements.New([]entitlements.Feature{entitlements.FeatureGovernance}, nil),
	}
	if p.For("any-org").Has(entitlements.FeatureGovernance) {
		t.Fatal("an expired license must deny every feature, live, without re-registering")
	}
}
