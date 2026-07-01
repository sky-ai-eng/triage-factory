package ee

import (
	"testing"
	"time"

	"github.com/sky-ai-eng/triage-factory/ee/license"
	"github.com/sky-ai-eng/triage-factory/internal/entitlements"
)

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
