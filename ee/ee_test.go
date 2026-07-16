package ee

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/ee/license"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

// The production public key ships as publicKeyB64's source default, so a
// mangled edit (truncated paste, stray whitespace, wrong encoding) would
// silently produce a build that ignores every official license. loadPublicKey
// fails closed to nil on any parse problem, so "the default loads" is the
// whole guarantee.
func TestShippedPublicKeyDefaultLoads(t *testing.T) {
	if publicKeyB64 == "" {
		t.Fatal("publicKeyB64 source default is empty — official builds would ignore TF_LICENSE entirely")
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
